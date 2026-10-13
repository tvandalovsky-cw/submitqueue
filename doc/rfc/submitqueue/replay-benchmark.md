# Replay Benchmark

Benchmarking outcome scorers against real merge-queue history from open-source repositories, instead of synthetic workloads whose failures we wrote ourselves.

**Status:** Implemented for repositories that use GitHub merge queue: `tool/replaybench` builds datasets and sandboxes, `submitqueue/extension/buildrunner/replay` answers builds from the record, and the `oracle` scorer can read the same record. Benchmarks only — none of it may configure a production queue.

See [code-aware-scoring.md](code-aware-scoring.md) for the scorers being compared and [outcome-scorer.md](outcome-scorer.md) for where a price is used.

## Why replay

A synthetic workload measures what its author anticipated. Real history supplies failures nobody designed — dependency bumps that break CI, refactors that collide, flaky infrastructure — at their real rate and with their real CI durations, which also makes time to land meaningful where a fake runner that finishes instantly does not.

## Source: GitHub merge queue

A repository using GitHub merge queue runs CI on a `merge_group` event for every queue attempt, on a commit GitHub builds as `gh-readonly-queue/<base>/pr-<n>-<parent>`. That run history is exactly a submit queue's record: which PR, tested on which queue head, with what result, for how long — including attempts that failed and never landed. The merge-group branches are deleted afterwards, but GitHub still serves their commits by SHA, so failed attempts are recoverable.

An attempt passes if every recorded run for its merge-group commit passed and fails if any failed. Groups whose runs were cancelled or unfinished have no outcome and are dropped: GitHub cancels the groups behind a failure, and those say nothing about their own PR.

## Rebuilding changes on one base

Upstream merge-group commits descend from queue heads the sandbox never has, so used as-is the change provider would measure each one against an ancient merge base and report every earlier PR as part of it. Each attempt is therefore rebuilt as one commit on a fixed base: the PR's own effect (merge-group commit minus its parent) three-way merged onto the base with `git merge-tree`. Every change then diverges from the sandbox's main at the base, and the queue lands them for real.

A long window drifts: later PRs touch code earlier ones changed after the base and no longer apply. Datasets are therefore split into short **epochs**, each rebuilt on the commit just before its first attempt and replayed as an independent run. Attempts that still conflict are kept in the dataset, marked skipped, and left out of the run.

## Answering builds

The `replay` build runner returns the recorded outcome after the recorded duration divided by a time scale. A build fails if any change in its base or head is recorded as failing, because a build containing a broken change is broken whatever path it was testing. The outcome and due time are encoded in the build ID, so the runner holds no state. The `oracle` scorer configured with the same dataset prices every recorded change at 0.99 or 0.01, giving the ceiling every other scorer is measured against.

## Known approximations

- **Attribution.** A merge-group commit contains the PRs queued ahead of it, so an upstream failure can belong to a PR ahead. Replay attributes it to the group's own PR.
- **Flakiness.** A failure whose exact change (same `git patch-id`) later passed for the same PR is marked `flaky_suspect`. These are recorded failures no code-based scorer could predict, and analyses should report them separately.
- **Selection.** Conflicting attempts are dropped, and they are disproportionately failures — PRs that needed rework against newer code. Epoch size trades kept attempts against how far each epoch is from upstream.
- **Context.** A rebuilt change's surrounding code is the epoch base, not the upstream queue head it was really tested on.

## Candidate repositories

Go repositories with merge-queue history, sampled when this was written: `jaegertracing/jaeger` (43 MB, about 24% of attempts fail), `open-telemetry/opentelemetry-collector` (73 MB, about 31%), `containerd/containerd` (138 MB, about 23%), `cilium/cilium` (565 MB, about 11%), and `pulumi/pulumi` (384 MB, about 49%, multi-language). Go keeps the reference heuristics in `changediff` applicable.

## Workflow

`replaybench fetch` lists a window's merge-group runs through the GitHub API (`GITHUB_TOKEN`) and writes a dataset; `split` cuts it into epochs; `materialize` clones or reuses an upstream mirror, fetches the commits no branch holds, rebuilds each attempt on the epoch base, marks flaky suspects, and creates a bare sandbox holding the base as `main` and one branch per attempt; `uris` prints the change URIs in queue order. A run points the git compose overlay at the sandbox (`SQ_GIT_SANDBOX_DIR`) and at a provider directory holding the dataset (`SQ_PROVIDER_CONFIG_DIR`), with `buildRunner: {type: replay, dataset: ...}`.
