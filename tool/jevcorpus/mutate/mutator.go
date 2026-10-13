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

package mutate

import (
	"math/rand"
)

// Intent is what a mutation is designed to do to the build.
type Intent string

const (
	// IntentUnknown is the zero-value sentinel.
	IntentUnknown Intent = ""
	// IntentSafe mutations are meant to keep the module building and its
	// tests passing.
	IntentSafe Intent = "safe"
	// IntentUnsafe mutations are meant to break the build or a test.
	IntentUnsafe Intent = "unsafe"
)

// Scope is what a mutator's target names.
type Scope string

const (
	// ScopeUnknown is the zero-value sentinel.
	ScopeUnknown Scope = ""
	// ScopeFile mutators take a Go file path.
	ScopeFile Scope = "file"
	// ScopePackage mutators take a package directory.
	ScopePackage Scope = "package"
)

// Mutator is one kind of edit.
type Mutator struct {
	// Name identifies the mutator in a corpus manifest; stable across releases.
	Name string
	// Intent is what the edit is designed to do to the build.
	Intent Intent
	// Scope is what Apply's target names.
	Scope Scope
	apply func(t Tree, target string, r *rand.Rand) (Patch, bool)
}

// Apply edits target in t. It returns false when target has no site for this
// mutator. Every returned file is gofmt-clean and differs from t's.
func (m Mutator) Apply(t Tree, target string, r *rand.Rand) (Patch, bool) {
	p, ok := m.apply(t, target, r)
	if !ok || len(p) == 0 {
		return nil, false
	}
	out := make(Patch, len(p))
	for path, src := range p {
		formatted, ok := gofmt(src)
		if !ok {
			return nil, false
		}
		if old, exists := t.File(path); exists && old == formatted {
			continue
		}
		out[path] = formatted
	}
	if len(out) == 0 {
		return nil, false
	}
	return out, true
}

// All is every mutator, safe first.
var All = []Mutator{
	RenameFunc, RenameLocals, DocumentFuncs, DeleteDeadFunc, AddHelper,
	FlipComparison, OffByOne, DropErrorCheck, DeleteCalledFunc, AddParam,
}

// ByIntent returns the mutators with intent i and scope s, in All order. An
// empty s matches every scope.
func ByIntent(i Intent, s Scope) []Mutator {
	var out []Mutator
	for _, m := range All {
		if m.Intent == i && (s == ScopeUnknown || m.Scope == s) {
			out = append(out, m)
		}
	}
	return out
}
