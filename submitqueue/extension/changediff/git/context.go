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

package git

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/uber/submitqueue/submitqueue/entity"
)

// Bounds on how much context one change can pull out of the repository. They
// keep a sweeping change from turning into thousands of git invocations; the
// consumer applies its own, tighter, byte budget on top.
const (
	maxFileBytes          = 256 << 10
	maxSymbolsPerKind     = 20
	maxCallersPerSymbol   = 10
	maxDefinitionsPerName = 3
)

// Symbol extraction is a line-oriented heuristic over the unified diff, not a
// parser: it recognizes Go declarations first, with Python and JavaScript
// forms as a best effort. A missed symbol costs context, never correctness.
var (
	definitionPatterns = []*regexp.Regexp{
		regexp.MustCompile(`^\s*func\s+(?:\([^)]*\)\s*)?([A-Za-z_]\w*)\s*[\[(]`),
		regexp.MustCompile(`^\s*type\s+([A-Za-z_]\w*)\b`),
		regexp.MustCompile(`^\s*(?:var|const)\s+([A-Za-z_]\w*)\b`),
		regexp.MustCompile(`^\s*(?:def|class)\s+([A-Za-z_]\w*)\b`),
		regexp.MustCompile(`^\s*(?:export\s+)?function\s+([A-Za-z_]\w*)\s*\(`),
	}
	// A bare call: an identifier followed by "(", not preceded by "." (a
	// method or package-qualified call, whose definition cannot be found by
	// name alone) or by another identifier character.
	callPattern = regexp.MustCompile(`(?:^|[^.\w])([A-Za-z_]\w*)\s*\(`)
)

// notSymbols are language keywords and builtins that look like calls.
var notSymbols = map[string]bool{}

func init() {
	for _, w := range strings.Fields(`if for switch return func go defer select case range else
		make new len cap append copy delete panic recover print println close complex real imag
		string int int8 int16 int32 int64 uint uint8 uint16 uint32 uint64 uintptr float32 float64
		byte rune bool error any min max clear struct interface map chan
		def class print isinstance super self function typeof`) {
		notSymbols[w] = true
	}
}

// changedLines returns the removed and added code lines of a unified diff,
// without their +/- markers and without the file headers.
func changedLines(patch string) (removed, added []string) {
	for _, line := range strings.Split(patch, "\n") {
		switch {
		case strings.HasPrefix(line, "---"), strings.HasPrefix(line, "+++"):
		case strings.HasPrefix(line, "-"):
			removed = append(removed, line[1:])
		case strings.HasPrefix(line, "+"):
			added = append(added, line[1:])
		}
	}
	return removed, added
}

// declaration is a symbol and the line that declares it.
type declaration struct {
	name string
	line string
}

func declaredSymbols(lines []string) []declaration {
	seen := map[string]bool{}
	var decls []declaration
	for _, line := range lines {
		for _, re := range definitionPatterns {
			if m := re.FindStringSubmatch(line); m != nil && !seen[m[1]] {
				seen[m[1]] = true
				decls = append(decls, declaration{name: m[1], line: strings.TrimSpace(line)})
			}
		}
	}
	return decls
}

func calledSymbols(lines []string, defined map[string]bool) []string {
	seen := map[string]bool{}
	var names []string
	for _, line := range lines {
		if isDefinition(line) {
			continue
		}
		for _, m := range callPattern.FindAllStringSubmatch(line, -1) {
			name := m[1]
			if notSymbols[name] || defined[name] || seen[name] {
				continue
			}
			seen[name] = true
			names = append(names, name)
		}
	}
	return names
}

func isDefinition(line string) bool {
	for _, re := range definitionPatterns {
		if re.MatchString(line) {
			return true
		}
	}
	return false
}

// touchedFile is one entry of `git diff --name-status`.
type touchedFile struct {
	path    string
	added   bool
	deleted bool
}

// touchedFiles parses `git diff --name-status -M -z`: a status field, then one
// path, or two (old, new) for a rename or copy.
func touchedFiles(out string) []touchedFile {
	fields := strings.Split(out, "\x00")
	var files []touchedFile
	for i := 0; i < len(fields); i++ {
		status := fields[i]
		if status == "" {
			continue
		}
		if (status[0] == 'R' || status[0] == 'C') && i+2 < len(fields) {
			files = append(files, touchedFile{path: fields[i+2]})
			i += 2
			continue
		}
		if i+1 >= len(fields) {
			break
		}
		files = append(files, touchedFile{path: fields[i+1], added: status[0] == 'A', deleted: status[0] == 'D'})
		i++
	}
	return files
}

// changeContext reads the files and references for one change at sha.
func (d *differ) changeContext(ctx context.Context, base, sha, patch string) ([]entity.FileContent, []entity.CodeReference, error) {
	if !d.includeFiles && !d.includeReferences {
		return nil, nil, nil
	}
	nameStatus, err := d.repo.RunRaw(ctx, "diff", "--name-status", "-M", "-z", base, sha)
	if err != nil {
		return nil, nil, err
	}
	touched := touchedFiles(nameStatus)
	isTouched := make(map[string]bool, len(touched))
	for _, f := range touched {
		isTouched[f.path] = true
	}

	var files []entity.FileContent
	if d.includeFiles {
		for _, f := range touched {
			// An added file is already whole in the diff.
			if f.added || f.deleted {
				continue
			}
			content, err := d.repo.RunRaw(ctx, "show", sha+":"+f.path)
			if err != nil {
				return nil, nil, err
			}
			if len(content) > maxFileBytes || strings.ContainsRune(content, 0) {
				continue
			}
			files = append(files, entity.FileContent{Path: f.path, Content: content})
		}
		sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	}

	var refs []entity.CodeReference
	if d.includeReferences {
		refs, err = d.references(ctx, sha, patch, isTouched)
		if err != nil {
			return nil, nil, err
		}
	}
	return files, refs, nil
}

// references follows Go's scoping to keep same-named symbols in unrelated
// packages out: a bare call resolves only within the caller's package
// directory, and a use from another package is qualified ("pkg.Name"). Other
// languages get the same rule, which errs toward reporting too little.
func (d *differ) references(ctx context.Context, sha, patch string, isTouched map[string]bool) ([]entity.CodeReference, error) {
	removed, added := changedLines(patch)
	touchedDirs := make(map[string]bool, len(isTouched))
	for p := range isTouched {
		touchedDirs[path.Dir(p)] = true
	}

	// A symbol defined on a removed line was deleted, renamed, or had its
	// declaration rewritten; any untouched use of it may no longer compile.
	after := map[string]string{}
	definedHere := map[string]bool{}
	for _, decl := range declaredSymbols(added) {
		after[decl.name] = decl.line
		definedHere[decl.name] = true
	}

	var refs []entity.CodeReference
	for _, decl := range capped(declaredSymbols(removed), maxSymbolsPerKind) {
		name := decl.name
		hits, err := d.grep(ctx, sha, "-w", "-e", name)
		if err != nil {
			return nil, err
		}
		n := 0
		for _, h := range hits {
			if isTouched[h.Path] || n == maxCallersPerSymbol {
				continue
			}
			if !touchedDirs[path.Dir(h.Path)] && !strings.Contains(h.Text, "."+name) {
				continue
			}
			h.Kind, h.Symbol = entity.CodeReferenceKindCaller, name
			h.DeclarationBefore, h.DeclarationAfter = decl.line, after[name]
			refs = append(refs, h)
			n++
		}
	}

	for _, name := range capped(calledSymbols(added, definedHere), maxSymbolsPerKind) {
		quoted := regexp.QuoteMeta(name)
		hits, err := d.grep(ctx, sha, "-E",
			"-e", `^func (\([^)]*\) )?`+quoted+`[[(]`,
			"-e", `^(type|var|const) `+quoted+`([^A-Za-z0-9_]|$)`,
			"-e", `^[[:space:]]*(def|class) `+quoted+`([^A-Za-z0-9_]|$)`,
			"-e", `^(export )?function `+quoted+`[[:space:]]*\(`,
		)
		if err != nil {
			return nil, err
		}
		n := 0
		for _, h := range hits {
			if !touchedDirs[path.Dir(h.Path)] || n == maxDefinitionsPerName {
				continue
			}
			h.Kind, h.Symbol = entity.CodeReferenceKindDefinition, name
			refs = append(refs, h)
			n++
		}
	}
	return refs, nil
}

// grep runs `git grep` over the tree at sha. No match is an empty result, not
// an error: git reports it with exit status 1.
func (d *differ) grep(ctx context.Context, sha string, args ...string) ([]entity.CodeReference, error) {
	full := append([]string{"grep", "-n", "-I", "--null"}, args...)
	full = append(full, sha, "--")
	out, err := d.repo.RunRaw(ctx, full...)
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to search %s: %w", sha, err)
	}
	var hits []entity.CodeReference
	for _, line := range strings.Split(out, "\n") {
		// "<sha>:<path>\x00<line>\x00<text>"
		parts := strings.SplitN(line, "\x00", 3)
		if len(parts) != 3 {
			continue
		}
		path := strings.TrimPrefix(parts[0], sha+":")
		n, err := strconv.Atoi(parts[1])
		if err != nil {
			continue
		}
		hits = append(hits, entity.CodeReference{Path: path, Line: n, Text: parts[2]})
	}
	return hits, nil
}

func capped[T any](xs []T, n int) []T {
	if len(xs) > n {
		return xs[:n]
	}
	return xs
}
