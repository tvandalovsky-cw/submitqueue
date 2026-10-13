# Synthetic Scorer Corpus

Grading outcome scorers on generated Go changes whose outcome is caused by their code and confirmed by building and testing them, as a counterpart to the noisy labels of replayed history.

**Status:** Phase 1 is implemented as `tool/jevcorpus`: ten mutators, size-tiered sweeps with planted needles, build-verified labels, a replay-format dataset and sandbox, and offline grading of `jev` against a size baseline. Interaction pairs (phase 2) and queue runs at scale (phase 3) are proposed.

See [replay-benchmark.md](replay-benchmark.md) for the real-history benchmark this complements and [code-aware-scoring.md](code-aware-scoring.md) for what the `jev` scorer sends.

## Why a synthetic corpus

The demo workload writes text files and fails builds through URI markers, so nothing in its diffs decides an outcome and no content scorer can do better than chance on it. Replay supplies real failures, but its labels carry misattribution, flakiness and context drift, its failure mix is whatever the upstream repository happened to suffer, and its changes are mostly small. Neither can say *why* a scorer missed a failure, or whether it would have seen a bug placed beyond its diff budget.

A synthetic corpus controls exactly those variables. Every change is a known edit to real code, its label is the outcome of actually building and testing it, and the corpus is balanced so that size, position and surface features cannot stand in for reading the code.

## Design

### Seed

Changes are made to a real Go repository at a fixed commit, so that diffs, touched files and repository references look like real work. Any Go module with tests serves; a replay epoch base is a natural choice because it lets the two benchmarks share a codebase. Only packages whose tests pass at the base are mutated, and generated files are left alone, so every failure in the corpus is one the mutation caused.

### Mutators

Each mutator is a deterministic edit spliced into the source at token positions, so its diff is only the lines it means to change. Five are designed to be safe — renaming an unexported function with every use, renaming locals, adding doc comments, deleting an unreferenced function, adding a tested helper in a new file — and five to fail — inverting a comparison, an off-by-one bound, dropping an error check, deleting a function still called, and adding a parameter without updating callers. They span compile failures, test failures, and changes that look alarming but are harmless (a deleted function) or look trivial but are not (a one-character bound).

### Size tiers and needles

Changes are planned evenly across four tiers by measured diff: S (under 50 lines), M (under 500), L (500 or more), and XL, which exceeds the `jev` scorer's diff budget or file cap. Larger changes are sweeps: mechanical, behavior-preserving edits across many files, occasionally with a safe package-level edit as a decoy. An unsafe sweep carries one unsafe edit — the needle — in the first or the last touched file in path order. Because the diff is path-ordered, a late needle in an XL change sits past the point the scorer truncates, and the manifest records whether it does. That measures directly what truncation costs, and so what the proposed tiers of [code-aware-scoring.md](code-aware-scoring.md) would buy.

### Labels

A change's label is the exit status of a verify command run on the base plus the change — by default the whole module builds and the touched packages' tests pass. A change whose observed outcome contradicts its design is regenerated rather than relabeled, so a category such as "dropped error check" contains only drops that a test actually catches, and the regeneration rate per mutator shows which ones are weak.

### Leakage

Nothing a scorer can read names the mutation. Branches are numbered, commit subjects and authors are drawn from fixed pools independently of the label, and mutators write ordinary identifiers and comments. The answer key — intent, mutators, tier, needle placement, failing output — lives in a manifest beside the dataset, which no part of the queue reads.

### Output

The dataset is in the replay format and the sandbox has the same shape `replaybench materialize` produces, so `replaybench uris`, the `replay` build runner, the `oracle` scorer and the git compose overlay all work on a synthetic corpus unchanged. A verified change's build duration becomes its recorded duration.

## Grading

`jevcorpus eval` scores every change, and optionally sampled multi-change batches, through the real `jev` scorer, the git change provider and the git diff provider, with no fallback and no cache so that a failure is visible as one. It reports AUC, Brier score, log loss, expected calibration error and a calibration table for each scorer, overall and per tier, per category, per needle position, and for needles past the diff budget. A size baseline that prices batches by lines changed sits beside it: on a balanced corpus it should rank no better than chance, and a model that does no better than it has learned only size. The oracle, which prices from the record, is the ceiling by construction.

Slices holding one outcome, such as a single unsafe mutator, have no AUC; their mean price per outcome still shows how far a scorer moved.

## Phases

1. **Single changes** (implemented): mutators, tiers, needles, verified labels, offline grading.
2. **Interaction pairs** (proposed): two changes that each pass alone and fail together — a rename against a new caller of the old name, two changes adding the same symbol — which is what batch scoring exists to catch. The replay runner fails a build only when a member is recorded as failing, so pairs need either a dataset record of which combinations fail, verified at generation time, or a build runner that builds the merged tree.
3. **Queue runs** (proposed): the corpus through the git compose overlay with the replay runner, for time to land and wasted builds, compared with replay results on the same seed.

## Known limits

- Verification runs the touched packages' tests, not their dependents', so a change that breaks only a downstream package's tests is labeled as landing. A broader verify command trades generation time for that.
- Mutations are syntactic. They do not produce semantic conflicts across packages, concurrency bugs, or performance regressions, and a model could in principle learn their shapes; holding out a seed or a mutator from any tuning guards against that.
- Sweeps are mechanical, which real large changes rarely are. They test whether a scorer can find one consequential line in a large diff, not whether it understands a large redesign.
