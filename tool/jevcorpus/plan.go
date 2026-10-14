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
	"path"
	"sort"
	"strconv"
	"strings"

	"github.com/uber/submitqueue/submitqueue/extension/speculation/scorer/jev"
	"github.com/uber/submitqueue/tool/jevcorpus/mutate"
)

// Tier is a change's size class. XL is defined by the jev scorer's diff budget
// and file cap, so an XL change is one the model cannot see in full.
type Tier string

const (
	// TierUnknown is the zero-value sentinel.
	TierUnknown Tier = ""
	// TierS is under 50 changed lines.
	TierS Tier = "S"
	// TierM is 50 to 499 changed lines.
	TierM Tier = "M"
	// TierL is 500 or more changed lines within the diff budget.
	TierL Tier = "L"
	// TierXL exceeds the diff budget or the file cap.
	TierXL Tier = "XL"
)

var tiers = []Tier{TierS, TierM, TierL, TierXL}

// maxFilesInState mirrors the jev scorer's per-batch file cap.
const maxFilesInState = 200

func tierFor(lines, diffBytes, files int) Tier {
	switch {
	case diffBytes > jev.DefaultMaxDiffBytes || files > maxFilesInState:
		return TierXL
	case lines >= 500:
		return TierL
	case lines >= 50:
		return TierM
	default:
		return TierS
	}
}

// NeedlePosition is where an unsafe edit sits among a large change's files.
type NeedlePosition string

const (
	// NeedleNone is the zero-value sentinel: no needle.
	NeedleNone NeedlePosition = ""
	// NeedleEarly puts the needle in the first touched file in path order,
	// which is where the diff starts.
	NeedleEarly NeedlePosition = "early"
	// NeedleLate puts it in the last, where a truncated diff is cut.
	NeedleLate NeedlePosition = "late"
)

// spec is one change the corpus should contain.
type spec struct {
	intent mutate.Intent
	tier   Tier
}

// defaultTierMix is the share of changes per tier, from 1,650 merged pull
// requests across six Go repositories: about half under 50 lines, 40% under
// 500, and the rest split between changes that fit the jev diff budget and
// changes that do not. See doc/rfc/submitqueue/synthetic-corpus.md.
const defaultTierMix = "S=50,M=40,L=7,XL=3"

// defaultMaxXL bounds the XL slice. XL changes are rare and slow to verify,
// and a few dozen are enough to measure what truncation costs.
const defaultMaxXL = 20

// parseTierMix reads "S=50,M=40,L=7,XL=3" into a weight per tier. A tier left
// out has no weight; weights need not sum to 100.
func parseTierMix(mix string) (map[Tier]float64, error) {
	out := map[Tier]float64{}
	total := 0.0
	for _, part := range strings.Split(mix, ",") {
		name, value, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			return nil, fmt.Errorf("tier mix %q: %q is not tier=weight", mix, part)
		}
		tier := Tier(strings.ToUpper(strings.TrimSpace(name)))
		known := false
		for _, t := range tiers {
			known = known || t == tier
		}
		if !known {
			return nil, fmt.Errorf("tier mix %q: unknown tier %q", mix, name)
		}
		w, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
		if err != nil || w < 0 {
			return nil, fmt.Errorf("tier mix %q: weight %q is not a non-negative number", mix, value)
		}
		out[tier] = w
		total += w
	}
	if total == 0 {
		return nil, fmt.Errorf("tier mix %q has no positive weight", mix)
	}
	return out, nil
}

// tierCounts splits count over the tiers in proportion to mix, by largest
// remainder, then caps XL at maxXL and gives the overflow to M, the tier where
// real failures concentrate.
func tierCounts(count int, mix map[Tier]float64, maxXL int) map[Tier]int {
	total := 0.0
	for _, w := range mix {
		total += w
	}
	counts := map[Tier]int{}
	type remainder struct {
		tier Tier
		frac float64
	}
	var rems []remainder
	assigned := 0
	for _, t := range tiers {
		exact := float64(count) * mix[t] / total
		counts[t] = int(exact)
		assigned += counts[t]
		rems = append(rems, remainder{t, exact - float64(counts[t])})
	}
	sort.SliceStable(rems, func(i, j int) bool { return rems[i].frac > rems[j].frac })
	for i := 0; assigned < count; i++ {
		counts[rems[i%len(rems)].tier]++
		assigned++
	}
	if maxXL >= 0 && counts[TierXL] > maxXL {
		counts[TierM] += counts[TierXL] - maxXL
		counts[TierXL] = maxXL
	}
	return counts
}

// planSpecs plans count changes over the tiers by tierCounts, and within each
// tier designs unsafeRatio of them to fail. Balancing the label inside every
// tier, rather than the tiers against each other, is what keeps size from
// predicting the label while the tiers keep their real-world proportions.
func planSpecs(count int, unsafeRatio float64, mix map[Tier]float64, maxXL int, r *rand.Rand) []spec {
	counts := tierCounts(count, mix, maxXL)
	out := make([]spec, 0, count)
	for _, t := range tiers {
		n := counts[t]
		unsafe := int(float64(n)*unsafeRatio + 0.5)
		for i := 0; i < n; i++ {
			intent := mutate.IntentSafe
			if i < unsafe {
				intent = mutate.IntentUnsafe
			}
			out = append(out, spec{intent: intent, tier: t})
		}
	}
	r.Shuffle(len(out), func(i, j int) { out[i], out[j] = out[j], out[i] })
	return out
}

// candidates are the mutation targets: non-generated, non-test files and their
// directories, in packages whose tests pass at the base.
type candidates struct {
	files []string
	dirs  []string
}

// built is a change ready to write to the work tree.
type built struct {
	patch    mutate.Patch
	mutators []string
	needle   needle
}

// needle locates the one unsafe edit in an otherwise safe sweep.
type needle struct {
	mutator  string
	file     string
	position NeedlePosition
}

// sweepTarget is the approximate size a sweep grows to before it stops: lines
// for M and L, estimated diff bytes for XL.
type sweepTarget struct {
	lines, bytes int
}

var sweepTargets = map[Tier]sweepTarget{
	TierM:  {lines: 120},
	TierL:  {lines: 900},
	TierXL: {bytes: jev.DefaultMaxDiffBytes * 3 / 2},
}

// maxSweepFiles bounds how far a sweep searches for files it can grow by.
const maxSweepFiles = 600

func buildChange(t mutate.Tree, c candidates, s spec, r *rand.Rand) (built, bool) {
	if s.tier == TierS {
		return buildSingle(t, c, s.intent, r)
	}
	return buildSweep(t, c, s, r)
}

func buildSingle(t mutate.Tree, c candidates, intent mutate.Intent, r *rand.Rand) (built, bool) {
	ms := mutate.ByIntent(intent, mutate.ScopeUnknown)
	for try := 0; try < 40; try++ {
		m := ms[r.Intn(len(ms))]
		target, ok := pickTarget(c, m.Scope, r)
		if !ok {
			continue
		}
		if p, ok := m.Apply(t, target, r); ok {
			return built{patch: p, mutators: []string{m.Name}}, true
		}
	}
	return built{}, false
}

func pickTarget(c candidates, s mutate.Scope, r *rand.Rand) (string, bool) {
	pool := c.files
	if s == mutate.ScopePackage {
		pool = c.dirs
	}
	if len(pool) == 0 {
		return "", false
	}
	return pool[r.Intn(len(pool))], true
}

// buildSweep grows a mechanical, behavior-preserving change file by file until
// it reaches the tier's size, sometimes adding a safe package-level edit as a
// decoy, then for an unsafe spec plants one unsafe edit at the first or last
// touched file.
func buildSweep(t mutate.Tree, c candidates, s spec, r *rand.Rand) (built, bool) {
	target := sweepTargets[s.tier]
	patch := mutate.Patch{}
	est := newDiffEstimate(t)
	swept := 0
	var applied []string
	if r.Intn(10) < 3 {
		decoys := []mutate.Mutator{mutate.DeleteDeadFunc, mutate.RenameFunc, mutate.AddHelper}
		m := decoys[r.Intn(len(decoys))]
		if dir, ok := pickTarget(c, mutate.ScopePackage, r); ok {
			if p, ok := m.Apply(t, dir, r); ok {
				patch = patch.Merge(p)
				est.update(patch, p)
				applied = append(applied, m.Name)
			}
		}
	}
	for _, i := range r.Perm(len(c.files)) {
		if est.reached(target) || swept >= maxSweepFiles {
			break
		}
		file := c.files[i]
		swept++
		for _, m := range []mutate.Mutator{mutate.DocumentFuncs, mutate.RenameLocals} {
			if p, ok := m.Apply(t.With(patch), file, r); ok {
				patch = patch.Merge(p)
				est.update(patch, p)
				applied = appendOnce(applied, m.Name)
			}
		}
	}
	if len(patch) == 0 || !est.reached(target) {
		return built{}, false
	}
	b := built{patch: patch, mutators: applied}
	if s.intent == mutate.IntentSafe {
		return b, true
	}
	return plantNeedle(t, b, r)
}

func plantNeedle(t mutate.Tree, b built, r *rand.Rand) (built, bool) {
	files := b.patch.Paths()
	position := NeedleEarly
	if r.Intn(2) == 0 {
		position = NeedleLate
		sort.Sort(sort.Reverse(sort.StringSlice(files)))
	}
	needles := mutate.ByIntent(mutate.IntentUnsafe, mutate.ScopeFile)
	for _, file := range files {
		for _, i := range r.Perm(len(needles)) {
			m := needles[i]
			if p, ok := m.Apply(t.With(b.patch), file, r); ok {
				return built{
					patch:    b.patch.Merge(p),
					mutators: append(append([]string(nil), b.mutators...), m.Name),
					needle:   needle{mutator: m.Name, file: file, position: position},
				}, true
			}
		}
	}
	return built{}, false
}

func appendOnce(list []string, s string) []string {
	for _, x := range list {
		if x == s {
			return list
		}
	}
	return append(list, s)
}

// diffEstimate tracks a growing patch's estimated size file by file, so a sweep
// re-estimates only what each step touched.
type diffEstimate struct {
	base         mutate.Tree
	perFile      map[string][2]int
	lines, bytes int
}

func newDiffEstimate(base mutate.Tree) *diffEstimate {
	return &diffEstimate{base: base, perFile: map[string][2]int{}}
}

func (d *diffEstimate) update(full, step mutate.Patch) {
	for path := range step {
		prev := d.perFile[path]
		lines, bytes := estimateDiff(d.base, mutate.Patch{path: full[path]})
		d.perFile[path] = [2]int{lines, bytes}
		d.lines += lines - prev[0]
		d.bytes += bytes - prev[1]
	}
}

func (d *diffEstimate) reached(target sweepTarget) bool {
	if target.bytes > 0 {
		return d.bytes >= target.bytes
	}
	return d.lines >= target.lines
}

// estimateDiff approximates a patch's changed lines and unified-diff size
// without running git: changed lines are the per-file multiset difference of
// old and new lines, and each carries roughly its own length again in context.
// It only steers sweep growth; the corpus records git's own numbers.
func estimateDiff(t mutate.Tree, p mutate.Patch) (lines, bytes int) {
	for path, src := range p {
		old, _ := t.File(path)
		counts := map[string]int{}
		for _, l := range strings.Split(old, "\n") {
			counts[l]++
		}
		for _, l := range strings.Split(src, "\n") {
			counts[l]--
		}
		for l, n := range counts {
			if n < 0 {
				n = -n
			}
			lines += n
			bytes += n * 2 * (len(l) + 2)
		}
		bytes += 2*len(path) + 64
	}
	return lines, bytes
}

var commitSubjects = []string{
	"refactor(%s): tidy internals",
	"%s: clean up helpers",
	"chore(%s): small cleanups",
	"%s: simplify control flow",
	"fix(%s): adjust edge-case handling",
	"%s: update docs and naming",
	"perf(%s): avoid redundant work",
	"%s: follow-up to review comments",
}

var authors = []struct{ name, email string }{
	{"Avery Lin", "avery.lin@corpus.invalid"},
	{"Jordan Patel", "jordan.patel@corpus.invalid"},
	{"Sam Okafor", "sam.okafor@corpus.invalid"},
	{"Riley Novak", "riley.novak@corpus.invalid"},
	{"Casey Moreau", "casey.moreau@corpus.invalid"},
	{"Morgan Ito", "morgan.ito@corpus.invalid"},
}

// commitSubject names a change from where it is, never from what it does: the
// subject is drawn independently of the mutation so it cannot leak the label.
func commitSubject(p mutate.Patch, r *rand.Rand) string {
	paths := p.Paths()
	scope := path.Base(path.Dir(paths[r.Intn(len(paths))]))
	return fmt.Sprintf(commitSubjects[r.Intn(len(commitSubjects))], scope)
}
