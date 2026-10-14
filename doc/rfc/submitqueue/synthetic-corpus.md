# Synthetic Scorer Corpus

Grading outcome scorers on generated Go changes whose outcome is caused by their code and confirmed by building and testing them, as a counterpart to the noisy labels of replayed history.

**Status:** Phases 1 and 2 are implemented as `tool/jevcorpus`: ten single-change mutators, size-tiered sweeps with planted needles, eight pair mutators, build-verified labels, a replay-format dataset and sandbox whose runner understands pairs, and offline grading of `jev` against a size baseline. Queue runs at scale (phase 3) are proposed.

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

Changes fall into four tiers by measured diff: S (under 50 lines), M (under 500), L (500 or more), and XL, which exceeds the `jev` scorer's diff budget or file cap. The planner splits them in real-world proportions — by default S 50%, M 40%, L 7%, XL 3% (`-tier-mix`), with XL capped at 20 changes (`-max-xl`) and any overflow given to M — and designs the same share of each tier to fail, so size never predicts the label without the rare tiers being inflated. Larger changes are sweeps: mechanical, behavior-preserving edits across many files, occasionally with a safe package-level edit as a decoy. An unsafe sweep carries one unsafe edit — the needle — in the first or the last touched file in path order. Because the diff is path-ordered, a late needle in an XL change sits past the point the scorer truncates, and the manifest records whether it does. That measures directly what truncation costs, and so what the proposed tiers of [code-aware-scoring.md](code-aware-scoring.md) would buy.

Those proportions come from 1,650 recently merged pull requests across cilium, containerd, jaeger, opentelemetry-collector, pulumi and submitqueue. About 8% exceed the diff budget — an estimated 64 bytes of unified diff per changed line puts it near 1,000 lines — and roughly half of those are dependency bumps, vendored or generated code, docs and test fixtures rather than hand-written code, so about 4% of pull requests are hand-written changes the scorer cannot see in full. Joined with merge-queue outcomes, XL attempts failed no more often than small ones (23% against 22%, from 43 and 224 attempts), while M attempts failed most (39%). XL therefore earns a small fixed slice, enough to measure what truncation costs, and M the largest share of the effort.

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
2. **Interaction pairs** (implemented): see below.
3. **Queue runs** (proposed): the corpus through the git compose overlay with the replay runner, for time to land and wasted builds, compared with replay results on the same seed.

## Interaction pairs

A pair is two changes made against the same base that touch disjoint files, so their textual merge is always clean. Each side is verified to land alone, and the two are verified together. This is the commonest code failure found in real merge-queue history: one change alters an API and updates every use it can see, while another, written against the old API, adds a use it cannot — cilium#45579 and pulumi#24570 and #24588 are examples.

Five structural pair mutators conflict, each written to read as routine work: threading a `context.Context` through a function and every call — callers pass their own context or `context.TODO()` — against a new caller with the old arguments; a rename against a new use of the old name; deleting an unexported function nothing references against its first new caller; retiring an exported function nothing in the repository mentions against its first new caller; and two changes adding the same helper. A new caller is an exported function, so it reads as new API rather than dead code. Each has a compatible twin of the same shape — the same A against a B that touches a different function, or a different helper — so that the shape of a pair cannot predict its outcome; a family's AUC asks whether a scorer tells the twins apart. The planner cycles through the mutators so every family is represented however rare its sites. The unexported delete family finds no site in a lint-clean seed and is kept for seeds that have dead code.

Structural conflicts are compile errors, which a type-checker finds for nothing. Behavior pairs conflict where no compiler looks. A changes one comparison in a function whose parameters and results are basic values, in a way the package's existing tests do not notice; B adds a table-driven test of that function. The function is probed first — called on a few dozen literal inputs, twice each to drop anything nondeterministic, at the base and under each candidate change — and B's expected values are the base's outputs. A conflicting B includes an input A's change affects; its twin tests the same function on inputs A's change leaves alone, so telling them apart means reasoning about what the function returns.

Each side of a pair, half the time, also carries unrelated mechanical edits in up to three other files, as real changes carry more than the line that matters.

Both sides are committed as ordinary numbered branches with neutral subjects. The dataset records the conflict as `fails_with` on the second side, which the replay runner and the oracle read: a build holding both fails, and a build holding either alone passes. The manifest records each pair's mutator, intent, combined verification, noise, and whether the two together still compile — every change and pair is also run through a compile check, which builds the module and compiles the touched packages' tests without running them. `eval -scorers compile` prices from that record: the type-checker baseline a model has to beat, perfect on structural conflicts and blind to behavior ones.

Grading puts both sides in one batch, labeled with the combined outcome, and grades each side alone separately. Comparing the two separates a scorer that sees the interaction from one that reacts to a side that merely looks risky. With `-ahead`, each side is also scored with the other queued ahead of it, which is the framing a live queue would supply. A batch holds one request in the orchestrator today, so a scorer is never shown both sides of a pair in a live queue; the pair slice measures what it could do with that context, which is what request grouping or context from the batches ahead would supply.

## Known limits

- Verification runs the touched packages' tests, not their dependents', so a change that breaks only a downstream package's tests is labeled as landing. A broader verify command trades generation time for that.
- Mutations are syntactic. They do not produce semantic conflicts across packages, concurrency bugs, or performance regressions, and a model could in principle learn their shapes; holding out a seed or a mutator from any tuning guards against that.
- The `delete` pair family needs an unexported function nothing references, which a lint-clean seed does not have; on this repository it produces no pairs. Deleting an exported function with no in-repository caller would be the realistic form.
- A threaded context is passed along but not yet used inside the function, as in a first plumbing change; a repository that lints for unused parameters would flag it.
- Behavior conflicts are limited to functions of basic values and to one flipped or relaxed comparison; behavior behind interfaces, I/O or shared state is not probed.
- Sweeps are mechanical, which real large changes rarely are. They test whether a scorer can find one consequential line in a large diff, not whether it understands a large redesign.
