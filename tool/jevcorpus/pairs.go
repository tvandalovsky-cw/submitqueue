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

	"github.com/uber/submitqueue/tool/jevcorpus/mutate"
)

// planIntents returns n intents with the given unsafe share, shuffled.
func planIntents(n int, unsafeRatio float64, r *rand.Rand) []mutate.Intent {
	unsafe := int(float64(n)*unsafeRatio + 0.5)
	out := make([]mutate.Intent, n)
	for i := range out {
		out[i] = mutate.IntentSafe
		if i < unsafe {
			out[i] = mutate.IntentUnsafe
		}
	}
	r.Shuffle(n, func(i, j int) { out[i], out[j] = out[j], out[i] })
	return out
}

// maxNoiseFiles bounds the unrelated files a pair side may also touch.
const maxNoiseFiles = 3

// addPairNoise gives each side, half the time, unrelated mechanical edits in
// up to maxNoiseFiles other files, as real changes carry more than the line
// that matters. Noise files are drawn from outside both sides, so the sides
// stay disjoint. It returns the pair and the number of noise files added.
func addPairNoise(t mutate.Tree, c candidates, p mutate.Pair, r *rand.Rand) (mutate.Pair, int) {
	used := map[string]bool{}
	for file := range p.A.Merge(p.B) {
		used[file] = true
	}
	added := 0
	sides := []*mutate.Patch{&p.A, &p.B}
	for _, side := range sides {
		if r.Intn(2) == 0 {
			continue
		}
		want := 1 + r.Intn(maxNoiseFiles)
		for _, i := range r.Perm(len(c.files)) {
			if want == 0 {
				break
			}
			file := c.files[i]
			if used[file] {
				continue
			}
			m := mutate.DocumentFuncs
			if r.Intn(2) == 0 {
				m = mutate.RenameLocals
			}
			if edit, ok := m.Apply(t, file, r); ok {
				*side = side.Merge(edit)
				used[file] = true
				added++
				want--
			}
		}
	}
	return p, added
}

// planPairMutators returns n pair mutators with the given conflicting share,
// cycling through each intent's mutators so every family is represented
// whether or not its sites are common, then shuffled.
func planPairMutators(n int, unsafeRatio float64, r *rand.Rand) []mutate.PairMutator {
	unsafe := int(float64(n)*unsafeRatio + 0.5)
	conflicting, compatible := mutate.PairsByIntent(mutate.IntentUnsafe), mutate.PairsByIntent(mutate.IntentSafe)
	out := make([]mutate.PairMutator, n)
	for i := range out {
		if i < unsafe {
			out[i] = conflicting[i%len(conflicting)]
		} else {
			out[i] = compatible[(i-unsafe)%len(compatible)]
		}
	}
	r.Shuffle(n, func(i, j int) { out[i], out[j] = out[j], out[i] })
	return out
}

// buildPair applies m in the first candidate package, in random order, that
// has a site for it.
func buildPair(t mutate.Tree, c candidates, m mutate.PairMutator, r *rand.Rand) (mutate.Pair, bool) {
	for _, i := range r.Perm(len(c.dirs)) {
		if p, ok := m.Apply(t, c.dirs[i], r); ok {
			return p, true
		}
	}
	return mutate.Pair{}, false
}

// pairOutcome is a pair verified three ways: each side alone, then both.
type pairOutcome struct {
	pair     ManifestPair
	a, b     ManifestChange
	aTitle   string
	bTitle   string
	accepted bool
	reason   string
}

// verifyPair checks the combination first, since it is the result most likely
// to contradict the design, and commits each side only once it has. A pair is
// accepted only if both sides land alone and the combination does what the
// mutator intends.
func verifyPair(work, moduleRoot, base, verify, compileCheck, id, branchA, branchB string, p mutate.Pair, name string, intent mutate.Intent, noise int, r *rand.Rand) (pairOutcome, error) {
	out := pairOutcome{pair: ManifestPair{ID: id, Mutator: name, Intent: intent, A: branchA, B: branchB, NoiseFiles: noise}}
	both := p.A.Merge(p.B)
	if err := stagePatch(work, base, both); err != nil {
		return pairOutcome{}, err
	}
	v, err := runVerify(work, moduleRoot, verify, both)
	if err != nil {
		return pairOutcome{}, err
	}
	out.pair.Lands, out.pair.VerifyMs, out.pair.VerifyTail = v.lands, v.ms, v.tail
	if v.lands != (intent == mutate.IntentSafe) {
		out.reason = fmt.Sprintf("together lands=%v", v.lands)
		return out, nil
	}
	out.pair.CompilesTogether = v.lands
	if !v.lands {
		c, err := runVerify(work, moduleRoot, compileCheck, both)
		if err != nil {
			return pairOutcome{}, err
		}
		out.pair.CompilesTogether = c.lands
	}
	sides := []struct {
		branch string
		patch  mutate.Patch
		change *ManifestChange
		title  *string
	}{
		{branchA, p.A, &out.a, &out.aTitle},
		{branchB, p.B, &out.b, &out.bTitle},
	}
	for _, side := range sides {
		mc, title, err := commitChange(work, moduleRoot, base, side.branch, verify, compileCheck, built{patch: side.patch, mutators: []string{name}}, r)
		if err != nil {
			return pairOutcome{}, err
		}
		if !mc.Lands {
			out.reason = side.branch + " fails alone"
			return out, nil
		}
		mc.Intent, mc.TargetTier, mc.Pair = mutate.IntentSafe, TierS, id
		*side.change, *side.title = mc, title
	}
	out.pair.ASHA, out.pair.BSHA = out.a.SHA, out.b.SHA
	out.accepted = true
	return out, nil
}
