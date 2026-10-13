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
	"bufio"
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"math/rand"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/uber/submitqueue/submitqueue/extension/buildrunner/replay"
	"github.com/uber/submitqueue/submitqueue/extension/speculation/scorer/jev"
	"github.com/uber/submitqueue/tool/jevcorpus/mutate"
)

// defaultVerify is the corpus's CI: the whole module builds and the touched
// packages' tests pass. $PKGS holds the touched packages relative to the
// module root. go vet is left out: the baseline does not check it, so a
// package with existing vet findings would fail every safe change.
const defaultVerify = "go build ./... && go test -count=1 $PKGS"

// triesPerSpec bounds how many changes are built and verified for one planned
// slot before the slot is given up; most retries are intent mismatches.
const triesPerSpec = 6

// verifyTailLines is how much failing output a manifest keeps for debugging.
const verifyTailLines = 15

// queuedAtStepMs spaces synthetic attempts a minute apart in the dataset.
const queuedAtStepMs = 60_000

type generateFlags struct {
	repo, base, moduleDir, pkgs, out, sandbox, work, verify string
	count                                                   int
	unsafeRatio                                             float64
	seed                                                    int64
	keepMismatch                                            bool
}

func runGenerate(args []string) error {
	var f generateFlags
	flags := flag.NewFlagSet("generate", flag.ExitOnError)
	flags.StringVar(&f.repo, "repo", "", "seed git repository: a path or clone URL (required)")
	flags.StringVar(&f.base, "base", "HEAD", "seed commit every change is built on")
	flags.StringVar(&f.moduleDir, "module-dir", ".", "Go module root inside the repository")
	flags.StringVar(&f.pkgs, "pkgs", "./...", "space-separated package patterns, relative to the module root, to mutate")
	flags.StringVar(&f.out, "out", "", "dataset to write; the manifest is written beside it (required)")
	flags.StringVar(&f.sandbox, "sandbox", "", "bare sandbox repository to create; must not exist (required)")
	flags.StringVar(&f.work, "work", "", "working clone to build in; defaults to a temporary directory")
	flags.StringVar(&f.verify, "verify", defaultVerify, "shell command run at the module root; exit 0 means the change lands")
	flags.IntVar(&f.count, "count", 40, "changes to generate")
	flags.Float64Var(&f.unsafeRatio, "unsafe-ratio", 0.5, "share of changes designed to fail")
	flags.Int64Var(&f.seed, "seed", 1, "random seed; one seed and base always plan the same corpus")
	flags.BoolVar(&f.keepMismatch, "keep-mismatch", false, "keep changes whose observed outcome contradicts their intent")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if f.repo == "" || f.out == "" || f.sandbox == "" {
		return errors.New("-repo, -out, and -sandbox are required")
	}
	if f.count < 1 || f.unsafeRatio < 0 || f.unsafeRatio > 1 {
		return errors.New("-count must be positive and -unsafe-ratio within [0, 1]")
	}
	sandbox, err := filepath.Abs(f.sandbox)
	if err != nil {
		return err
	}
	if _, err := os.Stat(sandbox); err == nil {
		return fmt.Errorf("sandbox %s already exists; remove it first", sandbox)
	}
	return generate(f, sandbox)
}

func generate(f generateFlags, sandbox string) error {
	work := f.work
	if work == "" {
		tmp, err := os.MkdirTemp("", "jevcorpus-work-")
		if err != nil {
			return err
		}
		defer os.RemoveAll(tmp)
		work = filepath.Join(tmp, "repo")
	}
	if _, err := os.Stat(work); err != nil {
		logf("cloning %s into %s", f.repo, work)
		if _, err := git("", "clone", "--quiet", f.repo, work); err != nil {
			return err
		}
	}
	base, err := git(work, "rev-parse", f.base+"^{commit}")
	if err != nil {
		return err
	}
	if err := resetWorkTree(work, base); err != nil {
		return err
	}
	moduleRoot := filepath.Join(work, f.moduleDir)

	tree, err := loadTree(work, moduleRoot)
	if err != nil {
		return err
	}
	logf("base %s: %d Go files; running baseline tests over %s", base[:12], tree.Len(), f.pkgs)
	cands, err := healthyCandidates(work, moduleRoot, f.pkgs, tree)
	if err != nil {
		return err
	}
	logf("%d candidate files in %d packages whose tests pass at the base", len(cands.files), len(cands.dirs))
	if len(cands.files) == 0 {
		return errors.New("no package under -pkgs has passing tests at the base")
	}

	r := rand.New(rand.NewSource(f.seed))
	specs := planSpecs(f.count, f.unsafeRatio, r)
	manifest := Manifest{Repo: f.repo, BaseSHA: base, Seed: f.seed, Verify: f.verify}
	ds := replay.Dataset{Repo: f.repo, BaseBranch: "main", BaseSHA: base}
	start := time.Now().UnixMilli()
	mismatches := 0

	for i, s := range specs {
		branch := fmt.Sprintf("corpus/c-%04d", i+1)
		var kept bool
		unbuildable := 0
		for try := 0; try < triesPerSpec && !kept; try++ {
			b, ok := buildChange(tree, cands, s, r)
			if !ok {
				unbuildable++
				continue
			}
			mc, title, err := commitChange(work, moduleRoot, base, branch, f.verify, tree, b, r)
			if err != nil {
				return fmt.Errorf("%s: %w", branch, err)
			}
			mc.Intent, mc.TargetTier = s.intent, s.tier
			if mismatch := (s.intent == mutate.IntentSafe) != mc.Lands; mismatch {
				mismatches++
				if !f.keepMismatch {
					logf("%s: %s %s change (%s) observed lands=%v; retrying", branch, s.intent, s.tier, strings.Join(b.mutators, ","), mc.Lands)
					continue
				}
			}
			kept = true
			manifest.Changes = append(manifest.Changes, mc)
			ds.Attempts = append(ds.Attempts, replay.Attempt{
				PR:         i + 1,
				Title:      title,
				GroupSHA:   mc.SHA,
				ParentSHA:  base,
				Outcome:    outcomeOf(mc.Lands),
				DurationMs: mc.VerifyMs,
				QueuedAtMs: start + int64(i)*queuedAtStepMs,
				ReplaySHA:  mc.SHA,
				Branch:     branch,
			})
			logf("%s: %s %s lands=%v (%d files, %d lines, %s)", branch, s.intent, mc.Tier, mc.Lands, mc.Files, mc.LinesChanged, mc.Category())
		}
		if !kept {
			logf("%s: gave up on a %s %s change after %d tries; %d found too few mutation sites in the candidate files",
				branch, s.intent, s.tier, triesPerSpec, unbuildable)
		}
	}
	if len(ds.Attempts) == 0 {
		return errors.New("no change was kept")
	}

	if err := createSandbox(work, sandbox, base, ds.Attempts); err != nil {
		return err
	}
	if err := writeDataset(f.out, ds); err != nil {
		return err
	}
	if err := writeJSON(manifestPathFor(f.out), manifest); err != nil {
		return err
	}
	logf("kept %d of %d planned changes; %d built changes contradicted their intent", len(ds.Attempts), len(specs), mismatches)
	printGrid(manifest)
	return nil
}

// commitChange writes b onto a clean base, verifies it, and commits it to
// branch whatever the outcome; the caller decides whether to keep it.
func commitChange(work, moduleRoot, base, branch, verify string, tree mutate.Tree, b built, r *rand.Rand) (ManifestChange, string, error) {
	if err := resetWorkTree(work, base); err != nil {
		return ManifestChange{}, "", err
	}
	for p, src := range b.patch {
		full := filepath.Join(work, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return ManifestChange{}, "", err
		}
		if err := os.WriteFile(full, []byte(src), 0o644); err != nil {
			return ManifestChange{}, "", err
		}
	}
	if _, err := git(work, "add", "-A"); err != nil {
		return ManifestChange{}, "", err
	}
	mc := ManifestChange{Branch: branch, Mutators: b.mutators}
	if err := measureStaged(work, &mc, b.needle); err != nil {
		return ManifestChange{}, "", err
	}

	pkgs := touchedPackages(work, moduleRoot, b.patch)
	began := time.Now()
	cmd := exec.Command("sh", "-c", verify)
	cmd.Dir = moduleRoot
	cmd.Env = append(os.Environ(), "PKGS="+strings.Join(pkgs, " "))
	out, verr := cmd.CombinedOutput()
	mc.VerifyMs = time.Since(began).Milliseconds()
	mc.Lands = verr == nil
	if !mc.Lands {
		var exitErr *exec.ExitError
		if !errors.As(verr, &exitErr) {
			return ManifestChange{}, "", fmt.Errorf("run verify: %w", verr)
		}
		mc.VerifyTail = tailLines(string(out), verifyTailLines)
	}

	title := commitSubject(b.patch, r)
	who := authors[r.Intn(len(authors))]
	commit := exec.Command("git", "-c", "commit.gpgsign=false", "commit", "--quiet", "--no-verify", "-m", title)
	commit.Dir = work
	commit.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME="+who.name, "GIT_AUTHOR_EMAIL="+who.email,
		"GIT_COMMITTER_NAME=jevcorpus", "GIT_COMMITTER_EMAIL=jevcorpus@submitqueue.invalid")
	if _, err := run(commit); err != nil {
		return ManifestChange{}, "", err
	}
	sha, err := git(work, "rev-parse", "HEAD")
	if err != nil {
		return ManifestChange{}, "", err
	}
	mc.SHA = sha
	if _, err := git(work, "branch", "--force", branch, sha); err != nil {
		return ManifestChange{}, "", err
	}
	return mc, title, nil
}

// measureStaged records the staged change's size and where its needle sits
// in the diff.
func measureStaged(work string, mc *ManifestChange, n needle) error {
	numstat, err := git(work, "diff", "--cached", "--numstat")
	if err != nil {
		return err
	}
	for _, line := range strings.Split(numstat, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		mc.Files++
		added, _ := strconv.Atoi(fields[0])
		deleted, _ := strconv.Atoi(fields[1])
		mc.LinesChanged += added + deleted
	}
	patch, err := gitRaw(work, "diff", "--cached")
	if err != nil {
		return err
	}
	mc.DiffBytes = len(patch)
	mc.Tier = tierFor(mc.LinesChanged, mc.DiffBytes, mc.Files)
	if n.mutator != "" {
		mc.Needle = ManifestNeedle{Mutator: n.mutator, File: n.file, Position: n.position}
		if at := strings.Index(patch, "diff --git a/"+n.file+" "); at >= 0 {
			mc.Needle.OffsetBytes = at
			mc.Needle.BeyondDiffBudget = at >= jev.DefaultMaxDiffBytes
		}
	}
	return nil
}

// touchedPackages returns the package directories p edits, relative to the
// module root in the "./dir" form go commands take.
func touchedPackages(work, moduleRoot string, p mutate.Patch) []string {
	seen := map[string]bool{}
	var out []string
	for _, file := range p.Paths() {
		rel, err := filepath.Rel(moduleRoot, filepath.Join(work, filepath.FromSlash(path.Dir(file))))
		if err != nil || strings.HasPrefix(rel, "..") {
			continue
		}
		pkg := "./" + filepath.ToSlash(rel)
		if !seen[pkg] {
			seen[pkg] = true
			out = append(out, pkg)
		}
	}
	return out
}

func resetWorkTree(work, base string) error {
	if _, err := git(work, "checkout", "--quiet", "--force", "--detach", base); err != nil {
		return err
	}
	_, err := git(work, "clean", "-fdq")
	return err
}

// loadTree reads every Go file under moduleRoot, keyed by its path relative
// to the repository root. Vendored code, testdata, and hidden or underscore
// directories are skipped, as the go command skips them.
func loadTree(work, moduleRoot string) (mutate.Tree, error) {
	files := map[string]string{}
	err := filepath.WalkDir(moduleRoot, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := d.Name()
		if d.IsDir() {
			if p != moduleRoot && (name == "vendor" || name == "testdata" || strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_")) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(name, ".go") {
			return nil
		}
		src, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(work, p)
		if err != nil {
			return err
		}
		files[filepath.ToSlash(rel)] = string(src)
		return nil
	})
	return mutate.NewTree(files), err
}

// healthyCandidates runs the base's tests once and keeps the packages that
// pass, so every failure in the corpus is one the mutation caused.
func healthyCandidates(work, moduleRoot, pattern string, tree mutate.Tree) (candidates, error) {
	if _, err := goCmd(moduleRoot, "build", "./..."); err != nil {
		return candidates{}, fmt.Errorf("the base does not build: %w", err)
	}
	patterns := strings.Fields(pattern)
	listing, err := goCmd(moduleRoot, append([]string{"list", "-f", "{{.ImportPath}}\t{{.Dir}}"}, patterns...)...)
	if err != nil {
		return candidates{}, err
	}
	dirOf := map[string]string{}
	for _, line := range strings.Split(listing, "\n") {
		if imp, dir, ok := strings.Cut(line, "\t"); ok {
			dirOf[imp] = dir
		}
	}
	// A failing package exits non-zero but its "ok" siblings are still
	// reported, which is all this reads.
	out, _ := goCmd(moduleRoot, append([]string{"test", "-count=1"}, patterns...)...)
	var c candidates
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 2 || fields[0] != "ok" {
			continue
		}
		dir, ok := dirOf[fields[1]]
		if !ok {
			continue
		}
		rel, err := filepath.Rel(work, dir)
		if err != nil {
			continue
		}
		rel = filepath.ToSlash(rel)
		var files []string
		for _, p := range tree.GoFiles(rel) {
			if src, _ := tree.File(p); !mutate.IsTestFile(p) && !isGenerated(src) {
				files = append(files, p)
			}
		}
		if len(files) > 0 {
			c.dirs = append(c.dirs, rel)
			c.files = append(c.files, files...)
		}
	}
	sort.Strings(c.dirs)
	sort.Strings(c.files)
	return c, nil
}

// isGenerated follows the Go convention for generated files: a "Code
// generated ... DO NOT EDIT." line before the package clause.
func isGenerated(src string) bool {
	head, _, _ := strings.Cut(src, "\npackage ")
	return strings.Contains(head, "Code generated") && strings.Contains(head, "DO NOT EDIT")
}

func createSandbox(work, sandbox, base string, attempts []replay.Attempt) error {
	if err := os.MkdirAll(filepath.Dir(sandbox), 0o755); err != nil {
		return err
	}
	if _, err := git("", "init", "--bare", "--quiet", "-b", "main", sandbox); err != nil {
		return err
	}
	refspecs := []string{base + ":refs/heads/main"}
	for _, a := range attempts {
		refspecs = append(refspecs, a.ReplaySHA+":refs/heads/"+a.Branch)
	}
	_, err := git(work, append([]string{"push", "--quiet", sandbox}, refspecs...)...)
	return err
}

func printGrid(m Manifest) {
	type cell struct{ lands, fails int }
	grid := map[Tier]*cell{}
	for _, t := range tiers {
		grid[t] = &cell{}
	}
	for _, c := range m.Changes {
		if c.Lands {
			grid[c.Tier].lands++
		} else {
			grid[c.Tier].fails++
		}
	}
	fmt.Println("tier  lands  fails")
	for _, t := range tiers {
		fmt.Printf("%-4s  %5d  %5d\n", t, grid[t].lands, grid[t].fails)
	}
}

func outcomeOf(lands bool) replay.Outcome {
	if lands {
		return replay.OutcomePass
	}
	return replay.OutcomeFail
}

func tailLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

func logf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
}

func git(dir string, args ...string) (string, error) {
	out, err := gitRaw(dir, args...)
	return strings.TrimSpace(out), err
}

func gitRaw(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	return runRaw(cmd)
}

func goCmd(dir string, args ...string) (string, error) {
	cmd := exec.Command("go", args...)
	cmd.Dir = dir
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	if err != nil {
		return out.String(), fmt.Errorf("go %s: %w: %s", strings.Join(args, " "), err, tailLines(out.String(), verifyTailLines))
	}
	return out.String(), nil
}

func run(cmd *exec.Cmd) (string, error) {
	out, err := runRaw(cmd)
	return strings.TrimSpace(out), err
}

func runRaw(cmd *exec.Cmd) (string, error) {
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("%s: %w: %s", strings.Join(cmd.Args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return string(out), nil
}
