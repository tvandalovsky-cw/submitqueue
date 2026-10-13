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
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/uber-go/tally"

	"github.com/uber/submitqueue/platform/base/change"
	gitexec "github.com/uber/submitqueue/platform/git/exec"
	gitexectest "github.com/uber/submitqueue/platform/git/exectest"
	gitrepo "github.com/uber/submitqueue/platform/git/repo"
	"github.com/uber/submitqueue/submitqueue/entity"
	"github.com/uber/submitqueue/submitqueue/extension/changediff"
)

type fixture struct {
	t      *testing.T
	git    string
	remote string
	work   string
	differ changediff.Provider
	repo   *gitrepo.Repo
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	root := t.TempDir()
	f := &fixture{
		t:      t,
		git:    gitexectest.Git(t),
		remote: filepath.Join(root, "remote.git"),
		work:   filepath.Join(root, "work"),
	}
	f.run("", "init", "--bare", "-b", "main", f.remote)
	f.run("", "clone", f.remote, f.work)
	f.run(f.work, "config", "user.name", "Test")
	f.run(f.work, "config", "user.email", "test@example.invalid")
	f.write("seed.txt", "seed\n")
	f.run(f.work, "add", ".")
	f.run(f.work, "commit", "-m", "seed")
	f.run(f.work, "push", "origin", "main")

	repo, err := gitrepo.NewRepo(gitrepo.RepoConfig{
		Git:       f.git,
		Path:      filepath.Join(root, "copy.git"),
		RemoteURL: f.remote,
		Target:    "main",
	})
	require.NoError(t, err)
	require.NoError(t, repo.Provision(context.Background()))
	f.repo = repo
	f.differ = New(Params{Config: changediff.Config{QueueName: "q"}, Repo: repo, MetricsScope: tally.NoopScope})
	return f
}

func (f *fixture) run(dir string, args ...string) string {
	f.t.Helper()
	out, err := gitexec.Command(context.Background(), f.git, dir, args...).CombinedOutput()
	require.NoError(f.t, err, "git %s: %s", strings.Join(args, " "), out)
	return strings.TrimSpace(string(out))
}

func (f *fixture) write(path, contents string) {
	f.t.Helper()
	full := filepath.Join(f.work, path)
	require.NoError(f.t, os.MkdirAll(filepath.Dir(full), 0o755))
	require.NoError(f.t, os.WriteFile(full, []byte(contents), 0o644))
}

func (f *fixture) push(branch, base string, files map[string]string) string {
	f.t.Helper()
	f.run(f.work, "checkout", "-B", branch, base)
	for path, contents := range files {
		f.write(path, contents)
	}
	f.run(f.work, "add", "-A")
	f.run(f.work, "commit", "-m", branch)
	f.run(f.work, "push", "-f", "origin", branch)
	return f.run(f.work, "rev-parse", "HEAD")
}

func (f *fixture) mainSHA() string {
	return f.run(f.work, "rev-parse", "origin/main")
}

func (f *fixture) uri(branch, sha string) string {
	return fmt.Sprintf("git://git.example.com/demo/%s/%s", url.PathEscape("refs/heads/"+branch), sha)
}

func (f *fixture) diff(uris ...string) ([]entity.ChangeDiff, error) {
	return f.differ.Diff(context.Background(), change.Change{URIs: uris})
}

func TestDiff(t *testing.T) {
	t.Run("single change carries its code", func(t *testing.T) {
		f := newFixture(t)
		head := f.push("feature/one", f.mainSHA(), map[string]string{"pkg/a/one.go": "package a\n\nfunc One() int { return 1 }\n"})

		diffs, err := f.diff(f.uri("feature/one", head))
		require.NoError(t, err)
		require.Len(t, diffs, 1)
		assert.Equal(t, f.uri("feature/one", head), diffs[0].URI)
		assert.Contains(t, diffs[0].Patch, "+++ b/pkg/a/one.go")
		assert.Contains(t, diffs[0].Patch, "+func One() int { return 1 }")
	})

	// Diffing each change against the target would repeat the earlier changes'
	// code in every later one.
	t.Run("stack is diffed per change", func(t *testing.T) {
		f := newFixture(t)
		first := f.push("feature/s1", f.mainSHA(), map[string]string{"a.txt": "first\n"})
		second := f.push("feature/s2", first, map[string]string{"b.txt": "second\n"})

		diffs, err := f.diff(f.uri("feature/s1", first), f.uri("feature/s2", second))
		require.NoError(t, err)
		require.Len(t, diffs, 2)
		assert.Contains(t, diffs[0].Patch, "+first")
		assert.Contains(t, diffs[1].Patch, "+second")
		assert.NotContains(t, diffs[1].Patch, "+first")
	})

	// A change cut from a target commit the copy has not fetched yet must not
	// be diffed against the stale target, or it would carry that commit too.
	t.Run("target advanced since last fetch", func(t *testing.T) {
		f := newFixture(t)
		_, err := f.diff(f.uri("feature/warm", f.push("feature/warm", f.mainSHA(), map[string]string{"w.txt": "w\n"})))
		require.NoError(t, err)

		f.run(f.work, "checkout", "main")
		f.write("upstream.txt", "upstream\n")
		f.run(f.work, "add", "-A")
		f.run(f.work, "commit", "-m", "upstream")
		f.run(f.work, "push", "origin", "main")
		head := f.push("feature/rebased", f.mainSHA(), map[string]string{"mine.txt": "mine\n"})

		diffs, err := f.diff(f.uri("feature/rebased", head))
		require.NoError(t, err)
		assert.Contains(t, diffs[0].Patch, "+mine")
		assert.NotContains(t, diffs[0].Patch, "upstream")
	})

	t.Run("malformed URI is an error", func(t *testing.T) {
		_, err := newFixture(t).diff("not-a-change-uri")
		require.Error(t, err)
	})

	t.Run("unknown commit is an error", func(t *testing.T) {
		f := newFixture(t)
		_, err := f.diff(f.uri("feature/nope", "0123456789abcdef0123456789abcdef01234567"))
		require.Error(t, err)
	})
}

func (f *fixture) contextDiff(uri string) entity.ChangeDiff {
	f.t.Helper()
	d := New(Params{Repo: f.repo, IncludeFiles: true, IncludeReferences: true})
	diffs, err := d.Diff(context.Background(), change.Change{URIs: []string{uri}})
	require.NoError(f.t, err)
	require.Len(f.t, diffs, 1)
	return diffs[0]
}

func refsOf(diff entity.ChangeDiff, kind entity.CodeReferenceKind) []string {
	var out []string
	for _, r := range diff.References {
		if r.Kind == kind {
			out = append(out, fmt.Sprintf("%s %s:%d", r.Symbol, r.Path, r.Line))
		}
	}
	return out
}

func TestDiffContext(t *testing.T) {
	seed := map[string]string{
		"pkg/scale.go":  "package pkg\n\nfunc Scale(v int) int {\n\treturn v * 10\n}\n",
		"pkg/report.go": "package pkg\n\nfunc Report(v int) int {\n\treturn Scale(v)\n}\n",
		"pkg/parse.go":  "package pkg\n\nfunc ParsePort(s string) (int, error) {\n\treturn 0, nil\n}\n",
		"pkg/old.go":    "package pkg\n",
		// Same names in an unrelated package: never a reference to pkg's.
		"other/other.go": "package other\n\nfunc Scale(v int) int {\n\treturn Scale(v)\n}\n\nfunc ParsePort(s string) (int, error) {\n\treturn 0, nil\n}\n",
		// A qualified use from another package is a real caller.
		"cmd/main.go": "package main\n\nfunc main() {\n\t_ = pkg.Scale(1)\n}\n",
	}
	newSeeded := func(t *testing.T) (*fixture, string) {
		f := newFixture(t)
		return f, f.push("main", f.mainSHA(), seed)
	}

	t.Run("modified files are returned in full, added and deleted ones omitted", func(t *testing.T) {
		f, base := newSeeded(t)
		f.run(f.work, "checkout", "-B", "feature/files", base)
		f.write("pkg/scale.go", "package pkg\n\nfunc Scale(v int) int {\n\treturn v * 100\n}\n")
		f.run(f.work, "rm", "-q", "pkg/old.go")
		f.write("pkg/new.go", "package pkg\n\nconst Added = 1\n")
		f.run(f.work, "add", "pkg/new.go")
		f.run(f.work, "commit", "-qam", "files")
		f.run(f.work, "push", "-f", "origin", "feature/files")
		head := f.run(f.work, "rev-parse", "HEAD")

		diff := f.contextDiff(f.uri("feature/files", head))
		require.Len(t, diff.Files, 1)
		assert.Equal(t, "pkg/scale.go", diff.Files[0].Path)
		assert.Contains(t, diff.Files[0].Content, "return v * 100")
	})

	// The caller is outside the diff, so only a search of the tree can show
	// that it still calls the old signature.
	t.Run("a changed signature reports its untouched callers", func(t *testing.T) {
		f, base := newSeeded(t)
		head := f.push("feature/sig", base, map[string]string{
			"pkg/scale.go": "package pkg\n\nfunc Scale(v, factor int) int {\n\treturn v * factor\n}\n",
		})

		diff := f.contextDiff(f.uri("feature/sig", head))
		assert.Equal(t, []string{"Scale cmd/main.go:4", "Scale pkg/report.go:4"}, refsOf(diff, entity.CodeReferenceKindCaller),
			"the unqualified Scale in package other is a different function")
		for _, r := range diff.References {
			assert.Equal(t, "func Scale(v int) int {", r.DeclarationBefore)
			assert.Equal(t, "func Scale(v, factor int) int {", r.DeclarationAfter)
		}
	})

	t.Run("a rename that updates its callers reports none", func(t *testing.T) {
		f, base := newSeeded(t)
		head := f.push("feature/rename", base, map[string]string{
			"pkg/scale.go":  "package pkg\n\nfunc Rescale(v int) int {\n\treturn v * 10\n}\n",
			"pkg/report.go": "package pkg\n\nfunc Report(v int) int {\n\treturn Rescale(v)\n}\n",
			"cmd/main.go":   "package main\n\nfunc main() {\n\t_ = pkg.Rescale(1)\n}\n",
		})

		diff := f.contextDiff(f.uri("feature/rename", head))
		assert.Empty(t, refsOf(diff, entity.CodeReferenceKindCaller))
	})

	t.Run("a call to a function defined elsewhere reports its definition", func(t *testing.T) {
		f, base := newSeeded(t)
		head := f.push("feature/call", base, map[string]string{
			"pkg/addr.go": "package pkg\n\nfunc Addr(p string) error {\n\t_, err := ParsePort(p)\n\treturn err\n}\n",
		})

		diff := f.contextDiff(f.uri("feature/call", head))
		assert.Equal(t, []string{"ParsePort pkg/parse.go:3"}, refsOf(diff, entity.CodeReferenceKindDefinition),
			"a bare call cannot reach package other's ParsePort")
	})

	t.Run("a call to a function defined nowhere reports nothing", func(t *testing.T) {
		f, base := newSeeded(t)
		head := f.push("feature/missing", base, map[string]string{
			"pkg/addr.go": "package pkg\n\nfunc Addr(p string) error {\n\treturn Missing(p)\n}\n",
		})

		diff := f.contextDiff(f.uri("feature/missing", head))
		assert.Empty(t, refsOf(diff, entity.CodeReferenceKindDefinition))
	})
}
