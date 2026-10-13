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
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"math/rand"
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const fixtureA = `package sample

// Total sums the values no greater than limit.
func Total(values []int, limit int) int {
	sum := 0
	for i := 0; i < len(values); i++ {
		if values[i] > limit {
			continue
		}
		sum += values[i]
	}
	return sum
}

func helper(n int) int {
	if n == 3 {
		return 0
	}
	return n * 2
}

func unused(x int) int { return x + 1 }

func Double(n int) int { return helper(n) }

func check(err error) error {
	if err != nil {
		return err
	}
	return nil
}

type counter struct{ hits int }

func (c *counter) bump(by int) {
	total := c.hits + by
	c.hits = total
}
`

const fixtureB = `package sample

func Use() int {
	res := helper(4) + Total([]int{1, 2}, 5)
	for _, v := range []int{1, 2} {
		res += v
	}
	return res
}
`

func fixtureTree() Tree {
	return NewTree(map[string]string{"pkg/a.go": fixtureA, "pkg/b.go": fixtureB})
}

// typeCheck type-checks the non-test files in dir. The fixture imports
// nothing, so no importer is needed.
func typeCheck(t Tree, dir string) error {
	fset := token.NewFileSet()
	var files []*ast.File
	for _, p := range t.GoFiles(dir) {
		if IsTestFile(p) {
			continue
		}
		src, _ := t.File(p)
		f, err := parser.ParseFile(fset, p, src, 0)
		if err != nil {
			return err
		}
		files = append(files, f)
	}
	_, err := (&types.Config{}).Check(dir, fset, files, nil)
	return err
}

func TestFixtureTypeChecks(t *testing.T) {
	require.NoError(t, typeCheck(fixtureTree(), "pkg"))
}

func TestMutators(t *testing.T) {
	tests := []struct {
		mutator        Mutator
		target         string
		wantTypeChecks bool
	}{
		{mutator: RenameFunc, target: "pkg", wantTypeChecks: true},
		{mutator: RenameLocals, target: "pkg/a.go", wantTypeChecks: true},
		{mutator: RenameLocals, target: "pkg/b.go", wantTypeChecks: true},
		{mutator: DocumentFuncs, target: "pkg/a.go", wantTypeChecks: true},
		{mutator: DeleteDeadFunc, target: "pkg", wantTypeChecks: true},
		{mutator: AddHelper, target: "pkg", wantTypeChecks: true},
		{mutator: FlipComparison, target: "pkg/a.go", wantTypeChecks: true},
		{mutator: OffByOne, target: "pkg/a.go", wantTypeChecks: true},
		{mutator: DropErrorCheck, target: "pkg/a.go", wantTypeChecks: true},
		{mutator: DeleteCalledFunc, target: "pkg", wantTypeChecks: false},
		{mutator: AddParam, target: "pkg", wantTypeChecks: false},
	}
	for _, tt := range tests {
		t.Run(tt.mutator.Name+" "+tt.target, func(t *testing.T) {
			for seed := int64(0); seed < 20; seed++ {
				tree := fixtureTree()
				patch, ok := tt.mutator.Apply(tree, tt.target, rand.New(rand.NewSource(seed)))
				require.True(t, ok, "seed %d found no site", seed)
				for p, src := range patch {
					old, _ := tree.File(p)
					assert.NotEqual(t, old, src, "seed %d left %s unchanged", seed, p)
				}
				err := typeCheck(tree.With(patch), "pkg")
				if tt.wantTypeChecks {
					assert.NoError(t, err, "seed %d", seed)
				} else {
					assert.Error(t, err, "seed %d", seed)
				}
			}
		})
	}
}

func TestMutatorsAreDeterministic(t *testing.T) {
	for _, m := range All {
		t.Run(m.Name, func(t *testing.T) {
			target := "pkg/a.go"
			if m.Scope == ScopePackage {
				target = "pkg"
			}
			first, ok1 := m.Apply(fixtureTree(), target, rand.New(rand.NewSource(7)))
			second, ok2 := m.Apply(fixtureTree(), target, rand.New(rand.NewSource(7)))
			assert.Equal(t, ok1, ok2)
			assert.Equal(t, first, second)
		})
	}
}

func TestMutatorsWithoutSites(t *testing.T) {
	bare := NewTree(map[string]string{"pkg/a.go": "package sample\n\n// V is a value.\nvar V = 1\n"})
	for _, m := range All {
		if m.Name == AddHelper.Name {
			continue
		}
		t.Run(m.Name, func(t *testing.T) {
			target := "pkg/a.go"
			if m.Scope == ScopePackage {
				target = "pkg"
			}
			_, ok := m.Apply(bare, target, rand.New(rand.NewSource(1)))
			assert.False(t, ok)
		})
	}
}

func TestFileMutatorsSkipTestFiles(t *testing.T) {
	tree := NewTree(map[string]string{"pkg/a_test.go": fixtureA})
	for _, m := range ByIntent(IntentUnsafe, ScopeFile) {
		t.Run(m.Name, func(t *testing.T) {
			_, ok := m.Apply(tree, "pkg/a_test.go", rand.New(rand.NewSource(1)))
			assert.False(t, ok)
		})
	}
}

func TestRenameFuncRenamesEveryUse(t *testing.T) {
	tree := NewTree(map[string]string{
		"pkg/a.go": "package sample\n\n// helper doubles n.\nfunc helper(n int) int { return n * 2 }\n",
		"pkg/b.go": "package sample\n\nfunc Use() int { return helper(4) }\n",
	})
	patch, ok := RenameFunc.Apply(tree, "pkg", rand.New(rand.NewSource(3)))
	require.True(t, ok)
	assert.Equal(t, []string{"pkg/a.go", "pkg/b.go"}, patch.Paths())
	assert.NotContains(t, patch["pkg/a.go"], "// helper doubles", "the doc comment follows the rename")
	assert.NoError(t, typeCheck(tree.With(patch), "pkg"))
}

func TestAddParamHandlesTrailingComma(t *testing.T) {
	src := "package sample\n\nfunc f(\n\ta int,\n) int {\n\treturn a\n}\n\nfunc g() int { return f(1) }\n"
	tree := NewTree(map[string]string{"pkg/a.go": src})
	patch, ok := AddParam.Apply(tree, "pkg", rand.New(rand.NewSource(1)))
	require.True(t, ok)
	assert.Error(t, typeCheck(tree.With(patch), "pkg"))
	_, parses := gofmt(patch["pkg/a.go"])
	assert.True(t, parses)
}

func TestByIntent(t *testing.T) {
	var names []string
	for _, m := range ByIntent(IntentUnsafe, ScopeFile) {
		names = append(names, m.Name)
	}
	sort.Strings(names)
	assert.Equal(t, []string{"drop-error-check", "flip-comparison", "off-by-one"}, names)
}

func TestSplitWords(t *testing.T) {
	tests := map[string][]string{
		"parseHTTPHeader": {"parse", "http", "header"},
		"newClient":       {"new", "client"},
		"ID":              {"id"},
		"snake_case":      {"snake", "case"},
	}
	for in, want := range tests {
		t.Run(in, func(t *testing.T) {
			assert.Equal(t, want, splitWords(in))
		})
	}
}

func TestDocSentence(t *testing.T) {
	assert.Equal(t, "// parseHeader parses the header.", docSentence("parseHeader"))
	assert.Equal(t, "// isValid reports whether valid.", docSentence("isValid"))
	assert.Equal(t, "// Total implements the total step.", docSentence("Total"))
}
