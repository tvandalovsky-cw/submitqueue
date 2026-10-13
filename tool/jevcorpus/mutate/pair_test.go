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
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPairMutators(t *testing.T) {
	for _, m := range Pairs {
		t.Run(m.Name, func(t *testing.T) {
			for seed := int64(0); seed < 20; seed++ {
				tree := fixtureTree()
				pair, ok := m.Apply(tree, "pkg", rand.New(rand.NewSource(seed)))
				require.True(t, ok, "seed %d found no site", seed)

				for file := range pair.A {
					assert.NotContains(t, pair.B, file, "seed %d: both sides edit %s", seed, file)
				}
				assert.NoError(t, typeCheck(tree.With(pair.A), "pkg"), "seed %d: A alone must build", seed)
				assert.NoError(t, typeCheck(tree.With(pair.B), "pkg"), "seed %d: B alone must build", seed)

				both := typeCheck(tree.With(pair.A).With(pair.B), "pkg")
				if m.Intent == IntentUnsafe {
					assert.Error(t, both, "seed %d: the pair must break together", seed)
				} else {
					assert.NoError(t, both, "seed %d: the pair must build together", seed)
				}
			}
		})
	}
}

func TestPairMutatorsAreDeterministic(t *testing.T) {
	for _, m := range Pairs {
		t.Run(m.Name, func(t *testing.T) {
			first, ok1 := m.Apply(fixtureTree(), "pkg", rand.New(rand.NewSource(3)))
			second, ok2 := m.Apply(fixtureTree(), "pkg", rand.New(rand.NewSource(3)))
			assert.Equal(t, ok1, ok2)
			assert.Equal(t, first, second)
		})
	}
}

func TestPairsAreBalanced(t *testing.T) {
	assert.Len(t, PairsByIntent(IntentUnsafe), len(PairsByIntent(IntentSafe)),
		"every conflicting shape has a compatible twin, so shape alone cannot predict the outcome")
}

func TestSignatureVsNewCallerUpdatesEveryCall(t *testing.T) {
	tree := NewTree(map[string]string{
		"pkg/a.go": "package sample\n\nfunc scale(v int) int { return v * 2 }\n\nfunc A() int { return scale(1) }\n",
		"pkg/b.go": "package sample\n\nfunc B() int {\n\treturn scale(\n\t\t2,\n\t)\n}\n",
	})
	pair, ok := SignatureVsNewCaller.Apply(tree, "pkg", rand.New(rand.NewSource(1)))
	require.True(t, ok)
	assert.Equal(t, []string{"pkg/a.go", "pkg/b.go"}, pair.A.Paths(), "the caller in another file is updated too")
	assert.NoError(t, typeCheck(tree.With(pair.A), "pkg"))
	assert.Error(t, typeCheck(tree.With(pair.A).With(pair.B), "pkg"))
}

func TestSignatureChangeSkipsFunctionValues(t *testing.T) {
	tree := NewTree(map[string]string{
		"pkg/a.go": "package sample\n\nfunc scale(v int) int { return v * 2 }\n\nvar apply = scale\n\nfunc A() int { return scale(1) }\n",
	})
	for _, m := range []PairMutator{SignatureVsNewCaller, SignatureVsOtherCaller} {
		_, ok := m.Apply(tree, "pkg", rand.New(rand.NewSource(1)))
		assert.False(t, ok, "%s: a function used as a value would break A alone", m.Name)
	}
}

func TestWrapperImportsWhatItsSignatureNeeds(t *testing.T) {
	src := "package sample\n\nimport (\n\t\"context\"\n\t\"strings\"\n)\n\nfunc lookup(ctx context.Context, keys ...string) (string, error) {\n\treturn strings.Join(keys, \",\"), ctx.Err()\n}\n"
	tree := NewTree(map[string]string{"pkg/a.go": src})
	files := packageFiles(tree, "pkg")
	sites := unexportedFuncs(files)
	require.Len(t, sites, 1)

	p, ok := wrapperPatch(tree, files, sites[0], rand.New(rand.NewSource(1)))
	require.True(t, ok)
	require.Len(t, p, 1)
	for _, wrapper := range p {
		formatted, parses := gofmt(wrapper)
		require.True(t, parses)
		assert.Contains(t, formatted, `"context"`)
		assert.NotContains(t, formatted, `"strings"`, "an import only the body used is not carried over")
		assert.Contains(t, formatted, "keys...)", "a variadic parameter is forwarded as a spread")
		assert.True(t, strings.Contains(formatted, "return lookup("), "results are returned")
	}
}
