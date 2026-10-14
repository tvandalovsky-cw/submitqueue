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

// LegacyTotal%[3]d_%[1]d%[2]d is kept for callers outside this module.
func LegacyTotal%[3]d_%[1]d%[2]d(x int) int { return x }

`, f, fn, p)
			}
			path := fmt.Sprintf("%s/file%02d.go", dir, f)
			files[path] = b.String()
			c.files = append(c.files, path)
		}
	}
	return mutate.NewTree(files), c
}

func defaultMix(t *testing.T) map[Tier]float64 {
	mix, err := parseTierMix(defaultTierMix)
	require.NoError(t, err)
	return mix
}

func TestTierCountsFollowTheMix(t *testing.T) {
	counts := tierCounts(200, defaultMix(t), -1)
	assert.Equal(t, map[Tier]int{TierS: 100, TierM: 80, TierL: 14, TierXL: 6}, counts)
}

func TestTierCountsSumToCount(t *testing.T) {
	for _, count := range []int{0, 1, 7, 33, 101} {
		total := 0
		for _, n := range tierCounts(count, defaultMix(t), defaultMaxXL) {
			total += n
		}
		assert.Equal(t, count, total, "count %d", count)
	}
}

func TestTierCountsCapXL(t *testing.T) {
	counts := tierCounts(100, map[Tier]float64{TierS: 1, TierXL: 1}, 10)
	assert.Equal(t, 10, counts[TierXL])
	assert.Equal(t, 40, counts[TierM], "the overflow goes to M")
	assert.Equal(t, 50, counts[TierS])
}

func TestPlanSpecsBalancesTheLabelWithinEveryTier(t *testing.T) {
	specs := planSpecs(200, 0.5, defaultMix(t), -1, rand.New(rand.NewSource(1)))
	require.Len(t, specs, 200)
	cells := map[spec]int{}
	for _, s := range specs {
		cells[s]++
	}
	for tier, n := range map[Tier]int{TierS: 100, TierM: 80, TierL: 14, TierXL: 6} {
		assert.Equal(t, n/2, cells[spec{intent: mutate.IntentSafe, tier: tier}], "safe %s", tier)
		assert.Equal(t, n/2, cells[spec{intent: mutate.IntentUnsafe, tier: tier}], "unsafe %s", tier)
	}
}

func TestPlanSpecsHonorsUnsafeRatio(t *testing.T) {
	unsafe := 0
	for _, s := range planSpecs(40, 0.25, map[Tier]float64{TierS: 1}, -1, rand.New(rand.NewSource(1))) {
		if s.intent == mutate.IntentUnsafe {
			unsafe++
		}
	}
	assert.Equal(t, 10, unsafe)
}

func TestParseTierMix(t *testing.T) {
	mix, err := parseTierMix("s=2, XL=1")
	require.NoError(t, err)
	assert.Equal(t, map[Tier]float64{TierS: 2, TierXL: 1}, mix)
	for _, bad := range []string{"", "S", "Q=1", "S=-1", "S=x", "S=0,M=0"} {
		_, err := parseTierMix(bad)
		assert.Error(t, err, bad)
	}
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
	batches, rows := planEvalBatches(changes, nil, evalFlags{batchSize: 3, batches: 2}, rand.New(rand.NewSource(1)))
	require.Len(t, batches, 5)
	require.Len(t, rows, 5)
	assert.True(t, rows[0].Lands)
	assert.False(t, rows[1].Lands)
	assert.Contains(t, rows[1].Slices, "needle beyond diff budget")
	assert.Equal(t, []string{"1"}, batches[0].Contains)
	assert.False(t, rows[3].Lands, "a batch holding a failing change fails")
	assert.Len(t, batches[3].Contains, 3)
}

func TestPlanEvalBatchesWithPairs(t *testing.T) {
	changes := []ManifestChange{
		{Branch: "s1", Lands: true, Tier: TierS, TargetTier: TierS, Mutators: []string{"rename-func"}},
		{Branch: "a", Lands: true, Tier: TierS, TargetTier: TierS, Pair: "p-0001"},
		{Branch: "b", Lands: true, Tier: TierS, TargetTier: TierS, Pair: "p-0001"},
		{Branch: "s2", Lands: false, Tier: TierS, TargetTier: TierS, Mutators: []string{"off-by-one"}},
	}
	pairs := []ManifestPair{{ID: "p-0001", Mutator: "rename-vs-new-caller", A: "a", B: "b", Lands: false}}

	batches, rows := planEvalBatches(changes, pairs, evalFlags{batchSize: 2, batches: 5}, rand.New(rand.NewSource(1)))
	require.Len(t, rows, 4+1+5)

	assert.Equal(t, []string{"pair member alone"}, rows[1].Slices, "a pair side is graded alone, outside the single-change slices")
	pair := rows[4]
	assert.Equal(t, []string{"a", "b"}, pair.Branches)
	assert.Equal(t, []string{"2", "3"}, batches[4].Contains)
	assert.False(t, pair.Lands, "a pair is labeled by its verified outcome, not by its sides'")
	assert.Equal(t, []string{"pairs", "pair family rename", "pair rename-vs-new-caller"}, pair.Slices)
	for _, row := range rows[5:] {
		assert.NotContains(t, row.Branches, "a", "sampled batches draw from single changes only")
		assert.NotContains(t, row.Branches, "b")
	}
}

func TestPlanEvalBatchesWithChangesAhead(t *testing.T) {
	changes := []ManifestChange{
		{Branch: "a", Lands: true, Pair: "p-0001"},
		{Branch: "b", Lands: true, Pair: "p-0001"},
	}
	pairs := []ManifestPair{{ID: "p-0001", Mutator: "signature-vs-new-caller", A: "a", B: "b", Lands: false}}

	batches, rows := planEvalBatches(changes, pairs, evalFlags{ahead: true}, rand.New(rand.NewSource(1)))
	require.Len(t, rows, 2+1+2)
	bWithA, aWithB := 3, 4
	assert.Equal(t, []string{"b"}, rows[bWithA].Branches)
	assert.Equal(t, []string{batches[0].ID}, batches[bWithA].Dependencies, "A's own batch is queued ahead")
	assert.Equal(t, []string{"a"}, rows[aWithB].Branches)
	assert.Equal(t, []string{batches[1].ID}, batches[aWithB].Dependencies)
	for _, i := range []int{bWithA, aWithB} {
		assert.False(t, rows[i].Lands, "a side on top of its conflicting partner fails")
		assert.Contains(t, rows[i].Slices, "ahead family signature")
	}
	assert.Empty(t, batches[0].Dependencies, "a change scored alone has nothing ahead")
}

func TestPairFamilyJoinsTwins(t *testing.T) {
	for _, intent := range []mutate.Intent{mutate.IntentSafe, mutate.IntentUnsafe} {
		families := map[string]bool{}
		for _, m := range mutate.PairsByIntent(intent) {
			families[pairFamily(m.Name)] = true
		}
		assert.Len(t, families, len(mutate.PairsByIntent(intent)), "each %s pair mutator is its own family", intent)
	}
	for _, m := range mutate.PairsByIntent(mutate.IntentUnsafe) {
		twin := false
		for _, s := range mutate.PairsByIntent(mutate.IntentSafe) {
			twin = twin || pairFamily(s.Name) == pairFamily(m.Name)
		}
		assert.True(t, twin, "%s has a compatible twin in its family", m.Name)
	}
}

func TestPlanPairMutatorsCoversEveryFamily(t *testing.T) {
	counts := map[string]int{}
	unsafe := 0
	for _, m := range planPairMutators(2*len(mutate.Pairs), 0.5, rand.New(rand.NewSource(1))) {
		counts[m.Name]++
		if m.Intent == mutate.IntentUnsafe {
			unsafe++
		}
	}
	assert.Equal(t, len(mutate.Pairs), unsafe)
	for _, m := range mutate.Pairs {
		assert.Equal(t, 2, counts[m.Name], m.Name)
	}
}

func TestBuildPair(t *testing.T) {
	tree, c := syntheticModule(6, 3)
	for _, m := range mutate.Pairs {
		t.Run(m.Name, func(t *testing.T) {
			p, ok := buildPair(tree, c, m, rand.New(rand.NewSource(4)))
			require.True(t, ok)
			assert.NotEmpty(t, p.A)
			assert.NotEmpty(t, p.B)
		})
	}
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
