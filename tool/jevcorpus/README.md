# jevcorpus

Builds a synthetic corpus of safe and unsafe Go changes with build-verified labels, and grades outcome scorers against it. Benchmarks only. See [doc/rfc/submitqueue/synthetic-corpus.md](../../doc/rfc/submitqueue/synthetic-corpus.md) for the design.

## generate

`jevcorpus generate` clones a seed repository, runs the tests under `-pkgs` once at `-base`, and keeps the packages that pass as mutation targets. It then plans `-count` changes over the S, M, L and XL tiers in real-world proportions (`-tier-mix`, default `S=50,M=40,L=7,XL=3`, with XL capped by `-max-xl`), designing `-unsafe-ratio` of each tier to fail. Each change is built from the `mutate` package's mutators on a clean base, verified with `-verify` (run at the module root with the touched packages in `$PKGS`), and committed as `corpus/c-NNNN`. A change whose outcome contradicts its intent is rebuilt, up to a few tries, unless `-keep-mismatch` is set. A slot that cannot be filled is skipped and logged; L and XL slots need a seed with hundreds of candidate files.

With `-pairs`, it also makes that many pairs, split by `-unsafe-pair-ratio`: two changes on the same base, in disjoint files, each verified to land alone and then verified together. A pair whose combination contradicts its design, or whose side fails alone, is rebuilt. With `-behavior-pairs`, it also makes pairs whose conflict is in behavior: A changes one comparison in a function in a way the package's tests miss, and B adds a table test of that function whose expected values come from probing the base, run in throwaway tests in the work tree. Either kind of pair may carry unrelated noise edits on each side. Both sides are ordinary `corpus/c-NNNN` branches; the conflict is recorded as `fails_with` in the dataset and in the manifest's `pairs`. Every failing change and pair is also run through `-compile-check`, so the manifest records whether a type-checker alone would have caught it.

It writes the dataset at `-out`, the manifest beside it as `<name>.manifest.json`, and a bare sandbox at `-sandbox` holding the base as `main` and one branch per kept change. One seed, base and flag set always plan the same corpus, though verification time and flaky tests can change which attempts are kept.

Example, with a replay candidate as the seed:

```bash
git clone --quiet https://github.com/jaegertracing/jaeger /tmp/seed/jaeger
bazel run //tool/jevcorpus -- generate -repo /tmp/seed/jaeger -pkgs "./internal/... ./cmd/..." \
  -count 80 -out /tmp/corpus/jaeger.json -sandbox /tmp/corpus/sandbox.git
```

## eval

`jevcorpus eval` scores one batch per change, one batch per pair holding both sides and labeled with their combined outcome, plus `-batches` sampled batches of `-batch-size` changes, with each scorer in `-scorers`. `size` prices by lines changed; `compile` prices from the recorded compile check, the type-checker baseline; `jev` is the real scorer over the git change and diff providers, reading the proxy root and key from `LITE_URL` and `LITE_KEY` by default, with no fallback or cache. `-files` and `-references` match the scorer's `jev.diff` options, and `-path /typesafe/v1/systemone` selects the TypeSafe pass-through. With `-ahead`, each side of a pair is also scored with the other side queued ahead of it, through the `jev` scorer's `Ahead` option, under `ahead`, per mutator and family, and per direction. Pair sides are graded alone under `pair member alone` and kept out of the single-change slices; pairs are graded under `pairs`, per mutator, and per family, where a conflicting mutator and its compatible twin share a slice. `-max-ahead-bytes` sets the changes-ahead budget. `-expect-model` fails any answer whose response names a different model than the one given (for example `jev-1.13.0`), and `-cache <dir>` keeps every successful response keyed by the exact request bytes, so rerunning an unchanged request is free and any change to the question, state or budgets is a fresh call. It prints a report per scorer and slice, with each slice's count of batches that failed to score — those are left out of its metrics, so a slice whose failures share a cause is graded on a biased sample — and a line naming the models that answered, the cache hits, and the largest answered and smallest failed request sizes, which locate a backend's request size limit. With `-out` it writes every row, with its model calls, and every report as JSON.

Sending diffs sends the seed's code to the proxy and its model provider.

## In a queue

The dataset is in the replay format, so a corpus runs through the git compose overlay exactly as a replay epoch does: `replaybench uris -dataset <corpus>` lists the change URIs, `buildRunner: {type: replay, dataset: <corpus>}` answers builds from the labels, and `oracle` reads them as the ceiling. See [doc/rfc/submitqueue/replay-benchmark.md](../../doc/rfc/submitqueue/replay-benchmark.md).
