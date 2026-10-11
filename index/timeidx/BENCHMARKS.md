# TimeIdx before/after measurements

Baseline: `ae8fda19018d9ea76eed990a963aa40a8536a4b3` (merged GeoIdx PR #15).
After: the namespace-aware version 4 implementation in this change.
Machine: Apple M4 Pro, Go 1.27.0, darwin/arm64, 12 benchmark workers. Five samples per
comparison, `-benchtime=200ms`, **medians**, with benchmarks run sequentially
without correctness/race suites competing for CPU. Numbers are hot-cache
microbenchmarks, not cold-storage or service SLAs.

`workload_benchmark_test.go` uses only the pre-existing public API and was copied
unchanged into a detached baseline worktree. It uses 100K entries spread over a
synthetic year, identical numeric keys/IDs and result cardinalities in both
versions. Timestamp spacing is aligned to the old quantum to avoid comparing
different candidate counts. The new encoding's calendar conversion is tested
separately. Append cases are moving upserts toward newer times, with a bounded
periodic wrap so the old 32-bit encoding does not overflow. Historical cases
move the same 64 IDs to deterministic dispersed older times. They are NOT
no-op writes. Setup and caller-buffer allocation are outside timing.

## Comparable range/lookup workloads (100K base)

| Query | Matches | Before | After | Speedup | Before/after B/op; allocs/op |
|---|---:|---:|---:|---:|---|
| Exact timestamp hit | 1 | 66.42 ns | 14.57 ns | 4.56x | 0 / 0; 0 / 0 |
| Exact miss | 0 | 64.04 ns | 15.09 ns | 4.24x | 0 / 0; 0 / 0 |
| One hour | 12 | 89.73 ns | 15.52 ns | 5.78x | 0 / 0; 0 / 0 |
| One day | 274 | 619.2 ns | 43.53 ns | 14.22x | 0 / 0; 0 / 0 |
| One month | 8,220 | 16.799 us | 1.079 us | 15.57x | 0 / 0; 0 / 0 |
| One repeated time, spanning shards | 100,000 | 204.271 us | 17.822 us | 11.46x | 0 / 0; 0 / 0 |

Bulk-appending immutable ID-column slices removes the old per-record delta
membership checks. Both versions already support zero-allocation searches;
the change improves throughput, not an allocation guarantee absent before.

## Writes and reopen (100K base unless noted)

| Operation | Before | After | Before -> after B/op | Before -> after allocs/op |
|---|---:|---:|---:|---:|
| 64 newer-time moves, memory | 13.141 us | 4.528 us | 61,949 -> 6,928 | 0 -> 5 |
| 64 historical moves, memory | 210.265 us | 98.574 us | 97,727 -> 352,650 | 0 -> 377 |
| 64 newer-time moves, persistent sync | 3.793 ms | 8.001 ms | 51,347 -> 33,050 | 0 -> 31 |
| 64 historical moves, persistent sync | 3.754 ms | 9.436 ms | 78,307 -> 122,673 | 0 -> 319 |
| Clean reopen | 167.863 us | 499.799 us | 625,684 -> 81,696 | 107 -> 455 |
| Single persistent sync insert, growing initially empty index | 3.104 ms | 8.047 ms | 0 -> 2,233 | 0 -> 12 |

Important regressions/tradeoffs:

- **Sync writes are 2.1–2.6x slower here.** WAL acknowledgment needed one sync;
  the confirmed COW-before-return policy needs payload sync then root sync.
  Atomic batch visibility and complete committed snapshots are stronger than
  the old mutable-delta prefix visibility. This is not an across-the-board
  ingestion improvement. An embedded journal/group durability design was not
  selected by the user.
- Clean reopen is about 3x slower, despite roughly 7.7x fewer allocated bytes.
  The new loader validates directory/column checksums, global uniqueness and
  recoverable extent sets; smaller shards add descriptors. It does not silently
  skip correctness checks to match the old open time.
- Random memory writes allocate more bytes and objects because they publish
  replacement immutable shards immediately rather than staging mutable deltas.
  Persistent writes reuse merge buffers; allocation counts are per affected
  shard/metadata object, not per input record. Tiny shards reduce rewritten
  payload but increase directory/object overhead.
- Old reported `0 allocs/op` write counts are rounded amortized values including
  background worker allocations, **not zero B/op** or a universal no-allocation
  guarantee. Memory comparisons include that background work, not its complete
  drain time. Persistent baseline writes defer base compaction to checkpoint;
  the new API has already done selective compaction when it returns.

## Existing benchmarks retained (1M search fixture)

| Operation | Before | After | Comment |
|---|---:|---:|---|
| Narrow range | 105.5 ns | 18.17 ns | 0 B/op, 0 allocations in both |
| Medium range | 9.830 us | 0.413 us | 0 B/op, 0 allocations in both |
| Broad range | 577.616 us | 44.024 us | 0 B/op, 0 allocations in both |
| `BenchmarkUpdate` | 535.4 ns | 68.90 ns | This fixture repeats identical upserts; new code detects no-ops |

Do not extrapolate the no-op update result to real changed-key insertion cost.

## Shard-size evidence and default

`BenchmarkTimeShardSize` compares 100K entries with clustered 64-ID updates
and a 25K-result scan. A separate historical sweep spreads those IDs throughout
the time domain. Clustered/scan numbers are five-sample medians; historical
numbers are three-sample medians, all after the scalar membership optimization.

| Maximum payload | Clustered batch | 25K-result scan | Historical batch |
|---:|---:|---:|---:|
| 4 KiB | 4.707 us | 4.313 us | 94.335 us |
| 16 KiB | 7.971 us | 4.446 us | 310.449 us |
| 64 KiB | 15.850 us | 3.625 us | 374.641 us |
| 256 KiB | 49.579 us | 3.400 us | not sampled |
| 1 MiB | 346.510 us | 3.331 us | 358.962 us |

The 1 MiB clustered case straddles the initial balanced shard boundary, so it
rewrites two shards. It is not a universal 74x advantage for 4 KiB. The
historical sweep supplies the independent evidence: about 3.8x less CPU at
4 KiB than 1 MiB, at a cost of about 29% more time for this broad scan and larger
directories. Small batches and random historical imports are specified goals,
so **4 KiB is the measured initial default**, not an assumed GeoIdx constant.
Read-heavy/very-large namespaces can select larger `MaxShardBytes` explicitly.
Retained `BlockSize` selects its equivalent payload size when provided.

## New capabilities and storage counters (after only)

| Benchmark | Median result |
|---|---|
| Eight namespaces, concurrent durable 64-entry batches | 2.877 ms/batch aggregate; 3,018 B/op, 11 allocs/op |
| Selective 64-entry changed commit on 100K base | 7.994 ms; 26,488 logical bytes written; 26,112 allocated extent bytes |
| Same selective case, arena size | 4,194,304 bytes of logical sparse file capacity |
| Grow a 4 KiB arena while adding 1K entries | 7.562 ms; final file capacity 32,768 bytes |
| 5K-result search during durable commits | 566.8 ns/op; sampled p50 666 ns, p99 792 ns; median 27 concurrent commits/run |

There is no old namespace-manager baseline: that capability did not exist.
`BenchmarkNamespaceParallel` reports aggregate throughput per completed batch,
NOT per-request p99 latency. Its per-worker streams reuse IDs. During-commit
p50/p99 sample the first up to 4,096 queries and include clock overhead; reported
B/op includes concurrent writer allocations. They are not production tail
latency guarantees. Search itself is separately verified at zero allocations.

Logical bytes include new shard payloads, the complete directory and the
512-byte root envelope. Extent bytes include allocator-rounded shard/directory
allocations, not an assertion of physical device writes. File-B is sparse
logical capacity, not filesystem allocated blocks or live payload bytes.

The full-width payload is **16 bytes/entry versus the old 12 bytes/entry**
(4-byte marker plus 8-byte ID), a derived 33% increase before directory,
allocator rounding and retained-generation overhead. That buys exact timestamps
and the required historical coverage. The default initial sparse capacity is
4 MiB; it is not evidence that a small namespace physically consumes 4 MiB.

## Reproduction

```sh
# Run this identical public-API harness at baseline and after revisions:
go test ./index/timeidx -run '^$' -bench '^BenchmarkModernization$' \
  -benchmem -benchtime=200ms -count=5

# Retained baseline names:
go test ./index/timeidx -run '^$' \
  -bench '^(BenchmarkSearch|BenchmarkUpdate|BenchmarkApplyPersistentSyncSingle)$' \
  -benchmem -benchtime=200ms -count=5

# Architecture-specific after measurements:
go test ./index/timeidx -run '^$' \
  -bench '^(BenchmarkTimeShardSize|BenchmarkTimeHistoricalShardSize|BenchmarkTimeSelectiveCommit|BenchmarkNamespaceParallel|BenchmarkTimeGrowth|BenchmarkTimeSearchDuringCommit)$' \
  -benchmem -benchtime=200ms -count=5
```

The old private block/WAL matrix was replaced by the public-API comparison and
namespace/shard benchmarks; it cannot describe the new architecture unchanged.
Before declaring a production no-regression gate satisfied, repeat these on
target hardware at application scale, including representative namespace counts,
long-held readers, cold opens, update-ID distributions and durability requirements.
No SIMD or timestamp compression was added.
