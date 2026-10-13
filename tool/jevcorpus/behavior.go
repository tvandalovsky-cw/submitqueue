// Copyright (c) 2026 Uber Technologies, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/uber/submitqueue/tool/jevcorpus/mutate"
)

// A behavior pair conflicts at run time, where no compiler looks: A changes
// what a function computes in a way the existing tests do not notice, and B,
// written against the old behavior, adds a table test that pins it. The
// compatible twin is the same A against a table test of the same function
// whose inputs A's change does not affect, so telling the two apart means
// reasoning about what the function returns.
const (
	behaviorConflict   = "behavior-vs-new-test"
	behaviorCompatible = "behavior-vs-unaffected-test"
)

// probeCaseCount bounds the inputs probed per function; probeTargetTries the
// functions tried per planned pair.
const (
	probeCaseCount   = 24
	probeTargetTries = 12
)

// probeValues are the literal inputs probed for each supported type.
var probeValues = map[string][]string{
	"int":      {"-1", "0", "1", "2", "3", "5", "10", "100"},
	"int64":    {"-1", "0", "1", "2", "3", "10", "1000"},
	"uint":     {"0", "1", "2", "3", "10"},
	"string":   {`""`, `"a"`, `"abc"`, `"a,b"`, `" x "`, `"hello world"`, `"A"`, `"1"`, `"a/b"`},
	"bool":     {"true", "false"},
	"float64":  {"0", "0.5", "1.5", "-2"},
	"[]int":    {"nil", "[]int{}", "[]int{1}", "[]int{1, 2, 3}", "[]int{3, 1, 2}"},
	"[]string": {"nil", `[]string{"a"}`, `[]string{"a", "b"}`, `[]string{"b", "a", "b"}`},
	"[]byte":   {"nil", `[]byte("a")`, `[]byte("abc")`},
}

// probeSeparator splits one case's rendered results. A unit separator cannot
// appear in a %#v rendering, which escapes control characters.
const probeSeparator = "\x1f"

const (
	probeMarker   = "JEVCORPUS_PROBE"
	probePanic    = "PANIC"
	probeUnstable = "UNSTABLE"
	probeTestName = "TestJevcorpusProbe"
)

// behaviorCandidate is a probed function with a mutant that changes some of
// its outputs.
type behaviorCandidate struct {
	target  mutate.ProbeTarget
	cases   [][]string
	base    []string
	mutant  mutate.NamedPatch
	killing []int
	keeping []int
}

// findBehaviorCandidate probes functions in random candidate packages until one
// has a mutant that changes at least one stable output and leaves at least two
// unchanged.
func findBehaviorCandidate(work, moduleRoot, base string, tree mutate.Tree, c candidates, r *rand.Rand) (behaviorCandidate, bool, error) {
	var targets []mutate.ProbeTarget
	for _, i := range r.Perm(len(c.dirs)) {
		targets = append(targets, mutate.ProbeTargets(tree, c.dirs[i])...)
		if len(targets) >= probeTargetTries {
			break
		}
	}
	r.Shuffle(len(targets), func(i, j int) { targets[i], targets[j] = targets[j], targets[i] })
	for _, pt := range targets[:min(len(targets), probeTargetTries)] {
		cases := probeCases(pt, r)
		baseOut, err := runProbe(work, moduleRoot, base, nil, pt, cases)
		if err != nil {
			return behaviorCandidate{}, false, err
		}
		mutants := mutate.BehaviorMutants(tree, pt.File, pt.Func, r)
		for _, m := range mutants[:min(3, len(mutants))] {
			// Most mutants are caught by the package's own tests, which would
			// make A fail alone; the package's tests are far cheaper than the
			// full verification that would otherwise find out.
			if err := stagePatch(work, base, m.Patch); err != nil {
				return behaviorCandidate{}, false, err
			}
			survives, err := runVerify(work, moduleRoot, "go test -count=1 $PKGS", m.Patch)
			if err != nil {
				return behaviorCandidate{}, false, err
			}
			if !survives.lands {
				continue
			}
			out, err := runProbe(work, moduleRoot, base, m.Patch, pt, cases)
			if err != nil {
				return behaviorCandidate{}, false, err
			}
			cand := behaviorCandidate{target: pt, cases: cases, base: baseOut, mutant: m}
			for i := range cases {
				if i >= len(baseOut) || i >= len(out) || !stableOutput(baseOut[i]) || !stableOutput(out[i]) {
					continue
				}
				if baseOut[i] != out[i] {
					cand.killing = append(cand.killing, i)
				} else {
					cand.keeping = append(cand.keeping, i)
				}
			}
			if len(cand.killing) > 0 && len(cand.keeping) >= 2 {
				return cand, true, nil
			}
		}
	}
	return behaviorCandidate{}, false, nil
}

func stableOutput(s string) bool { return s != "" && s != probePanic && s != probeUnstable }

// probeCases returns up to probeCaseCount distinct argument lists for pt.
func probeCases(pt mutate.ProbeTarget, r *rand.Rand) [][]string {
	seen := map[string]bool{}
	var out [][]string
	for try := 0; try < probeCaseCount*4 && len(out) < probeCaseCount; try++ {
		args := make([]string, len(pt.Params))
		for i, typ := range pt.Params {
			vals := probeValues[typ]
			args[i] = vals[r.Intn(len(vals))]
		}
		key := strings.Join(args, "\x00")
		if !seen[key] {
			seen[key] = true
			out = append(out, args)
		}
	}
	return out
}

// runProbe stages base with p, adds a throwaway test that calls pt on every
// case twice, and returns each case's rendered results — or PANIC, or
// UNSTABLE when the two calls disagree.
func runProbe(work, moduleRoot, base string, p mutate.Patch, pt mutate.ProbeTarget, cases [][]string) ([]string, error) {
	if err := stagePatch(work, base, p); err != nil {
		return nil, err
	}
	probePath := filepath.Join(work, filepath.FromSlash(path.Dir(pt.File)), "zz_jevcorpus_probe_test.go")
	if err := os.WriteFile(probePath, []byte(probeSource(pt, cases)), 0o644); err != nil {
		return nil, err
	}
	defer os.Remove(probePath)
	pkg, err := filepath.Rel(moduleRoot, filepath.Dir(probePath))
	if err != nil {
		return nil, err
	}
	cmd := exec.Command("go", "test", "-count=1", "-v", "-run", "^"+probeTestName+"$", "./"+filepath.ToSlash(pkg))
	cmd.Dir = moduleRoot
	raw, _ := cmd.CombinedOutput()
	out := make([]string, len(cases))
	for _, line := range strings.Split(string(raw), "\n") {
		rest, ok := strings.CutPrefix(line, probeMarker+" ")
		if !ok {
			continue
		}
		idx, val, ok := strings.Cut(rest, " ")
		if i, err := strconv.Atoi(idx); ok && err == nil && i < len(out) {
			out[i] = val
		}
	}
	return out, nil
}

func probeSource(pt mutate.ProbeTarget, cases [][]string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "package %s\n\nimport (\n\t\"fmt\"\n\t\"testing\"\n)\n\n", pt.Package)
	fmt.Fprintf(&b, "func %s(t *testing.T) {\n\tcases := []func() string{\n", probeTestName)
	results := resultNames(pt)
	for _, args := range cases {
		b.WriteString("\t\tfunc() (out string) {\n\t\t\tdefer func() {\n\t\t\t\tif recover() != nil {\n")
		fmt.Fprintf(&b, "\t\t\t\t\tout = %q\n\t\t\t\t}\n\t\t\t}()\n", probePanic)
		fmt.Fprintf(&b, "\t\t\t%s := %s(%s)\n", strings.Join(results, ", "), pt.Func, strings.Join(args, ", "))
		formats := make([]string, 0, len(pt.Results)+1)
		for range pt.Results {
			formats = append(formats, "%#v")
		}
		if pt.ReturnsError {
			formats = append(formats, "err=%v")
		}
		b.WriteString("\t\t\treturn fmt.Sprintf(" + strconv.Quote(strings.Join(formats, probeSeparator)))
		for _, name := range results {
			if name == "err" {
				b.WriteString(", err != nil")
			} else {
				b.WriteString(", " + name)
			}
		}
		b.WriteString(")\n\t\t},\n")
	}
	fmt.Fprintf(&b, "\t}\n\tfor i, c := range cases {\n\t\tfirst, second := c(), c()\n\t\tif first != second {\n\t\t\tfirst = %q\n\t\t}\n", probeUnstable)
	fmt.Fprintf(&b, "\t\tfmt.Printf(\"%s %%d %%s\\n\", i, first)\n\t}\n}\n", probeMarker)
	return b.String()
}

func resultNames(pt mutate.ProbeTarget) []string {
	var names []string
	for i := range pt.Results {
		names = append(names, "got"+strconv.Itoa(i))
	}
	if pt.ReturnsError {
		names = append(names, "err")
	}
	return names
}

// behaviorTestPatch writes B: a table test of cand's function over the chosen
// cases, expecting the outputs the base produced.
func behaviorTestPatch(tree mutate.Tree, cand behaviorCandidate, chosen []int) (mutate.Patch, bool) {
	pt := cand.target
	dir := path.Dir(pt.File)
	stem := strings.ToLower(strings.Join(splitIdent(pt.Func), "_"))
	file := path.Join(dir, stem+"_cases_test.go")
	if _, exists := tree.File(file); exists {
		return nil, false
	}
	testName := "Test" + upperFirst(pt.Func) + "Cases"
	for _, p := range tree.GoFiles(dir) {
		if src, _ := tree.File(p); strings.Contains(src, "func "+testName+"(") {
			return nil, false
		}
	}
	caseField := "name"
	for _, n := range pt.Names {
		if n == "name" {
			caseField = "caseName"
		}
	}
	var b strings.Builder
	imports := "\t\"testing\"\n"
	if len(pt.Results) > 0 {
		imports = "\t\"reflect\"\n" + imports
	}
	fmt.Fprintf(&b, "package %s\n\nimport (\n%s)\n\n", pt.Package, imports)
	fmt.Fprintf(&b, "func %s(t *testing.T) {\n\ttests := []struct {\n\t\t%s string\n", testName, caseField)
	for i, typ := range pt.Params {
		fmt.Fprintf(&b, "\t\t%s %s\n", pt.Names[i], typ)
	}
	for i, typ := range pt.Results {
		fmt.Fprintf(&b, "\t\twant%d %s\n", i, typ)
	}
	if pt.ReturnsError {
		b.WriteString("\t\twantErr bool\n")
	}
	b.WriteString("\t}{\n")
	for _, ci := range chosen {
		args := cand.cases[ci]
		wants := strings.Split(cand.base[ci], probeSeparator)
		var parts []string
		var label []string
		for i, a := range args {
			parts = append(parts, pt.Names[i]+": "+a)
			label = append(label, pt.Names[i]+"="+a)
		}
		for i := range pt.Results {
			parts = append(parts, fmt.Sprintf("want%d: %s", i, wants[i]))
		}
		if pt.ReturnsError {
			parts = append(parts, "wantErr: "+strings.TrimPrefix(wants[len(wants)-1], "err="))
		}
		fmt.Fprintf(&b, "\t\t{%s: %q, %s},\n", caseField, strings.Join(label, " "), strings.Join(parts, ", "))
	}
	b.WriteString("\t}\n\tfor _, tt := range tests {\n")
	fmt.Fprintf(&b, "\t\tt.Run(tt.%s, func(t *testing.T) {\n", caseField)
	var args []string
	for _, n := range pt.Names {
		args = append(args, "tt."+n)
	}
	results := resultNames(pt)
	fmt.Fprintf(&b, "\t\t\t%s := %s(%s)\n", strings.Join(results, ", "), pt.Func, strings.Join(args, ", "))
	if pt.ReturnsError {
		b.WriteString("\t\t\tif (err != nil) != tt.wantErr {\n\t\t\t\tt.Fatalf(\"error = %v, wantErr %v\", err, tt.wantErr)\n\t\t\t}\n")
	}
	for i := range pt.Results {
		fmt.Fprintf(&b, "\t\t\tif !reflect.DeepEqual(got%d, tt.want%d) {\n\t\t\t\tt.Errorf(\"got %%#v, want %%#v\", got%d, tt.want%d)\n\t\t\t}\n", i, i, i, i)
	}
	b.WriteString("\t\t})\n\t}\n}\n")
	return mutate.Patch{file: b.String()}, true
}

// chooseBehaviorCases picks a conflicting test's cases — at least one input the
// mutant changes among unchanged ones — or a compatible test's, all unchanged.
func chooseBehaviorCases(cand behaviorCandidate, conflict bool, r *rand.Rand) []int {
	keep := append([]int(nil), cand.keeping...)
	r.Shuffle(len(keep), func(i, j int) { keep[i], keep[j] = keep[j], keep[i] })
	if !conflict {
		return keep[:min(len(keep), 2+r.Intn(3))]
	}
	kill := append([]int(nil), cand.killing...)
	r.Shuffle(len(kill), func(i, j int) { kill[i], kill[j] = kill[j], kill[i] })
	chosen := append(kill[:1+r.Intn(min(2, len(kill)))], keep[:min(len(keep), 1+r.Intn(3))]...)
	r.Shuffle(len(chosen), func(i, j int) { chosen[i], chosen[j] = chosen[j], chosen[i] })
	return chosen
}

func splitIdent(name string) []string {
	var words []string
	start := 0
	for i := 1; i < len(name); i++ {
		if name[i] >= 'A' && name[i] <= 'Z' && name[i-1] >= 'a' && name[i-1] <= 'z' {
			words = append(words, name[start:i])
			start = i
		}
	}
	return append(words, name[start:])
}

func upperFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
