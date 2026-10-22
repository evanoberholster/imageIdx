## Phase 3.0: reproducible baseline and instrumentation

This PR establishes the Phase 3 baseline before writer, planner, snapshot or recovery changes.

### Fixes

- `database.validateAddition` now checks every immutable namespace field independently. A simultaneous `ProjectionVersion` bump and `Config` change is rejected, including when combined with a namespace addition.
- Added regression tests for both same-count and addition bypasses.

### Baseline instrumentation

- Converted the addition from a `switch` to independent field checks so no later immutable-field mismatch can be hidden.
- Added commit→indexed latency percentiles via `internal/bench.Recorder`.
- Added mixed bitmap query latency percentiles.
- Added logical journal/store bytes per operation, heap and derived-file size probes.
- Added 1M startup/rebuild coverage and opt-in 10M coverage through `IMAGEIDX_BENCH_LARGE`.
- Kept logical `Stats()` counters explicitly separate from physical device I/O.

### Representative baseline

| Workload | journal B/op | store B/op |
|---|---:|---:|
| insert | 145.0 | 188.5 |
| rating | 63.0 | 106.5 |
| tags | 77.0 | 120.5 |
| GPS | 42.1 | 85.6 |
| unchanged | 27.0 | 70.5 |

| Workload | Result | Allocations |
|---|---:|---:|
| Commit+index batch 64 warm | 20.4 ms/op, 94.2 KiB | 510 |
| Journal encode scratch | 51.7 ns/op, 0 B | 0 |
| Journal encode allocated | 61.9 ns/op, 80 B | 1 |

Latency sample (rating, batch 1): commit→indexed p50 24.98 ms; bitmap query p50 208 ns, p95/p99 292 ns.

### Validation

- `go build ./...`
- `go test -count=1 ./...`
- `go test -race -count=1 ./...`
- `go vet ./...`
- `sigmap validate` — 90% indexed-file coverage

No writer/planner/recovery algorithm changes are included. Phase 3.1 will build on this baseline.
