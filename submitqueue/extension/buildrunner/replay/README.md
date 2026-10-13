# replay

A `BuildRunner` that answers every build from a recorded CI history, and an `oracle.Truth` reading the same record. Benchmarks only: it never runs anything, and must never configure a production queue. See [doc/rfc/submitqueue/replay-benchmark.md](../../../../doc/rfc/submitqueue/replay-benchmark.md).

A dataset (`Dataset`, written by `tool/replaybench`) lists one repository's merge-queue attempts with their recorded outcome and duration, each rebuilt as a sandbox commit (`ReplaySHA`). The runner looks changes up by the commit SHA in their `git://` URI. A build fails if any change in its base or head is recorded as failing, and finishes after the longest recorded duration among them divided by `TimeScale`. Unrecorded changes pass instantly. The outcome and due time are encoded in the build ID, so `Status` holds no state and any instance answers for any build.

`Truth` reports a batch failing if any of its changes is recorded as failing, and unknown if none is recorded, so an `oracle` configured with the same dataset agrees with the runner on every head.

Configured in the orchestrator's `profiles.yaml` as `buildRunner: {type: replay, dataset: <path>, timeScale: <n>}` and, for the ceiling, `scorer: {base: {type: oracle, oracle: {dataset: <path>}}}`.
