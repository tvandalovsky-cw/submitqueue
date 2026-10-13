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

func TestThreadContext(t *testing.T) {
	tree := NewTree(map[string]string{
		"pkg/a.go": "package sample\n\nfunc scale(v int) int { return v * 2 }\n\nfunc twice(v int) int { return scale(scale(v)) }\n",
		"pkg/b.go": "package sample\n\nimport \"context\"\n\nfunc Handle(ctx context.Context, v int) int { return scale(v) }\n",
		"pkg/c.go": "package sample\n\nimport stdctx \"context\"\n\nfunc Other(c stdctx.Context) int {\n\treturn func() int { return scale(1) }()\n}\n",
	})
	pair, ok := SignatureVsNewCaller.Apply(tree, "pkg", rand.New(rand.NewSource(1)))
	require.True(t, ok)
	a := pair.A

	assert.Contains(t, a["pkg/a.go"], "func scale(ctx context.Context, v int) int")
	assert.Contains(t, a["pkg/a.go"], `import "context"`, "a file that had no context import gains one")
	assert.Contains(t, a["pkg/a.go"], "scale(context.TODO(), scale(context.TODO(), v))", "a caller without a context passes TODO")
	assert.Contains(t, a["pkg/b.go"], "scale(ctx, v)", "a caller with a context passes its own")
	assert.Contains(t, a["pkg/c.go"], "scale(c, 1)", "a closure inherits its enclosing context, under the file's import name")
	assert.NoError(t, typeCheck(tree.With(a), "pkg"))
	assert.Error(t, typeCheck(tree.With(a).With(pair.B), "pkg"))
	for _, src := range pair.B {
		assert.Regexp(t, `func [A-Z]\w*Scale\(`, src, "the new caller is exported")
	}
}

func TestThreadContextSkipsFunctionsThatTakeOne(t *testing.T) {
	tree := NewTree(map[string]string{
		"pkg/a.go": "package sample\n\nimport \"context\"\n\nfunc scale(ctx context.Context, v int) int { return v }\n\nfunc A() int { return scale(context.TODO(), 1) }\n",
	})
	_, ok := SignatureVsNewCaller.Apply(tree, "pkg", rand.New(rand.NewSource(1)))
	assert.False(t, ok)
}

func TestRemoveAPI(t *testing.T) {
	t.Run("an exported function nothing mentions is removable", func(t *testing.T) {
		tree := NewTree(map[string]string{
			"pkg/a.go": "package sample\n\n// Legacy is deprecated.\nfunc Legacy(v int) int { return v }\n\nfunc keep(v int) int { return v }\n",
		})
		pair, ok := RemoveAPIVsNewCaller.Apply(tree, "pkg", rand.New(rand.NewSource(1)))
		require.True(t, ok)
		assert.NotContains(t, pair.A["pkg/a.go"], "Legacy")
		assert.Error(t, typeCheck(tree.With(pair.A).With(pair.B), "pkg"))
	})
	t.Run("a mention anywhere in the tree keeps it", func(t *testing.T) {
		tree := NewTree(map[string]string{
			"pkg/a.go":   "package sample\n\n// Legacy is deprecated.\nfunc Legacy(v int) int { return v }\n",
			"other/b.go": "package other\n\n// See sample.Legacy for the old behavior.\nvar X = 1\n",
		})
		_, ok := RemoveAPIVsNewCaller.Apply(tree, "pkg", rand.New(rand.NewSource(1)))
		assert.False(t, ok)
	})
}

func TestProbeTargetsAndBehaviorMutants(t *testing.T) {
	tree := NewTree(map[string]string{"pkg/a.go": strings.Join([]string{
		"package sample",
		"",
		"func Clamp(v, lo, hi int) int {",
		"\tif v < lo {",
		"\t\treturn lo",
		"\t}",
		"\tif v > hi {",
		"\t\treturn hi",
		"\t}",
		"\treturn v",
		"}",
		"",
		"func Parse(s string) (int, error) { if len(s) == 0 { return 0, nil }; return len(s), nil }",
		"",
		"func noCompare(v int) int { return v }",
		"",
		"func withMap(m map[string]int) int { if len(m) > 0 { return 1 }; return 0 }",
		"",
	}, "\n")})

	targets := ProbeTargets(tree, "pkg")
	byName := map[string]ProbeTarget{}
	for _, pt := range targets {
		byName[pt.Func] = pt
	}
	require.Contains(t, byName, "Clamp")
	assert.Equal(t, []string{"int", "int", "int"}, byName["Clamp"].Params, "grouped parameters are listed one per name")
	assert.Equal(t, []string{"v", "lo", "hi"}, byName["Clamp"].Names)
	assert.Equal(t, []string{"int"}, byName["Clamp"].Results)
	require.Contains(t, byName, "Parse")
	assert.True(t, byName["Parse"].ReturnsError)
	assert.NotContains(t, byName, "noCompare", "a function with no comparison has no behavior mutant")
	assert.NotContains(t, byName, "withMap", "a map parameter cannot be written as a literal")

	mutants := BehaviorMutants(tree, "pkg/a.go", "Clamp", rand.New(rand.NewSource(1)))
	assert.Len(t, mutants, 4, "two comparisons, each flipped or relaxed")
	for _, m := range mutants {
		assert.Contains(t, []string{FlipComparison.Name, OffByOne.Name}, m.Mutator)
		assert.NoError(t, typeCheck(tree.With(m.Patch), "pkg"))
		assert.NotContains(t, m.Patch["pkg/a.go"], "func Parse(s string) (int, error) { if len(s) != 0", "only the named function changes")
	}
}

func TestWordCount(t *testing.T) {
	tree := NewTree(map[string]string{"a.go": "package a // Legacy\nfunc Legacy() {}\n", "b.go": "package b\nvar LegacyX = 1\n"})
	assert.Equal(t, 2, tree.wordCount("Legacy"), "whole words only")
	assert.Equal(t, 0, tree.With(Patch{"a.go": "package a\n"}).wordCount("Legacy"), "a patched tree counts its own files")
}
