# timeidx benchmark evidence

Measured on Apple M4 Pro, Go 1.27.0, darwin/arm64, `GOMAXPROCS=12`, same local
filesystem, serial runs with no competing test/benchmark load. All figures are
medians of repeated hot-cache microbenchmark samples — **not** cold-storage,
p50/p95/p99 or service-level latency claims. Re-run on the target hardware and
workload before treating any number as a regression gate.

Where a baseline is shown it is the previous quantized/WAL implementation; the
comparison isolates the current synchronous-COW architecture and is not an
across-the-board ingestion improvement (see the write tradeoffs below).

## Search and range queries

100K-entry base, hot cache. All cases are **0 B/op and 0 allocs/op** with
caller-provided destination capacity.

| Query | Matches | Current | Previous | Speedup |
|---|---:|---:|---:|---:|
| Exact timestamp hit | 1 | 14.6 ns | 66.4 ns | 4.6× |
| Exact miss | 0 | 15.1 ns | 64.0 ns | 4.2× |
| One hour | 12 | 15.5 ns | 89.7 ns | 5.8× |
| One day | 274 | 43.5 ns | 619 ns | 14.2× |
| One month | 8,220 | 1.08 µs | 16.8 µs | 15.6× |
| One repeated time spanning shards | 100,000 | 17.8 µs | 204 µs | 11.5× |

1M-entry fixture (retained baseline names):

| Query | Current | Previous |
|---|---:|---:|
| Narrow range | 18.2 ns | 105.5 ns |
| Medium range | 0.41 µs | 9.83 µs |
| Broad range | 44.0 µs | 577.6 µs |

Bulk-appending immutable ID-column slices removes per-record delta membership
checks. Both implementations already supported zero-allocation searches, so this
improves throughput, not an allocation guarantee that was previously absent.

## Writes and reopen

100K base unless noted. Final interleaved ten-process A/B vs the prior
implementation; `benchstat` for the p-values.

| Operation | Previous | Current | p | B/op | allocs/op |
|---|---:|---:|---:|---:|---:|
| 64 historical moves, memory | 93.3 µs | 86.6 µs | .001 | ~353 KB | 252 |
| 64 newer-time moves, memory | 4.5 µs | ~4.5 µs | — | 7,440 B | 4 |
| 64 durable appends | 8.18 ms | 8.12 ms | .631 | ~18.7 KB | 11 |
| 64 historical durable moves | 9.96 ms | 10.09 ms | .315 | ~98.5 KB | 291 |
| 10M sequential single mutation | 19.49 ms | 18.38 ms | .052 | 1,663,888 B | 11 |
| Clean reopen | 472.9 µs | 474.1 µs | .353 | ~67.3 KB | 436 |
| Day search | 41.56 ns | 42.12 ns | <.001 | 0 | 0 |

**Tradeoffs, stated explicitly:**

- A confirmed persistent `Apply` syncs payload and root (two barriers) before
  returning, so durable writes are slower than the old one-sync WAL
  acknowledgment. Atomic batch visibility and complete committed snapshots are
  stronger than prefix visibility.
- Random/historical memory writes allocate more bytes than the old staged deltas
  because they publish replacement immutable shards immediately; persistent
  writes reuse merge buffers, so allocation counts track affected shards, not
  input records.
- Reopen validates directory/column checksums, global uniqueness and recoverable
  extent sets; it does not skip correctness checks to match the old open time.
- The day-search drift is below the 5% acceptance threshold and remains
  zero-allocation; search code did not change.
- Old `0 allocs/op` write counts were rounded amortized values including
  background workers, not zero bytes or a universal guarantee.

## Shard size

`BenchmarkTimeShardSize` compares 100K clustered 64-ID updates and a 25K-result
scan; a separate sweep spreads the same IDs historically. Ten preparation samples
and three-to-five matrix samples.

| Max payload | Clustered batch | 25K scan | Historical batch |
|---:|---:|---:|---:|
| 4 KiB | 4.7 µs | 4.31 µs | 94.3 µs |
| 16 KiB | 8.0 µs | 4.45 µs | 310 µs |
| 64 KiB | 15.9 µs | 3.63 µs | 375 µs |
| 256 KiB | 49.6 µs | 3.40 µs | — |
| 1 MiB | 347 µs | 3.33 µs | 359 µs |

At 10M rows, random-ID historical-commit write volume and wide-search cost split
the decision; no single bound dominates:

| Bound | 1M rewrite bytes | 10M rewrite bytes | 10M wide search (2.5M IDs) | 10M directory bytes / entries |
|---:|---:|---:|---:|---:|
| 4 KiB | 478,232 | 2,446,960 | 1.22 ms | 2,187,752 / 39,067 |
| 16 KiB | 1,103,392 | 1,596,024 | 0.65 ms | 546,952 / 9,767 |
| 64 KiB | 4,160,704 | 4,330,600 | 0.46 ms | 136,808 / 2,443 |
| 256 KiB | 8,003,984 | 8,152,624 | 0.39 ms | 34,272 / 612 |
| 1 MiB | 8,001,408 | 9,420,840 | 0.35 ms | 8,568 / 153 |

Wide searches remain 0 B/op / 0 allocs/op. **4 KiB remains the default** because
small and historical batches are the specified goal and 4 KiB has the lowest
scattered-update write volume; larger bounds favor wide reads and large
namespaces. `MaxShardBytes` stays configurable (16 KiB is worth evaluating for
10M-row namespaces); there is no adaptive sizing.

## Random-ID scaling

Ten preparation samples; three-sample changed durable commits. Tuple values are
batches **1 / 8 / 32 / 64**.

| Rows | Prepare (ms) | Durable commit (ms) | Prepare bytes / allocs |
|---:|---|---:|---|
| 1M | 2.91 / 3.17 / 1.83 / 2.00 | 12.23 / 13.93 / 13.44 / 14.53 | 32,864 / 33,536 / 35,840 / 38,912 B; 2 / 9 / 33 / 65 |
| 10M | 12.04 / 23.90 / 19.42 / 19.76 | 25.38 / 37.77 / 32.47 / 34.44 | 319,584 / 320,256 / 322,560 / 325,632 B; 2 / 9 / 33 / 65 |

Detection inspects approximately all IDs at 10M (~39,063 shards at 4 KiB) but
rewrites only the affected shards, so random-ID detection is explicitly
**O(records)** and directory publication is O(shards). At 10M this is roughly
1.8K mutations/s in explicit 64-record batches on this machine — useful batch
throughput, not a QPS or tail-latency guarantee. There is no reverse index.

10M reopen medians are ~1.36 s across all five bounds; sorting IDs for
duplicate validation dominates and leaves temporary O(records) memory.

## Directory and commit-phase costs

10M rows / 4 KiB, ten phase samples (diagnostics, not an additive commit model):

| Phase | Median | B/op / allocs/op |
|---|---:|---:|
| Allocate/free directory | 18.6 µs | 0 / 0 |
| Encode directory (no clear pass) | 386 µs | 0 / 0 |
| Directory CRC32C | 189.5 µs | 0 / 0 |
| Union of sorted retained extent lists | 185.9 µs | 630,784 / 1 |
| Allocator reconstruction | 616 µs | 73,824 / 2 |
| Dirty unpublished directory, one real sync | 8.04 ms | 0 / 0 |

A 10M-row directory is ~2.19 MB per root and is rewritten per committed
generation, so a one-row change pays that directory cost even though it does not
rewrite record columns. Flat-directory O(shards) write amplification and one
dirty-directory sync are the largest remaining durable costs. No directory tree,
extra data file, persistent reverse lookup or relaxed durability was added.

## Sustained growth and extent reuse

`TestSustainedGrowthAndExtentReuse` starts at 4096 bytes / 512-byte shards, ramps
to 2048 rows, then runs 128 bounded cycles of insert64/move64/delete64/reinsert64
with four concurrent readers, reopening after every growth event. At steady
state the file plateaus (64 active shards, 3584 directory bytes, file 65,536
bytes, largest free run 16,896 bytes) with exact capacity accounting
(current + retained-only + free + superblocks = file). Both full-width ID
distributions produce the same geometry; no obsolete-generation accumulation
occurs. Reuse is only counted when an address disappears from both recoverable
roots and returns.

## Reproduction

```sh
# Current public-API comparison harness:
go test ./index/timeidx -run '^$' -bench '^BenchmarkModernization$' \
  -benchmem -benchtime=200ms -count=5

# Retained search/durable benchmark names:
go test ./index/timeidx -run '^$' \
  -bench '^(BenchmarkSearch|BenchmarkUpdate|BenchmarkApplyPersistentSyncSingle)$' \
  -benchmem -benchtime=200ms -count=5

# Architecture-specific measurements:
TIMESTAMP_BENCH_LARGE=1 go test ./index/timeidx -run '^$' \
  -bench '^(BenchmarkTimeShardSize|BenchmarkTimeHistoricalShardSize|BenchmarkTimeSelectiveCommit|BenchmarkNamespaceParallel|BenchmarkTimeGrowth|BenchmarkTimeSearchDuringCommit)$' \
  -benchmem -benchtime=200ms -count=5

# Opt-in 1M/10M scale matrix and correctness:
TIMEIDX_SCALE=1 GOMAXPROCS=12 go test ./index/timeidx -run '^$' \
  -bench '^BenchmarkScalePreparation$' -benchmem -benchtime=200ms -count=10
TIMEIDX_SCALE=1 go test -race ./index/timeidx -run '^TestScaleMutationCorrectness$' -count=1
go test -race ./index/timeidx -run 'SustainedGrowth|Rebuild' -count=1 -v
```

`benchmark_ab.sh` runs an alternating A/B comparison across two worktrees; the
`commit*_benchmark_test.go` probes isolate allocation, encoding and commit
components. Before declaring a no-regression gate satisfied, repeat these on
target hardware at application scale, including representative namespace counts,
long-held readers, cold opens, update-ID distributions and durability needs.
