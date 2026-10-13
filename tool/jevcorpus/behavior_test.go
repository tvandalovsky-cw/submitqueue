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
	"context"
	"go/format"
	"math/rand"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/uber/submitqueue/submitqueue/entity"
	"github.com/uber/submitqueue/tool/jevcorpus/mutate"
)

var clampTarget = mutate.ProbeTarget{
	File: "pkg/clamp.go", Package: "sample", Func: "clamp",
	Params: []string{"int", "int", "int"}, Names: []string{"v", "lo", "hi"}, Results: []string{"int"},
}

func TestProbeSourceIsValidGo(t *testing.T) {
	withErr := clampTarget
	withErr.ReturnsError = true
	for _, pt := range []mutate.ProbeTarget{clampTarget, withErr} {
		src := probeSource(pt, [][]string{{"1", "0", "10"}, {"-1", "0", "10"}})
		_, err := format.Source([]byte(src))
		require.NoError(t, err, src)
		assert.Contains(t, src, "clamp(1, 0, 10)")
		assert.Equal(t, 2, strings.Count(src, "defer func()"), "every case recovers its own panic")
	}
}

func TestBehaviorTestPatch(t *testing.T) {
	cand := behaviorCandidate{
		target: clampTarget,
		cases:  [][]string{{"1", "0", "10"}, {"11", "0", "10"}, {"-1", "0", "10"}},
		base:   []string{"1", "10", "0"},
	}
	p, ok := behaviorTestPatch(mutate.NewTree(nil), cand, []int{1, 2})
	require.True(t, ok)
	require.Len(t, p, 1)
	src := p["pkg/clamp_cases_test.go"]
	_, err := format.Source([]byte(src))
	require.NoError(t, err, src)
	assert.Contains(t, src, "func TestClampCases(t *testing.T)")
	assert.Contains(t, src, `{name: "v=11 lo=0 hi=10", v: 11, lo: 0, hi: 10, want0: 10}`, "expected values are the base's outputs")
	assert.NotContains(t, src, "v: 1, lo", "only chosen cases are written")

	_, exists := behaviorTestPatch(mutate.NewTree(map[string]string{"pkg/clamp_cases_test.go": "package sample\n"}), cand, []int{0})
	assert.False(t, exists, "an existing file is never overwritten")
}

func TestBehaviorTestPatchWithErrorOnly(t *testing.T) {
	pt := mutate.ProbeTarget{File: "pkg/v.go", Package: "sample", Func: "Validate", Params: []string{"string"}, Names: []string{"name"}, ReturnsError: true}
	cand := behaviorCandidate{target: pt, cases: [][]string{{`""`}}, base: []string{"err=true"}}
	p, ok := behaviorTestPatch(mutate.NewTree(nil), cand, []int{0})
	require.True(t, ok)
	src := p["pkg/validate_cases_test.go"]
	_, err := format.Source([]byte(src))
	require.NoError(t, err, src)
	assert.NotContains(t, src, `"reflect"`, "no result to compare means no reflect import")
	assert.Contains(t, src, "caseName string", "a parameter called name does not collide with the case label")
	assert.Contains(t, src, "wantErr: true")
}

func TestChooseBehaviorCases(t *testing.T) {
	cand := behaviorCandidate{killing: []int{0, 1}, keeping: []int{2, 3, 4, 5}}
	for seed := int64(0); seed < 20; seed++ {
		r := rand.New(rand.NewSource(seed))
		conflicting := chooseBehaviorCases(cand, true, r)
		hasKilling := false
		for _, i := range conflicting {
			hasKilling = hasKilling || i < 2
		}
		assert.True(t, hasKilling, "a conflicting test includes an input the change affects")

		compatible := chooseBehaviorCases(cand, false, r)
		assert.GreaterOrEqual(t, len(compatible), 2)
		for _, i := range compatible {
			assert.GreaterOrEqual(t, i, 2, "a compatible test uses only unaffected inputs")
		}
	}
}

func TestAddPairNoiseKeepsSidesDisjoint(t *testing.T) {
	tree, c := syntheticModule(4, 4)
	p := mutate.Pair{A: mutate.Patch{c.files[0]: "package x\n"}, B: mutate.Patch{c.files[1]: "package x\n"}}
	sawNoise := false
	for seed := int64(0); seed < 20; seed++ {
		got, n := addPairNoise(tree, c, p, rand.New(rand.NewSource(seed)))
		sawNoise = sawNoise || n > 0
		assert.Equal(t, len(got.A)+len(got.B)-2, n)
		for file := range got.A {
			assert.NotContains(t, got.B, file)
		}
		assert.Equal(t, "package x\n", got.A[c.files[0]], "noise never rewrites a side's own edit")
	}
	assert.True(t, sawNoise)
}

func TestCompileScorer(t *testing.T) {
	s := compileScorer{"1": true, "2": false}
	p, err := s.Score(context.Background(), entity.Batch{ID: "1"}, entity.SpeculationPathSet{})
	require.NoError(t, err)
	assert.Greater(t, p, 0.5)
	p, err = s.Score(context.Background(), entity.Batch{ID: "2"}, entity.SpeculationPathSet{})
	require.NoError(t, err)
	assert.Less(t, p, 0.5)
}
