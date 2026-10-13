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

// Package mutate makes deterministic, labeled edits to Go source: changes meant
// to keep a module building and its tests passing (safe) and changes meant to
// break one or the other (unsafe). Edits are spliced into the original text at
// token positions rather than reprinted from the syntax tree, so a mutation's
// diff is only the lines it means to change.
//
// The intent is a hypothesis, not a label. A caller turns it into a label by
// building and testing the result; see tool/jevcorpus.
package mutate

import (
	"path"
	"sort"
	"strings"
	"sync"
)

// Tree is an immutable snapshot of Go source files keyed by slash-separated
// path relative to the repository root.
type Tree struct {
	files map[string]string
	// words counts identifier-shaped words across every file, built on first
	// use. It is shared by copies of one Tree, which hold the same files.
	words *wordIndex
}

type wordIndex struct {
	once   sync.Once
	counts map[string]int
}

// NewTree returns a Tree holding a copy of files.
func NewTree(files map[string]string) Tree {
	cp := make(map[string]string, len(files))
	for p, src := range files {
		cp[p] = src
	}
	return Tree{files: cp, words: &wordIndex{}}
}

// wordCount is how many times w appears as a whole identifier-shaped word in
// the tree, comments and strings included.
func (t Tree) wordCount(w string) int {
	t.words.once.Do(func() {
		t.words.counts = map[string]int{}
		for _, src := range t.files {
			for _, word := range identifierPattern.FindAllString(src, -1) {
				t.words.counts[word]++
			}
		}
	})
	return t.words.counts[w]
}

// File returns the content at p.
func (t Tree) File(p string) (string, bool) {
	src, ok := t.files[p]
	return src, ok
}

// Len is the number of files in the tree.
func (t Tree) Len() int { return len(t.files) }

// Dirs returns every directory holding a Go file, sorted.
func (t Tree) Dirs() []string {
	seen := map[string]bool{}
	for p := range t.files {
		seen[path.Dir(p)] = true
	}
	dirs := make([]string, 0, len(seen))
	for d := range seen {
		dirs = append(dirs, d)
	}
	sort.Strings(dirs)
	return dirs
}

// GoFiles returns the Go files directly in dir, sorted.
func (t Tree) GoFiles(dir string) []string {
	var out []string
	for p := range t.files {
		if path.Dir(p) == dir && strings.HasSuffix(p, ".go") {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}

// With returns a new Tree with p's files replacing or adding to t's.
func (t Tree) With(p Patch) Tree {
	cp := make(map[string]string, len(t.files)+len(p))
	for k, v := range t.files {
		cp[k] = v
	}
	for k, v := range p {
		cp[k] = v
	}
	return Tree{files: cp, words: &wordIndex{}}
}

// Patch is a set of whole-file contents keyed by path. A path absent from the
// tree it applies to is a new file.
type Patch map[string]string

// Merge returns a new Patch holding p's files overlaid by q's.
func (p Patch) Merge(q Patch) Patch {
	out := make(Patch, len(p)+len(q))
	for k, v := range p {
		out[k] = v
	}
	for k, v := range q {
		out[k] = v
	}
	return out
}

// Paths returns the patched paths, sorted.
func (p Patch) Paths() []string {
	out := make([]string, 0, len(p))
	for k := range p {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// IsTestFile reports whether p is a Go test file.
func IsTestFile(p string) bool { return strings.HasSuffix(p, "_test.go") }
