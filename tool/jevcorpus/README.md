# jevcorpus

Builds a synthetic corpus of safe and unsafe Go changes with build-verified labels, and grades outcome scorers against it. Benchmarks only. See [doc/rfc/submitqueue/synthetic-corpus.md](../../doc/rfc/submitqueue/synthetic-corpus.md) for the design.

## generate

`jevcorpus generate` clones a seed repository, runs the tests under `-pkgs` once at `-base`, and keeps the packages that pass as mutation targets. It then plans `-count` changes split by `-unsafe-ratio` and spread evenly over the S, M, L and XL tiers. Each change is built from the `mutate` package's mutators on a clean base, verified with `-verify` (run at the module root with the touched packages in `$PKGS`), and committed as `corpus/c-NNNN`. A change whose outcome contradicts its intent is rebuilt, up to a few tries, unless `-keep-mismatch` is set. A slot that cannot be filled is skipped and logged; L and XL slots need a seed with hundreds of candidate files.

It writes the dataset at `-out`, the manifest beside it as `<name>.manifest.json`, and a bare sandbox at `-sandbox` holding the base as `main` and one branch per kept change. One seed, base and flag set always plan the same corpus, though verification time and flaky tests can change which attempts are kept.

Example, with a replay candidate as the seed:

```bash
git clone --quiet https://github.com/jaegertracing/jaeger /tmp/seed/jaeger
bazel run //tool/jevcorpus -- generate -repo /tmp/seed/jaeger -pkgs "./internal/... ./cmd/..." \
  -count 80 -out /tmp/corpus/jaeger.json -sandbox /tmp/corpus/sandbox.git
```

## eval

`jevcorpus eval` scores one batch per change, plus `-batches` sampled batches of `-batch-size` changes, with each scorer in `-scorers`. `size` prices by lines changed; `jev` is the real scorer over the git change and diff providers, reading the proxy root and key from `LITE_URL` and `LITE_KEY` by default, with no fallback or cache. `-files` and `-references` match the scorer's `jev.diff` options, and `-path /typesafe/v1/systemone` selects the TypeSafe pass-through. It prints a report per scorer and slice, and with `-out` writes every row and report as JSON.

Sending diffs sends the seed's code to the proxy and its model provider.

## In a queue

The dataset is in the replay format, so a corpus runs through the git compose overlay exactly as a replay epoch does: `replaybench uris -dataset <corpus>` lists the change URIs, `buildRunner: {type: replay, dataset: <corpus>}` answers builds from the labels, and `oracle` reads them as the ceiling. See [doc/rfc/submitqueue/replay-benchmark.md](../../doc/rfc/submitqueue/replay-benchmark.md).
