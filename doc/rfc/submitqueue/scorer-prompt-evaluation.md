# Scorer Prompt Evaluation and Optimization

How the `jev` scorer's request — its questions, the layout of its state and the context it carries — is designed, tested and changed, given that small changes to it move prices further than the model's own run-to-run noise.

**Status:** Phase 1 is partly implemented: criteria are serialized in a fixed order, the scorer can require a named model version (`ExpectModel`, `jevcorpus eval -expect-model`), every call's request size is recorded, and `jevcorpus eval -cache` answers repeated requests from disk. Sizing the default budgets under the backend's limit is open. Later phases are proposed.

See [code-aware-scoring.md](code-aware-scoring.md) for what the state carries, [synthetic-corpus.md](synthetic-corpus.md) for the labeled corpus, and [replay-benchmark.md](replay-benchmark.md) for real history.

## Problem

The request is the model's whole program. On the pair corpus, rewording the question moved borderline prices by up to 0.7 while identical requests repeated within 0.1, and published work on the same model agrees: rewording alone spans 24 accuracy points, a colon combined with a different JSON key order took false acceptance from 0 to 26%, option names outweigh their definitions, and accuracy falls as unrelated state grows. The current request was tuned by hand against 12 pairs, which is too few to tell a better wording from a luckier one, and nothing stops a wording that wins on the corpus from losing on real changes.

So a request change has to be treated like a model change: proposed as data, measured on held-out labeled changes with stated uncertainty, checked for the failure modes we already know about, and promoted only through a gate.

## Decisions

### The request is a versioned spec, not code

A **prompt spec** is a JSON document holding everything the model sees beyond the batch itself: the questions (each a noul with structured `instructions` and `criteria`), the order of state fields, which context sections are sent and their byte budgets, how answers combine into one price, and the calibration applied to it. Specs live in the repository with an ID; the scorer embeds the promoted one and logs its ID with every price, and `jevcorpus eval -prompt` takes a candidate file. This is the one seam both consumers need — the shipped scorer and the evaluator — and it makes every variant a diff of data that the gate can grade, instead of a code change.

The serialized request is canonical: the spec fixes field and criteria order explicitly rather than inheriting Go's sorted map keys (today `false` is always sent before `true`). The model version is pinned in the spec (`jev-1.13.0`, not an alias), and a response from another version fails the evaluation run.

### Decompose, compute facts in code, combine in code

Following the model vendor's guidance and its published workflows, the target shape is a set of narrow nouls over a state that already carries what code can compute, combined by a small fitted model:

- **Facts** are computed before asking and placed first in the state as raw values, not bucketed labels: whether the changed packages typecheck, `apidiff`-style incompatible API changes, and per caller whether its call site matches the new declaration. A question that a fact settles is not asked. The model is weak at arity, counting and multi-hop comparison, and was shown the broken caller and still missed it.
- **Questions** each judge one property and point at state by backticked path — a call site against a declaration, whether a test's expectation changed, whether the batch relies on something `changes_ahead` alters, whether the change only deletes tests. Questions in one request run in parallel at flat latency and do not see each other.
- **Combination** is a logistic regression over the question probabilities and the facts, fitted on the development split and stored in the spec. It is also where calibration happens; there is no separate calibration stage to keep in sync.

The single `batch_lands` question remains a valid spec, and the first comparison the gate runs is decomposed against it.

### Evaluation protocol

- **Splits.** The corpus is split by seed repository and by mutator family into *development* and *sealed* sets. Optimizers and humans iterate on development only; the sealed set is scored once per candidate at the gate. Holding out whole mutators guards against a wording that has learned a mutator's shape.
- **Cache.** Every response is cached by a hash of the exact request bytes and the model version, with an explicit repeat index for deliberate re-asks. Regrading is free, and a cache miss is the only thing that spends money.
- **Metrics.** AUC, Brier, log loss and capture of failures at the top 5/10/30% of risk, each with a paired bootstrap interval over the same rows; ECE beside the noise floor of a perfectly calibrated predictor on the same sample; and the queue's own cost from a corpus queue run. Baselines on every report: constant, `size`, `compile`, and the promoted spec.
- **Noise floors.** A test–retest run of identical requests gives the floor every invariance threshold is measured against. Slice sizes are chosen from a probe of the metric's variance, so a comparison is sized to the difference it has to detect.

### Suites beyond accuracy

Each runs on development changes and needs no new labels except where noted.

- **Invariance:** reorder state fields, reorder criteria, reorder changes, reformat whitespace, rename an unrelated identifier. The mean price shift and the share of prices crossing 0.5 must stay within the test–retest floor.
- **Negation:** the spec's main question asked in its explicit negative must return close to the complement.
- **Dilution:** the same change with 0, 8, 32 and 64KB of unrelated context. Sets the context budgets from measurement rather than from the token limit.
- **Adversarial (labeled):** author-controlled text — comments and commit messages — asserting that callers were updated or that the build passes, added to failing changes; the reverse added to passing ones. Untrusted text is kept in its own fields, and the suite measures whether that is enough.
- **Size:** every request's reported input tokens must fit the model's state limit (32k tokens for the state and the longest question on `jev-1.13.0`). The default byte budgets can exceed it today, and what the service does past it is undocumented. Where a proxy does not report tokens, request bytes stand in: on the proxy this was developed against, requests failed above about 24KB of Go code, and a run with full files and references lost every M-tier batch to that limit, leaving the reported AUC computed over small changes only. The same proxy answered from a model other than the one requested, which is why the model is checked on every response rather than assumed from the request.

### The gate

A candidate spec — whether written by hand or by an optimizer — is promoted only if, on the sealed set, its AUC improves on the promoted spec with a paired interval excluding zero, no slice regresses beyond its interval, the invariance and negation suites pass, every request fits the token limit, and it does no worse on the replay benchmark. The sealed set needs at least 30 failing and 30 passing changes for the gate to run; smaller sets produce a pilot report, not a promotion. When the model version changes, the promoted spec is re-graded and its combination refitted before the new version is pinned.

### Optimizer

An optimizer is a search over spec text fields scored by the evaluator. The first is GEPA (`optimize_anything`, a dict of text components such as each question's instructions and criteria) run as an out-of-tree harness that calls `jevcorpus eval` on the development split. Its objective is pairwise ranking — one row per failing–passing pair, so the score is AUC — under a fixed call budget, with accept/reject decided on one fixed slice so each comparison is paired. Its feedback to the reflecting model names the misses per slice and never the label of an individual row. Only the winning spec and its graded report enter the repository, through the gate.

Human iteration uses the same loop with a person as the proposer; the gate does not care who wrote the spec.

## Rejected alternatives

- **Keep tuning by hand on the whole corpus.** With no held-out set every improvement is in-sample, and at 12 pairs the noise is larger than the effects being chosen between.
- **Prompt variants as Go code.** Each variant would be a code change, and the evaluator could not try one without a build; a spec is the smallest thing both consumers share.
- **DSPy, MIPROv2 and similar program optimizers.** They optimize generative LM programs; a typed noul has no output to bootstrap demonstrations from, and the extra framework buys nothing over GEPA's text-component search.
- **Tuning on an open-weight model of the same shape.** Their context windows are far below our diffs, and wording effects are model-specific, so a wording chosen on them does not transfer.
- **An LLM judge or an LLM cascade on uncertain prices.** Labels come from builds, so no judge is needed, and on this model's confident errors LLM judges repeat its answer about 96% of the time; a cascade lowers cost but adds little accuracy.
- **Averaging repeated calls.** Re-asking improves calibration but not ranking. Calibration belongs in the fitted combination, which is free at request time.
- **A separate calibration stage.** Calibration fitted apart from the combination drifts from it whenever either is refitted.

## Phases

1. **Request hygiene:** canonical serialization, pinned model, input tokens recorded, budgets sized under the limit, response cache.
2. **Specs and protocol:** the prompt spec, splits, intervals and noise floors in `grade`, and the invariance, negation, dilution and size suites.
3. **Facts and decomposition:** computed facts, the decomposed question set and the fitted combination, compared with `batch_lands` through the gate.
4. **Optimizer:** the GEPA harness on the decomposed spec.
5. **Real outcomes:** the gate's replay check on a larger real-history set, and price-against-outcome monitoring for the promoted spec in a corpus queue run.

## References

TypedBench (arXiv 2610.11392) for wording ranges and calibration noise floors; Adversarial Cues (2610.11436) for layout sensitivity; Type-Safe Is Not Error-Free (2609.26758) for option names; HVAC fault diagnosis (2610.09937) for facts computed in code; Jev vs LLM judges (2609.29769) for correlated errors; Readout Stability (2610.07716) for re-asking; Ranking-PE (2609.40361) and BudgetAPO (2610.05671) for the optimizer objective and budget; the vendor's model documentation for token limits and context rot; its published workflow evals for the facts–questions–gates shape.
