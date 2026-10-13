# Code-Aware Outcome Scoring

How a model-backed base scorer (`jev`) sees enough of the code to tell a batch that will fail from one that will pass, without sending the whole repository on every call.

**Status:** Tiers 1–3 are implemented: `submitqueue/extension/changediff` (git) returns each change's diff and, optionally, its touched files in full and references found in the tree; the `jev.diff` scorer option sends them (`files`, `references`). Tier 4 is proposed.

See [outcome-scorer.md](outcome-scorer.md) for where a base price sits under the evidence layer, and [speculation.md](speculation.md) for how that price ranks speculation paths.

## Problem

A base scorer is only worth its latency and cost if it separates failing batches from passing ones. Given file names and line counts alone, a decision model has nothing a size `heuristic` does not: in local runs it priced every batch at roughly the same confident value and, on a serialized queue with real failures, spent more builds than a flat 0.5 because it bet heavily on optimistic paths that one early failure invalidated.

Given each change's diff, the same model separated blatant failures (a compile error, a test asserting the wrong value) from correct changes almost perfectly, and the queue reached the minimum possible number of builds at a constrained build budget. The question this document answers is how far beyond the diff the model needs to see, and how to supply that without the costs of sending the repository.

## Why not the whole repository

- **Size.** A repository is megabytes to gigabytes. A System One model is built for a small structured state and an answer in about 100ms; a repository per call either does not fit or turns a fast call into a slow, expensive one.
- **No reusable server-side context.** The System One request is `state` plus `questions`. There is no upload-once-and-reference mechanism, and whether the backend caches repeated prompt prefixes is not exposed through the proxy.
- **Staleness.** The target moves with every land, so any snapshot is keyed by target commit and must be rebuilt as the queue advances.
- **Egress.** Sending the codebase off-network is a larger decision than sending diffs, which already requires sign-off.

The repository is already cached on our side: the orchestrator keeps a local copy for the git change provider, and the git differ reads from it. The design is to *select* from that copy per batch rather than to ship it.

## Context tiers

Each tier is an addition to the scorer state, bounded by a byte budget, computed once per batch and covered by the existing per-batch price cache.

1. **The change's diff** (implemented). The unified diff of each change, measured from the same baseline as its numstat, capped at 64KB per batch and cut at a line boundary with a truncation flag. Catches failures visible in the changed lines.
2. **Touched files in full** (implemented, `files`). The post-change contents of each touched file, not just the hunks. Catches failures that depend on code adjacent to the change in the same file, such as a new call to a function whose existing definition takes different arguments.
3. **References** (implemented, `references`). For identifiers the diff removes, renames or changes the signature of, `git grep` the local copy at the change's head for remaining references outside the diff and include those snippets. Catches the cross-file break: a signature change whose callers, untouched by the change, no longer compile. The reverse direction is included too: the definitions of functions the change calls but does not define, so a call into existing code is not mistaken for a call to nothing. Both follow Go scoping — a bare call resolves only in the caller's package directory, and a use from another package is qualified — because an unscoped search returns same-named symbols from unrelated packages, which in testing crowded out the real caller and made existing callees look unreachable.
4. **Batches ahead.** The diffs of the batches this batch is being built on top of. Catches interaction failures: a change correct on its own that calls a function a batch ahead of it renames.

Tier 4 is not a property of the batch alone, and that is a contract change rather than an addition. The scorer today prices "this batch succeeds", independent of path, and the generator multiplies those prices assuming independence. An interaction failure is only likely *given* a particular dependency succeeding. Supporting it means pricing a batch conditional on the batches ahead — for example a `Score` variant taking the path's assumptions, or a pairwise interaction term the generator applies on top of the per-batch price — and that belongs in a revision of [outcome-scorer.md](outcome-scorer.md) when tier 4 is built.

## What no code tier can see

Failures with no cause in the code — flaky tests, infrastructure outages, quota or environment drift — are invisible to any amount of code context. They are what the evidence layer's `pathFailed` factor and the speculator's failure-assumption paths absorb. A code-aware scorer should not be expected to price them, and an evaluation should report them separately so they do not read as model misses.

## Budgets and caching

Every tier shares one per-batch byte budget, spent in tier order so the diff is never displaced by context. The price cache is keyed by queue and batch ID, and a batch's contents are fixed at creation, so context is gathered at most once per batch however many speculation runs score it. Tiers 2–3 read the change's head; tier 4 also depends on which batches are ahead, which changes as the queue moves and therefore shortens how long a cached price holds.

## Placement

Tiers 2 and 3 are further reads against the same local copy, so they belong beside the git differ — either a richer `changediff` git implementation or a separate context extension with the same input contract (`change.Change` in, per-URI content out). Tier 4 composes existing diffs and needs the batches ahead, which the scorer can resolve through `changeset.Resolver` from the path set it is already handed.

## Security

Every tier sends code to the LiteLLM proxy and the provider behind it. Each is opt-in per queue in configuration, defaults off, and should be enabled on a repository only after sign-off for that repository.

## Evaluation

Each tier is justified by the failures it newly catches without new false alarms. The local evaluation uses a serialized ten-change workload on the git provider with the fake build runner, where failures are injected by a branch-name marker that is hidden from the model, and each failure is chosen to need a specific tier:

| Change | Outcome | Needs |
|---|---|---|
| Off-by-one loop with a test that catches it | fails | tier 1 |
| Signature change whose caller is outside the diff | fails | tier 3 |
| New call to a function a batch ahead renames | fails | tier 4 |
| Comment-only change that fails anyway | fails | nothing can see it |
| Large correct addition; new BUILD file; correct rename; deleted test; error wrapping; small helper | pass | decoys for false alarms |

Reported per run: per-change price, separation between failing and passing prices, builds dispatched, builds on false assumptions, and time to land, at build budgets 4 and 2 against a flat heuristic and the model without code. Beyond the synthetic workload, the next level replays real change history with recorded CI outcomes, which measures calibration on failures nobody designed.

## Findings with tier 1

Over six runs per arm of the workload above (budgets 4 and 2):

- **The diff-visible failure is caught** (mean price 0.02). Correct changes that are large, rename symbols with their callers, or wrap errors are priced 0.94–0.97.
- **The cross-file signature break is missed** (0.85). The diff shows a correct-looking signature change; the broken caller is outside it. This is the case tier 3 exists for.
- **The interaction failure looks caught (0.07), but only through a shortcut.** A correct change that calls a function defined outside the diff is priced the same (0.08). The model penalizes calls it cannot resolve, whether or not they are broken. Tier 2–3 context that shows the callee exists is the fix for the false alarm; tier 4 is still the only way to catch the real interaction.
- **A change that deletes a test is a noisy false alarm** (0.20–0.72), with or without the diff.
- **Infrastructure flakiness is invisible**, as expected (0.97).
- Without code the model separates nothing: its failing changes are priced no lower than its passing ones, and the deleted-test decoy drags it below chance (mean AUC 0.29 against 0.67 with the diff).

The queue-level effect follows the separation. With blatant failures, tier 1 reached the minimum number of builds at budget 2. With these subtler ones it roughly ties the flat heuristic at budget 2 and spends more builds at budget 4, because its two confident false alarms send speculation down "fails" paths that then break. Tier 1 is necessary but not sufficient; tiers 2–3 are needed before the price is trustworthy on changes that call into the rest of the codebase, which is most of them.

## Findings with tiers 2–3

The same workload over four arms (no model, model without code, tier 1, tiers 1–3), two rounds at each of budgets 4 and 2, prices averaged over the four runs per arm:

| Change | Truth | No code | Tier 1 | Tiers 1–3 |
|---|---|---|---|---|
| Off-by-one, visible in the diff | fails | 0.87 | 0.03 | 0.02 |
| Signature change, caller outside the diff | fails | 0.82 | 0.88 | 0.91 |
| Calls a function a batch ahead renames | fails | 0.83 | 0.04 | 0.11 |
| Flaky infrastructure | fails | 0.81 | 0.93 | 0.96 |
| Calls a function defined outside the diff | passes | 0.86 | 0.06 | 0.85 |
| Deletes a test | passes | 0.18 | 0.55 | 0.71 |
| Large addition, BUILD file, rename with callers, error wrapping | pass | 0.71–0.81 | 0.84–0.97 | 0.89–0.98 |
| Separation (AUC) | | 0.27 | 0.71 | 0.79 |

- **Definitions fix the out-of-diff false alarm**: the correct change calling an existing function moves from 0.06 to 0.85, and passing changes rise across the board.
- **The caller does not flip the signature break.** The model is handed the one untouched line still calling the old signature and still prices the change at 0.91. Providing the evidence is not sufficient; the fast model does not carry out the cross-reference. Candidate next steps are an explicit structural check that compares call arity against the new signature before asking, or a question that names the comparison (for example a second noul question per caller).
- **The interaction failure is still priced low with its callee's definition in view**, so whatever the model keys on there, it is not the missing definition; tier 4 remains the only principled way to see it.
- Queue cost follows: tiers 1–3 spent the fewest builds at budget 4 (22.0 against 22.5 for no model) and was fastest first-accept-to-last-land at both budgets, though at budget 2 tier 1 alone spent half a build fewer.
