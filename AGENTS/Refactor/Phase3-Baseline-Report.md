# Phase 3.0 Baseline Report

> **Historical baseline note (superseded for query semantics).** This report was
> recorded before Phase 3 planner/snapshot changes. Its statement that there is
> no composite read root is no longer current; PR #27 added lock-free query
> snapshots. The measured numbers remain useful as the Phase 3.0 baseline.

**Baseline:** `6fd50b7` (Phase 2 merged)
**Environment:** Go 1.27.0, darwin/arm64, Apple M4 Pro
**Scope:** reproducible instrumentation and baseline only; no planner, writer,
recovery or engine algorithm changes.

## Phase 2 closure

- PR #25 head `51bcbd8` CI: all Linux AMD64/ARM64 jobs passed.
- Phase 2 merge: `6fd50b7`.
- `database.validateAddition` previously stopped at the first differing field
  because it used a `switch`. A simultaneous `ProjectionVersion` bump and
  `Config` change could bypass the config check. The check is now independent
  per field, with tests for both same-count and namespace-addition cases.

## Instrumentation added

`database/workload_bench_test.go` already provides deterministic photographic
operations for insert, rating, tags, GPS, AI and unchanged projections at batch
sizes 1/16/32/64. Phase 3 adds:

- commit→indexed lag p50/p95/p99/p99.9/max via `internal/bench.Recorder`;
- mixed bitmap query latency percentiles;
- logical journal and canonical/store bytes per operation;
- heap and derived-file size probes;
- 1M startup/rebuild baseline and opt-in 10M via `IMAGEIDX_BENCH_LARGE`;
- explicit "logical counters, not device I/O" labels.

## Representative baseline

### Logical update bytes (36-byte photo projection, batch 64)

| Workload | journal B/op | store B/op |
|---|---:|---:|
| insert | 145.0 | 188.5 |
| rating | 63.0 | 106.5 |
| tags | 77.0 | 120.5 |
| GPS | 42.1 | 85.6 |
| unchanged | 27.0 | 70.5 |

### Latency sample (rating, batch 1, 1x seed run)

- commit→indexed: p50 24.98 ms (single sample)
- bitmap query: p50 208 ns, p95/p99 292 ns
- journal 63.0 B/op; store 138.0 B/op

Latency distributions require a longer separate run before being used as a
benchstat gate.

### Index write path

| Workload | Result | Allocations |
|---|---:|---:|
| Commit+index batch 64 warm | 20.4 ms/op, 94.2 KiB | 510 |
| Journal encode scratch | 51.7 ns/op, 0 B | 0 |
| Journal encode allocated | 61.9 ns/op, 80 B | 1 |

### Sparse SimHash scale (batch 64)

| Live IDs | pHash search | PDQ scalar | PDQ bulk | Hot apply |
|---:|---:|---:|---:|---:|
| 10K | 14.7 µs | 35.9 µs | 36.1 µs | 411 µs |
| 100K | 165 µs | 357 µs | 356 µs | 1.34 ms |
| 1M | 1.51 ms | 3.43 ms | 3.41 ms | 5.19 ms |

## Critical path observed

1. Pebble canonical+journal batch construction and sync.
2. Coordinator decode/diff/coalesce into family staging.
3. Serialized per-engine apply/flush; bitmap/time/geo/radix/simhash each has its
   own persistence and view publication.
4. SimHash sparse hot updates spend most time in durable segment/manifest
   syscalls; search scans a bounded base+overlay.
5. Queries currently load engine-specific views independently; there is no
   composite read root yet.

## Metric boundaries

- `DB.Stats()`, `store.Stats()` and `index.Stats()` are logical/application
  counters. They are not fsync counts, device writes or filesystem bytes.
- Derived file sizes come from `internal/bench.DiskBytes`.
- Heap is `runtime.MemStats.HeapAlloc` after GC.
- Pebble WAL/flush/compaction counters and actual device bytes are not yet
  exposed by the current APIs and are marked unavailable rather than estimated.

## Commands used

```sh
go test ./database -run '^$' -bench 'BenchmarkPhotoMetadataWorkloads' -benchmem
go test ./database -run '^$' -bench 'BenchmarkPhotoMetadataLatency' -benchmem
go test ./database -run '^$' -bench 'BenchmarkCommitAndIndex|BenchmarkStartupRebuild' -benchmem
go test ./index/simhash -run '^$' -bench 'BenchmarkSparseScale' -benchmem
```
