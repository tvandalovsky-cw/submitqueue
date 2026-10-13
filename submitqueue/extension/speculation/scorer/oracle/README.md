# oracle

`oracle` is a content `Scorer` that already knows each batch's outcome: it prices a batch that will fail at 0.01 and one that will pass at 0.99. It is the ceiling for benchmarking scorers — the fewest builds and shortest time to land speculation can reach on a workload whose outcomes are recorded — and must never be configured on a production queue.

Outcomes come from a `Truth`. `MarkerTruth` reads the fake build runner's `sq-fake=build-fail` URI marker, so on a fake-build workload the oracle agrees with the runner by construction. A replayed workload supplies a `Truth` backed by its recorded CI results. A batch fails if any of its changes fails. A batch whose outcome is not recorded takes the fallback scorer's price, or an error without one.

The prices stop short of 0 and 1 because the `bestfirst` generator ranks paths by summed log-probability, which a certain price would make infinite. Sitting under `evidence`, the oracle's price is still revised by build results; that only pushes it further toward the outcome it already predicted.

In `profiles.yaml`, under a scorer `base`: `{type: oracle}`, optionally with `fallback`.
