# Context-reset delta benchmark

The context-reset benchmark measures the storage and local CPU trade-offs of
session-tree delta encoding without involving a model provider. Model latency
would dominate these operations and make the comparison less repeatable.

The benchmark constructs valid provider-neutral transcripts with retention-like
sparse rewrites at three approximate payload sizes: 64 KiB, 256 KiB, and 1 MiB.
For every size it builds equivalent trees using:

- `snapshot`: the legacy full `messages` context-reset representation;
- `delta`: the current `context_delta` representation selected by production
  encoding logic.

Both trees are replayed and checked against the same expected transcript before
timing begins.

## Run

Run the full benchmark with allocation reporting:

```sh
go test -run '^$' \
  -bench '^BenchmarkContextReset' \
  -benchmem -benchtime=1s -count=5 \
  ./internal/session
```

For a quick local sample:

```sh
go test -run '^$' \
  -bench '^BenchmarkContextReset.*/256KiB/' \
  -benchmem -benchtime=250ms -count=1 \
  ./internal/session
```

Use the same Go version, machine, power mode, and `GOMAXPROCS` when comparing
revisions. Run separately built worktrees rather than modifying the active
workspace, save each command's output, and compare medians across the repeated
samples.

## Measurements

`BenchmarkContextResetEncode` measures construction plus JSON encoding of one
reset entry. The delta case includes production's snapshot-versus-delta size
selection. Reported columns include:

- `ns/op`, `B/op`, and `allocs/op` from Go's benchmark framework;
- `tree-bytes/op`, the serialized reset-entry size;
- `storage-saved-%`, the delta entry's reduction from its equivalent snapshot.

`BenchmarkContextResetReplay` calls `Tree.BuildContext` on equivalent in-memory
trees. It captures the replay cost of loading unchanged ancestors and applying
and validating a delta versus starting from a newer full-snapshot anchor.
`tree-bytes/op` is the complete serialized tree size associated with the case;
it is context for the timing rather than bytes rewritten by each replay.

`BenchmarkContextResetLoad` repeatedly calls `LoadTree` on saved equivalent
trees. This includes cached filesystem reads, NDJSON decoding, validation, and
active-context materialization. `tree-bytes/op` is the physical `tree.ndjson`
size read by the case.

Expected trade-off: sparse deltas should substantially reduce serialized bytes.
Delta construction or replay can still use more CPU than a full snapshot because
it fingerprints the base and result and validates the reconstructed transcript.
Treat storage reduction and CPU results as separate promotion dimensions.

## Live-session measurement

Delta encoding itself is deterministic and provider-neutral, so the benchmark
above is the primary isolated performance test. To measure storage on live model
workloads, run the provider-backed retention matrix described in
[`flowbench.md`](flowbench.md#retention-policy-benchmark), then analyze its
session directories with `harness session analyze`. Relevant analyzer fields are
context-reset snapshot/delta counts and bytes plus physical session-tree bytes.
Live wall time is not an isolated delta metric because provider latency dominates
it.
