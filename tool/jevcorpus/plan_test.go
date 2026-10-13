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
	"fmt"
	"math/rand"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/uber/submitqueue/tool/jevcorpus/mutate"
)

// syntheticModule is a tree of undocumented functions with locals, comparisons
// and error checks — enough sites for every mutator and for sweeps of any
// tier.
func syntheticModule(packages, filesPerPackage int) (mutate.Tree, candidates) {
	files := map[string]string{}
	var c candidates
	for p := 0; p < packages; p++ {
		dir := fmt.Sprintf("pkg%02d", p)
		c.dirs = append(c.dirs, dir)
		for f := 0; f < filesPerPackage; f++ {
			var b strings.Builder
			fmt.Fprintf(&b, "package pkg%02d\n\n", p)
			for fn := 0; fn < 6; fn++ {
				fmt.Fprintf(&b, `func computeTotal%[1]d%[2]d(values []int, limit int) (int, error) {
	res := 0
	for i := 0; i < len(values); i++ {
		v := values[i]
		if v > limit {
			continue
		}
		res += v
	}
	err := validateTotal%[1]d%[2]d(res)
	if err != nil {
		return 0, err
	}
	return res, nil
}

func validateTotal%[1]d%[2]d(n int) error {
	if n == 42 {
		return nil
	}
	return nil
}

func unusedHelper%[1]d%[2]d(x int) int { return x + 1 }

`, f, fn)
			}
			path := fmt.Sprintf("%s/file%02d.go", dir, f)
			files[path] = b.String()
			c.files = append(c.files, path)
		}
	}
	return mutate.NewTree(files), c
}

func TestPlanSpecsBalancesIntentAndTier(t *testing.T) {
	specs := planSpecs(80, 0.5, rand.New(rand.NewSource(1)))
	require.Len(t, specs, 80)
	cells := map[spec]int{}
	for _, s := range specs {
		cells[s]++
	}
	for _, intent := range []mutate.Intent{mutate.IntentSafe, mutate.IntentUnsafe} {
		for _, tier := range tiers {
			assert.Equal(t, 10, cells[spec{intent: intent, tier: tier}], "%s %s", intent, tier)
		}
	}
}

func TestPlanSpecsHonorsUnsafeRatio(t *testing.T) {
	unsafe := 0
	for _, s := range planSpecs(40, 0.25, rand.New(rand.NewSource(1))) {
		if s.intent == mutate.IntentUnsafe {
			unsafe++
		}
	}
	assert.Equal(t, 10, unsafe)
}

func TestTierFor(t *testing.T) {
	tests := []struct {
		name                    string
		lines, diffBytes, files int
		want                    Tier
	}{
		{name: "small", lines: 10, diffBytes: 800, files: 1, want: TierS},
		{name: "medium", lines: 50, diffBytes: 4000, files: 3, want: TierM},
		{name: "large", lines: 500, diffBytes: 40_000, files: 20, want: TierL},
		{name: "past the diff budget", lines: 900, diffBytes: 70_000, files: 40, want: TierXL},
		{name: "past the file cap", lines: 300, diffBytes: 20_000, files: 201, want: TierXL},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, tierFor(tt.lines, tt.diffBytes, tt.files))
		})
	}
}

func TestBuildChange(t *testing.T) {
	tree, c := syntheticModule(30, 12)
	for _, intent := range []mutate.Intent{mutate.IntentSafe, mutate.IntentUnsafe} {
		for _, tier := range tiers {
			t.Run(string(intent)+" "+string(tier), func(t *testing.T) {
				r := rand.New(rand.NewSource(5))
				b, ok := buildChange(tree, c, spec{intent: intent, tier: tier}, r)
				require.True(t, ok)
				require.NotEmpty(t, b.patch)
				for _, name := range b.mutators {
					m := mutatorNamed(t, name)
					if m.Intent == mutate.IntentUnsafe {
						assert.Equal(t, mutate.IntentUnsafe, intent, "a safe change applied %s", name)
					}
				}
				if tier != TierS {
					lines, bytes := estimateDiff(tree, b.patch)
					if target := sweepTargets[tier]; target.bytes > 0 {
						assert.GreaterOrEqual(t, bytes, target.bytes)
					} else {
						assert.GreaterOrEqual(t, lines, target.lines)
					}
				}
				if intent == mutate.IntentUnsafe && tier != TierS {
					assert.NotEmpty(t, b.needle.mutator)
					assert.Contains(t, b.patch, b.needle.file)
				}
			})
		}
	}
}

func TestPlantNeedlePosition(t *testing.T) {
	tree, c := syntheticModule(4, 4)
	sweep, ok := buildSweep(tree, c, spec{intent: mutate.IntentSafe, tier: TierM}, rand.New(rand.NewSource(2)))
	require.True(t, ok)
	paths := sweep.patch.Paths()
	seen := map[NeedlePosition]bool{}
	for seed := int64(0); seed < 20; seed++ {
		b, ok := plantNeedle(tree, sweep, rand.New(rand.NewSource(seed)))
		require.True(t, ok)
		seen[b.needle.position] = true
		switch b.needle.position {
		case NeedleEarly:
			assert.Equal(t, paths[0], b.needle.file)
		case NeedleLate:
			assert.Equal(t, paths[len(paths)-1], b.needle.file)
		}
	}
	assert.True(t, seen[NeedleEarly] && seen[NeedleLate], "both positions are planted")
}

func TestCommitSubjectDoesNotNameTheMutation(t *testing.T) {
	p := mutate.Patch{"pkg/store/a.go": ""}
	for seed := int64(0); seed < 50; seed++ {
		subject := commitSubject(p, rand.New(rand.NewSource(seed)))
		assert.Contains(t, subject, "store")
		for _, m := range mutate.All {
			assert.NotContains(t, subject, m.Name)
		}
	}
}

func TestManifestChangeCategory(t *testing.T) {
	tests := []struct {
		name   string
		change ManifestChange
		want   string
	}{
		{name: "single edit", change: ManifestChange{TargetTier: TierS, Mutators: []string{"flip-comparison"}}, want: "flip-comparison"},
		{name: "safe sweep", change: ManifestChange{TargetTier: TierL, Mutators: []string{"document-funcs"}}, want: "sweep"},
		{name: "sweep with a needle", change: ManifestChange{TargetTier: TierXL, Needle: ManifestNeedle{Mutator: "off-by-one"}}, want: "sweep+off-by-one"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, tt.change.Category())
		})
	}
}

func TestPlanEvalBatches(t *testing.T) {
	changes := []ManifestChange{
		{Branch: "a", Lands: true, Tier: TierS, TargetTier: TierS, Mutators: []string{"rename-func"}},
		{Branch: "b", Lands: false, Tier: TierXL, TargetTier: TierXL, Needle: ManifestNeedle{Mutator: "off-by-one", Position: NeedleLate, BeyondDiffBudget: true}},
		{Branch: "c", Lands: true, Tier: TierM, TargetTier: TierM},
	}
	batches, rows := planEvalBatches(changes, evalFlags{batchSize: 3, batches: 2}, rand.New(rand.NewSource(1)))
	require.Len(t, batches, 5)
	require.Len(t, rows, 5)
	assert.True(t, rows[0].Lands)
	assert.False(t, rows[1].Lands)
	assert.Contains(t, rows[1].Slices, "needle beyond diff budget")
	assert.Equal(t, []string{"1"}, batches[0].Contains)
	assert.False(t, rows[3].Lands, "a batch holding a failing change fails")
	assert.Len(t, batches[3].Contains, 3)
}

func mutatorNamed(t *testing.T, name string) mutate.Mutator {
	for _, m := range mutate.All {
		if m.Name == name {
			return m
		}
	}
	require.Failf(t, "unknown mutator", "%q", name)
	return mutate.Mutator{}
}
