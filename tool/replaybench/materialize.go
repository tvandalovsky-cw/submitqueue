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
	"bytes"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	changegit "github.com/uber/submitqueue/platform/base/change/git"
	"github.com/uber/submitqueue/submitqueue/extension/buildrunner/replay"
)

// Every replayed change is rebuilt as one commit on the window's base: the
// upstream PR's own effect (its merge-group commit minus that commit's parent),
// three-way merged onto the base. Rebuilding on one base, rather than keeping
// upstream's queue topology, is what lets the change provider measure each
// change against the sandbox's own main: upstream group commits descend from
// queue heads the sandbox never has.
func runMaterialize(args []string) error {
	fs := flag.NewFlagSet("materialize", flag.ExitOnError)
	dsPath := fs.String("dataset", "", "dataset written by fetch; rewritten in place (required)")
	mirror := fs.String("mirror", "", "bare clone of the upstream repository, created if missing (required)")
	sandbox := fs.String("sandbox", "", "bare sandbox repository to create (required); must not exist")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *dsPath == "" || *mirror == "" || *sandbox == "" {
		return errors.New("-dataset, -mirror, and -sandbox are required")
	}
	// The push runs inside the mirror, so a relative sandbox path would
	// resolve against the wrong directory.
	sandboxPath, err := filepath.Abs(*sandbox)
	if err != nil {
		return err
	}
	*sandbox = sandboxPath
	ds, err := replay.Load(*dsPath)
	if err != nil {
		return err
	}
	if len(ds.Attempts) == 0 {
		return errors.New("dataset has no attempts")
	}
	if _, err := os.Stat(*sandbox); err == nil {
		return fmt.Errorf("sandbox %s already exists; remove it first", *sandbox)
	}

	if _, err := os.Stat(*mirror); err != nil {
		fmt.Printf("cloning %s into %s\n", ds.Repo, *mirror)
		if _, err := git("", "clone", "--bare", "--quiet", "https://github.com/"+ds.Repo+".git", *mirror); err != nil {
			return err
		}
	}
	if err := fetchMissing(*mirror, ds); err != nil {
		return err
	}

	ds.BaseSHA = ds.Attempts[0].ParentSHA
	baseTree, err := git(*mirror, "rev-parse", ds.BaseSHA+"^{tree}")
	if err != nil {
		return err
	}
	if err := markFlakySuspects(*mirror, ds.Attempts); err != nil {
		return err
	}
	kept := 0
	for i := range ds.Attempts {
		a := &ds.Attempts[i]
		a.ReplaySHA, a.Branch, a.Skipped = "", "", ""
		if err := rebuildOnBase(*mirror, ds.BaseSHA, baseTree, a); err != nil {
			return fmt.Errorf("PR %d: %w", a.PR, err)
		}
		if a.Skipped == "" {
			kept++
		}
	}

	refspecs := []string{ds.BaseSHA + ":refs/heads/" + ds.BaseBranch}
	for _, a := range ds.Attempts {
		if a.ReplaySHA != "" {
			refspecs = append(refspecs, a.ReplaySHA+":refs/heads/"+a.Branch)
		}
	}
	if err := os.MkdirAll(filepath.Dir(*sandbox), 0o755); err != nil {
		return err
	}
	if _, err := git("", "init", "--bare", "--quiet", "-b", ds.BaseBranch, *sandbox); err != nil {
		return err
	}
	if _, err := git(*mirror, append([]string{"push", "--quiet", *sandbox}, refspecs...)...); err != nil {
		return err
	}
	fmt.Printf("sandbox %s: base %s, %d of %d attempts materialized\n", *sandbox, ds.BaseSHA[:12], kept, len(ds.Attempts))
	return writeDataset(*dsPath, ds)
}

// fetchMissing fetches group and parent commits the mirror lacks. GitHub
// serves commits by SHA even after the merge-group branch is deleted, which
// is what makes failed attempts — never on any branch — recoverable.
func fetchMissing(mirror string, ds replay.Dataset) error {
	var missing []string
	seen := map[string]bool{}
	for _, a := range ds.Attempts {
		for _, sha := range []string{a.GroupSHA, a.ParentSHA} {
			if seen[sha] {
				continue
			}
			seen[sha] = true
			if _, err := git(mirror, "cat-file", "-e", sha+"^{commit}"); err != nil {
				missing = append(missing, sha)
			}
		}
	}
	if len(missing) == 0 {
		return nil
	}
	fmt.Printf("fetching %d commits not on any branch\n", len(missing))
	_, err := git(mirror, append([]string{"fetch", "--quiet", "origin"}, missing...)...)
	return err
}

func rebuildOnBase(mirror, base, baseTree string, a *replay.Attempt) error {
	tree, err := git(mirror, "merge-tree", "--write-tree", "--merge-base="+a.ParentSHA, base, a.GroupSHA)
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
			a.Skipped = "conflicts with the window base"
			return nil
		}
		return err
	}
	tree = strings.SplitN(tree, "\n", 2)[0]
	if tree == baseTree {
		a.Skipped = "no change relative to the window base"
		return nil
	}
	meta, err := git(mirror, "log", "-1", "--format=%an%x00%ae%x00%aI%x00%s", a.GroupSHA)
	if err != nil {
		return err
	}
	f := strings.SplitN(meta, "\x00", 4)
	if len(f) != 4 {
		return fmt.Errorf("unreadable metadata for %s", a.GroupSHA)
	}
	a.Title = f[3]
	cmd := exec.Command("git", "commit-tree", tree, "-p", base, "-m", a.Title)
	cmd.Dir = mirror
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME="+f[0], "GIT_AUTHOR_EMAIL="+f[1], "GIT_AUTHOR_DATE="+f[2],
		"GIT_COMMITTER_NAME=replaybench", "GIT_COMMITTER_EMAIL=replaybench@submitqueue.invalid", "GIT_COMMITTER_DATE="+f[2])
	out, err := run(cmd)
	if err != nil {
		return err
	}
	a.ReplaySHA = out
	a.Branch = fmt.Sprintf("replay/pr-%d/%s", a.PR, a.GroupSHA[:8])
	return nil
}

// markFlakySuspects records each attempt's patch ID and flags failures whose
// exact change later passed for the same PR.
func markFlakySuspects(mirror string, attempts []replay.Attempt) error {
	for i := range attempts {
		a := &attempts[i]
		cmd := exec.Command("sh", "-c", `git diff "$1" "$2" | git patch-id --stable`, "sh", a.ParentSHA, a.GroupSHA)
		cmd.Dir = mirror
		out, err := run(cmd)
		if err != nil {
			return fmt.Errorf("PR %d: patch id: %w", a.PR, err)
		}
		a.PatchID, _, _ = strings.Cut(out, " ")
	}
	for i := range attempts {
		a := &attempts[i]
		a.FlakySuspect = false
		if a.Outcome != replay.OutcomeFail || a.PatchID == "" {
			continue
		}
		for _, later := range attempts[i+1:] {
			if later.PR == a.PR && later.PatchID == a.PatchID && later.Outcome == replay.OutcomePass {
				a.FlakySuspect = true
				break
			}
		}
	}
	return nil
}

// runSplit cuts a fetched dataset into consecutive epochs of -size attempts,
// written next to it as <name>.e01.json, <name>.e02.json, and so on. Each
// epoch is materialized on its own base, so a short epoch drifts less from
// upstream and loses fewer attempts to conflicts.
func runSplit(args []string) error {
	fs := flag.NewFlagSet("split", flag.ExitOnError)
	dsPath := fs.String("dataset", "", "dataset written by fetch (required)")
	size := fs.Int("size", 12, "attempts per epoch")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *dsPath == "" || *size < 1 {
		return errors.New("-dataset and a positive -size are required")
	}
	ds, err := replay.Load(*dsPath)
	if err != nil {
		return err
	}
	stem := strings.TrimSuffix(*dsPath, filepath.Ext(*dsPath))
	for n, start := 1, 0; start < len(ds.Attempts); n, start = n+1, start+*size {
		epoch := ds
		epoch.BaseSHA = ""
		epoch.Attempts = append([]replay.Attempt(nil), ds.Attempts[start:min(start+*size, len(ds.Attempts))]...)
		path := fmt.Sprintf("%s.e%02d.json", stem, n)
		if err := writeDataset(path, epoch); err != nil {
			return err
		}
		fmt.Println(path)
	}
	return nil
}

func runURIs(args []string) error {
	fs := flag.NewFlagSet("uris", flag.ExitOnError)
	dsPath := fs.String("dataset", "", "materialized dataset (required)")
	remote := fs.String("remote", "git.example.com", "authority in the printed change URIs")
	repoName := fs.String("repo-name", "sandbox", "repository segment in the printed change URIs")
	if err := fs.Parse(args); err != nil {
		return err
	}
	ds, err := replay.Load(*dsPath)
	if err != nil {
		return err
	}
	for _, a := range ds.Attempts {
		if a.ReplaySHA == "" {
			continue
		}
		fmt.Println(changegit.ChangeID{Scheme: "git", Remote: *remote, Repo: *repoName, Ref: "refs/heads/" + a.Branch, CommitSHA: a.ReplaySHA}.String())
	}
	return nil
}

func git(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	return run(cmd)
}

func run(cmd *exec.Cmd) (string, error) {
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(cmd.Args[1:], " "), err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(string(out)), nil
}
