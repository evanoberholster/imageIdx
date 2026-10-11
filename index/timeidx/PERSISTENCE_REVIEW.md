# GeoIdx vs TimeIdx: persistence and performance review

This review compares GeoIdx PR #15 with the TimeIdx implementation at `cd3e2cf`
and the small follow-up optimizations in this change. Neither the persisted
format nor durability/publication ordering changes in this follow-up.

## Important differences

| Area | GeoIdx | TimeIdx |
|---|---|---|
| Write acknowledgment | Publishes immutable pending mutation layers; success is not itself a durable checkpoint | Changed persistent `Apply` syncs a complete COW generation before returning |
| Compaction | Bounded pending layers and a background compactor; explicit checkpoint waits for durability | Synchronous selective shard replacement on each changed batch; no pending layers or worker |
| Reader work | Base scans plus shadowed-ID checks and pending additions; cold delta cache builds through `sync.Once` | One fully compacted snapshot; binary bounds and bulk ID-column append |
| Shard sizing | Separate min/target/max controls; defaults target 16 MiB, max 32 MiB | One maximum, measured default 4 KiB; balanced splits and adjacent-new-shard merging |
| Ownership | One index/file, `geoidx.dat` plus advisory lock | Namespace manager; independent fixed-width namespace files and locks |
| Commit failure | Frozen state retained for retry; uncertain candidate extents remain protected | Uncertain root publication latches writes until close/reopen |
| Reader drain | Background compaction remains marked in flight through reclamation | Writer mutex held through reader drain, before allocator reuse |
| Key/query semantics | S2 keys, unions of inclusive ranges; geographic validation and optional distance work | Exact 1900-epoch nanoseconds, one half-open interval, chronological results |

Both use parallel 64-bit columns, selective COW extents, cached unchanged-column
checksums, two checksummed superblocks, two sync barriers per persisted
generation, retained fallback generations, allocator reconstruction, and
reader-safe mapping growth. Neither reconstructs discarded timestamps or
performs cross-index transactions.

**GeoIdx's mutation latency is not a fair baseline for TimeIdx's durable
`Apply`.** It omits the checkpoint work that TimeIdx must finish before returning.
Likewise, compare GeoIdx base-only scans separately from delta-bearing scans;
the latter include lazy cache construction and membership filtering. The shared
`MappedFile.Sync` flushes the whole mapping and then syncs its file in both
implementations. This review deliberately leaves those barriers intact.

## Small changes selected

1. **Initialize allocator free bits by word, not block.** Mark whole words free,
   clear reserved block zero and mask the partial final word. This removes one
   bit operation per arena block from every allocator reconstruction, keeping
   the same allocator, error checks and COW reclamation protocol. Both indexes
   benefit; initialization is still proportional to bitmap size, not constant.
2. **Build extent unions in one owned slice.** Concatenate, sort by address and
   compact duplicates, rejecting inconsistent lengths. This replaces a growing
   hash table plus output slice with one allocation. Inputs remain untouched;
   overlap validation still occurs when reserving extents in the allocator.
3. **Allocate TimeIdx heap SoA columns together.** Two non-overlapping,
   capacity-limited slices share one backing allocation. Persistent merge
   buffers remain writer-owned; heap snapshots still get fresh immutable data.
   Adjacent-new-shard merges use the same buffer helper instead of independently
   cloning/growing each column.

No WAL, reverse ID index, commit queue, asynchronous durability, on-disk
allocator metadata, SIMD, or additional synchronization was introduced. Large
directories and sparse-ID scans remain scaling costs. Changing them would be a
larger architecture change than the measurements justify here.

## Measurements

Apple M4 Pro, Go 1.27.0, darwin/arm64, five samples, 300 ms/sample; medians.
Baseline is `cd3e2cf`, not the old pre-modernization WAL backend. The same new
shared-kernel benchmark file ran in a detached baseline worktree. Benchmarks
were run sequentially without tests competing for CPU.

| Workload | Before | After | Allocations before → after |
|---|---:|---:|---:|
| Reconstruct allocator, 4 MiB arena / 400 extents | 18.218 µs | 5.555 µs | 2 → 2 |
| Reconstruct allocator, 1 GiB arena / 400 extents | 3.294 ms | 24.949 µs | 2 → 2 |
| Union, 400 distinct extents duplicated in two sets | 11.146 µs | 12.328 µs | 20 → 1 |
| Union, 4K distinct extents duplicated in two sets | 149.659 µs | 161.317 µs | 59 → 1 |
| TimeIdx clean reopen, 100K entries | 485.402 µs | 449.436 µs | 455 → 436 |
| 64 historical memory moves, 100K entries | 96.744 µs | 94.136 µs | 377 → 252 |
| 64 newer-time memory moves, 100K entries | 4.500 µs | 4.434 µs | 5 → 4 |
| 64 newer-time durable moves, 100K entries | 8.026 ms | 8.052 ms | 31 → 12 |
| 64 historical durable moves, 100K entries | 10.972 ms | 10.975 ms | 313 → 283 |
| GeoIdx one-ID update + compaction, 500K base / 1 MiB target | 14.516 ms | 13.595 ms | 35 → 31 |
| GeoIdx one-ID update + compaction, 500K base / 4 MiB target | 15.790 ms | 14.954 ms | 30 → 30 |

The 1 GiB allocator test initializes a runtime bitmap; it does not mmap, write
or sync a 1 GiB file. Its ~132× improvement isolates CPU work, not a promise of
132× faster commits. At 4 MiB the shared rebuild is ~3.3× faster, while full
TimeIdx reopen improves ~7%.

Tradeoffs are explicit: sorted extent union is 8–11% slower in these kernel
tests but cuts bytes from 17,656 to 6,528 (400 extents), and from 280,393 to
65,536 (4K). Total TimeIdx reopen bytes fall from 81,697 to 67,240; selective
commit bytes fall from 32,464 to 21,336 and allocations from 31 to 12. Durable
latency does **not** materially improve: storage sync still dominates. Combined
heap-column allocation slightly increases rounded bytes for the newer-time
memory case (6,928 → 7,440), while reducing allocation count. Historical memory
bytes remain roughly 353 KB/batch. Small latency differences should not be
treated as statistically established wins from these medians alone.

GeoIdx's existing `BenchmarkSingleFileCompact` also improved by 5–6% in these
samples. It uses a 96 MiB arena and includes delete, insert, shard rewrite and
the full durable compaction, rather than measuring only deferred mutation
acknowledgment. Its fixture repeatedly targets the same destination; it is not
a random ingestion benchmark or a direct comparison with TimeIdx's workload.

## Tests and reproduction

New tests exhaustively check allocator free bits around 64-bit word boundaries,
including reserved/tail bits and allocation exhaustion. Extent union tests check
input immutability, ownership, sorted deduplication and invalid lengths. SoA
tests verify that growing the time slice cannot overwrite IDs. Existing
differential, corruption, crash, pinned-reader and growth suites cover the
unchanged lifetime/durability protocol.

```sh
go test ./internal/storage -run '^$' -bench 'BenchmarkCOW' \
  -benchmem -benchtime=300ms -count=5
go test ./index/geoidx -run '^$' -bench '^BenchmarkSingleFileCompact$' \
  -benchmem -benchtime=300ms -count=5
go test ./index/timeidx -run '^$' \
  -bench 'BenchmarkModernization/(Batch64|Reopen)$|BenchmarkTimeHistoricalShardSize/Bytes=4096$|BenchmarkTimeSelectiveCommit$' \
  -benchmem -benchtime=300ms -count=5
```

Follow-up opportunities need evidence: whole-mapping sync cost at large arena
sizes, large-directory rewriting, sparse-ID lookup, and representative GeoIdx
checkpoint workloads beyond the measured one-ID fixture. Do not remove sync barriers or import deferred GeoIdx
acknowledgment semantics just to lower TimeIdx's write benchmark.
