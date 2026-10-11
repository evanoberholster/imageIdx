# PR #16 source/lifecycle and scale review — not merge-ready

Baseline: `4c7847cbe4e96e9cfb91ac55c5c474c78bd3b63d`.
Candidate: source-generation, heap-only metadata and batch-local sparse-ID
prefilter changes in this commit. Apple M4 Pro, darwin/arm64, Go 1.27.0,
default GOMAXPROCS=12. Before and after were run serially on the same machine,
using the same copied opt-in benchmark file and deterministic datasets.

## Reproduction and scope

Copy `scale_benchmark_test.go` into a detached baseline worktree; it deliberately
uses signatures already present in the baseline. Run each tree separately:

```sh
TIMEIDX_SCALE=1 go test ./index/timeidx -run '^$' \
  -bench '^BenchmarkScalePreparation$' -benchmem -benchtime=200ms -count=3
TIMEIDX_SCALE=1 go test ./index/timeidx -run '^$' \
  -bench '^(BenchmarkScaleShards|BenchmarkScaleDurableBatches)$' \
  -benchmem -benchtime=3x -count=1
go test ./index/timeidx -run '^$' \
  -bench '^(BenchmarkSearch|BenchmarkModernization)$' \
  -benchmem -benchtime=200ms -count=5
TIMEIDX_SCALE=1 go test ./index/timeidx -run '^$' \
  -bench '^BenchmarkScaleChronologicalInsert$' -benchmem -benchtime=3x -count=1
TIMEIDX_SCALE=1 go test ./index/timeidx -run '^TestScaleMutationCorrectness$' -count=1
```

The shard sweep covers every combination of 1M/10M rows, sequential/permuted
unique uint64 IDs, chronological/dense repeated timestamps (10,000 rows per
timestamp), and 4/16/64/256 KiB/1 MiB payload bounds. It measures exact and wide
searches, moving existing IDs to the chronological tail, historical moves,
reopen and storage counters. This tail workload is **not** new-ID insert growth.
The separate chronological-insert sweep covers both scales/distributions and
all five bounds using genuinely new IDs, validating increased cardinality.
Initial file creation starts at 4 MiB to measure remapping. Batch latency cases
use 1/8/32/64 mutations and a sequence retained across calibration calls, so
every measured iteration changes records rather than timing retries.

Preparation instrumentation is outside the timed production code. It reports
directory entries visited, candidate shards scanned, ID rows inspected during
old-ID detection, and rewritten shards. It does not count binary-search
comparisons or the subsequent affected-shard filtering pass. There are no
production metric branches. A separate scale correctness test verifies exact
move destinations, all IDs against a sorted reference (no lost or duplicate
IDs), and bounded shard rewrites after each batch at both scales/distributions.

**Sampling limits:** three-iteration matrix results are diagnostic, not robust
latency distributions. Exact-search cold/calibration noise is particularly
large. The five-sample existing hot-path medians are the appropriate evidence
for steady-state search. No p95/p99 claim is made. A full acceptance review
still needs repeated large-matrix samples, long-running insertion/growth
and the tail-latency comparisons listed below.

Raw outputs are in the session's approved temporary directory:
`/private/var/folders/24/5g9k_56576qclfkzp2xrvxv40000gn/T/opencode/`:
`timeidx-scale-preparation-{before,after}.txt`,
`timeidx-scale-matrix-{before,after}.txt`,
`timeidx-hot-before-4c7847c.txt`, and `timeidx-hot-after.txt`.
New-ID insert outputs are `timeidx-scale-insert-{before,after}.txt`.
These local artifacts are not a permanent CI artifact archive.

## Random-ID routing: preparation medians

| Rows | IDs | Batch | Before ns/op | After ns/op |
|---:|---|---:|---:|---:|
| 1M | sequential | 1 | 15,203 | 15,651 |
| 1M | sequential | 8 | 513,986 | 81,847 |
| 1M | sequential | 32 | 3,580,782 | 381,368 |
| 1M | sequential | 64 | 8,329,878 | 818,795 |
| 1M | random | 1 | 2,716,936 | 2,864,931 |
| 1M | random | 8 | 9,910,309 | 2,960,115 |
| 1M | random | 32 | 23,047,273 | 1,774,552 |
| 1M | random | 64 | 28,929,167 | 1,932,057 |
| 10M | sequential | 1 | 150,481 | 130,959 |
| 10M | sequential | 8 | 666,170 | 206,269 |
| 10M | sequential | 32 | 3,720,423 | 480,922 |
| 10M | sequential | 64 | 8,570,458 | 926,847 |
| 10M | random | 1 | 11,096,383 | 11,523,010 |
| 10M | random | 8 | 134,381,583 | 22,887,630 |
| 10M | random | 32 | 232,188,042 | 19,002,955 |
| 10M | random | 64 | 283,824,167 | 19,682,649 |

The sparse filter is a fixed 4096-bit negative membership filter built once per
batch. Hash collisions always fall through to exact binary search. Compact ID
ranges still use the existing exact bitset. A single mutation bypasses the new
filter. No reverse directory, map, per-row allocation or persistent field was
added for routing. Collision correctness has its own test.

At 10M rows/4 KiB the directory has 39,063 entries before mutations. Random
batches of 8/32/64 scan effectively all candidate shards and inspect about
9,998,516 / 9,996,020 / 9,991,886 IDs, respectively, while rewriting only 8/32/64 deletion
shards. Counts are identical before/after. Preparation allocation counts remain
2/9/33/65 for batches 1/8/32/64; B/op is 319,584 / 320,256 / 322,560 / 325,632
for these 10M random-ID cases, unchanged before/after.
The directory output slice scales with shard count, not the number of IDs.

This reduces expensive membership comparisons, **not** asymptotic scanning:
random-ID detection is still O(records), and directory copying/publication is
O(shards). Small sequential batches prune most shards. Routing additions
already sorts `(Timestamp, ValueID)`; for 64 additions into ~39K shards, at most
~1024 binary-search steps are cheaper than a full monotonic directory walk.
A future hybrid monotonic route should be measured on much larger batches,
not added to this small-batch fix. A transient sorted reverse lookup would
require additional per-record memory and maintenance on replacement; it was
not adopted without total-cost evidence. Remaining linear work is explicit.

## Durable 10M-row commits, default 4 KiB

Each cell is before → after. Allocations, rewrite bytes and cardinality are
unchanged. These are means of three commits, not percentile estimates.

| IDs | Batch | ns/op | B/op | allocs/op | Logical bytes/commit |
|---|---:|---:|---:|---:|---:|
| sequential | 1 | 11,739,861 → 13,730,347 | 1,983,376 | 12 | 2,192,120 |
| sequential | 8 | 12,710,667 → 13,986,653 | 1,984,912 | 28 | 2,220,792 |
| sequential | 32 | 16,698,208 → 15,352,458 | 1,990,288 | 78 | 2,319,096 |
| sequential | 64 | 22,188,167 → 16,989,570 | 1,997,456 | 143 | 2,450,168 |
| random | 1 | 26,389,778 → 24,465,250 | 1,983,376 | 12 | 2,192,120 |
| random | 8 | 154,313,847 → 37,712,139 | 1,984,912 | 28 | 2,220,792 |
| random | 32 | 253,913,430 → 34,432,375 | 1,990,288 | 78 | 2,319,096 |
| random | 64 | 312,998,945 → 35,078,472 | 1,997,456 | 143 | 2,450,168 |

The directory is rewritten for each durable generation. Thus a one-row change
does **not** rewrite all record columns but does pay a ~2.19 MB directory cost
at the default bound. This write amplification is real, not hidden by reporting
only payload writes. New root extents and both-generation retention explain
the publication allocations. Warm allocations are not per-ID allocations.

## Shard sweep: 10M random IDs, chronological timestamps

Wide search returns 2.5M IDs with a preallocated destination. All searches in
the matrix remain **0 B/op, 0 allocs/op**. Entries below reflect the final
three-iteration run, including three historical commits.

| Bound | Wide search ns/op before → after | Historical commit ms before → after | Logical bytes/commit | Directory entries / bytes |
|---|---:|---:|---:|---:|
| 4 KiB | 1,154,639 → 1,160,986 | 299.30 → 34.05 | 2,451,664 | 39,066 / 2,187,696 |
| 16 KiB | 714,153 → 696,806 | 299.79 → 30.13 | 1,596,024 | 9,767 / 546,952 |
| 64 KiB | 682,639 → 479,208 | 298.79 → 33.56 | 4,330,600 | 2,443 / 136,808 |
| 256 KiB | 410,528 → 483,417 | 304.40 → 35.18 | 8,152,624 | 612 / 34,272 |
| 1 MiB | 445,236 → 363,222 | 317.93 → 36.19 | 9,420,840 | 153 / 8,568 |

| Bound | Historical B/op / allocs/op (both) | Reopen ms before → after | Reopen B/op / allocs/op (both) |
|---|---:|---:|---:|
| 4 KiB | 2,207,717 / 167 | 1325.76 → 1401.05 | 169,639,640 / 78,168 |
| 16 KiB | 964,581 / 164 | 1328.55 → 1379.62 | 162,572,536 / 19,571 |
| 64 KiB | 1,601,509 / 164 | 1338.94 → 1365.15 | 160,797,688 / 4,923 |
| 256 KiB | 2,813,221 / 85 | 1360.59 → 1365.38 | 160,355,000 / 1,261 |
| 1 MiB | 2,872,186 / 32 | 1363.27 → 1379.63 | 160,242,936 / 343 |

| Bound | File bytes | Free bytes | Largest free run | Initial+commit remaps |
|---|---:|---:|---:|---:|
| 4 KiB | 274,595,840 | 109,952,000 | 107,759,104 | 6 |
| 16 KiB | 268,435,456 | 106,285,056 | 105,704,960 | 6 |
| 64 KiB | 268,435,456 | 103,926,784 | 103,658,496 | 6 |
| 256 KiB | 268,435,456 | 100,069,376 | 99,579,392 | 6 |
| 1 MiB | 268,435,456 | 98,962,432 | 95,833,088 | 6 |

File size is sparse logical capacity, not allocated filesystem blocks. Free
bytes include capacity slack; largest-run comparisons indicate fragmentation
but are not a long-running fragmentation study. Storage counters and allocation
metrics are identical in before/after runs. Large reopen performs global
duplicate-ID validation and sorts temporary IDs; O(records) memory/work remains.
The watermark itself is fixed-width and adds no reopen allocations.

**Default decision:** retain 4 KiB for compatibility and prior small/clustered
historical workloads. This is not a claim it wins the large-data mix: 16 KiB is
promising at 10M rows (less directory overhead and the lowest measured random
historical rewrite volume), and larger shards improve wide search. A default
change requires repeated, representative weighted results, not these few
commits or only the minimum rewrite count. Configurable fixed bounds remain;
there is no adaptive sharding.

### Genuinely new chronological inserts

10M random-ID fixture, 64 new records/commit, three changed commits:

| Bound | ns/op before → after | Logical bytes/op | B/op (both) | allocs/op (both) |
|---|---:|---:|---:|---:|
| 4 KiB | 299,622,542 → 35,297,528 | 2,192,552 | 2,198,986 | 15 |
| 16 KiB | 293,386,931 → 32,963,986 | 560,093 | 619,488 | 14 |
| 64 KiB | 292,863,556 → 31,620,514 | 182,717 | 228,320 | 14 |
| 256 KiB | 294,019,931 → 32,366,986 | 211,077 | 192,650 | 14 |
| 1 MiB | 292,640,514 → 31,702,083 | 1,056,907 | 1,126,752 | 14 |

There were no additional commit remaps in these short runs; initial import
already left capacity slack. File capacities match the table above. New random
IDs still need old-ID detection because Insert is an authoritative upsert,
not an unchecked append. The prefilter benefits that scan as well. This does
not establish sustained growth/fragmentation behavior after capacity is exhausted.

## Existing hot-path medians (five samples)

| Operation | ns/op before → after | B/op before → after | allocs/op before → after |
|---|---:|---:|---:|
| Narrow search | 18.08 → 18.59 | 0 → 0 | 0 → 0 |
| Medium search | 355.7 → 357.0 | 0 → 0 | 0 → 0 |
| Broad search | 39,765 → 40,529 | 0 → 0 | 0 → 0 |
| Exact search | 15.87 → 16.29 | 0 → 0 | 0 → 0 |
| Month search | 838 → 851 | 0 → 0 | 0 → 0 |
| Repeated-time lookup | 15,881 → 15,999 | 0 → 0 | 0 → 0 |
| Memory append64 | 4,956 → 4,676 | 7,440 → 7,440 | 4 → 4 |
| Durable append64 | 7,960,814 → 8,256,857 | 21,878 → 21,922 | 12 → 12 |
| Memory historical64 | 102,921 → 108,848 | 353,407 → 353,446 | 252 → 252 |
| Durable historical64 | 10,404,390 → 11,101,308 | 107,219 → 104,105 | 295 → 292 |
| Reopen | 494,915 → 499,824 | 67,272 → 67,272 | 436 → 436 |

Regressions are not dismissed: existing historical memory/durable cases are
~5.8%/~6.7% slower, durable append ~3.7% slower, and 10M sequential one-row
commit ~17% slower in the short matrix. Single-mutation preparation is also
~3.8% slower in the 10M random fixture, despite bypassing the sparse
filter. Search code was unchanged; its small median drift and large-fixture
exact-search spikes require interleaved repeat runs before assigning causality.
Sparse filtering adds a fixed stack buffer and membership instructions for
multi-ID sparse batches; repeat profiling must separate that cost from system
and filesystem variation. These are unresolved performance acceptance items,
not outweighed automatically by the random-ID speedup.

## Correctness status and remaining acceptance gates

Implemented and tested: atomic durable source watermark, monotonic validation,
checkpoint-only persistence, retries, stale/error distinction, independent
namespaces, coherent crash/uncertain recovery, immutable registry and retained
handle shutdown. The opt-in 1M/10M correctness test passed both normally and
under `-race`, and targeted source/crash/shutdown tests passed ten repeats.
The lifecycle audit is in `SOURCE_AND_LIFECYCLE.md`. Full build,
tests, race tests and vet passed; GeoIdx regressions passed. Map syntax audit
found no maps in TimeIdx, `internal/storage` or `internal/pagealloc`.

Existing shared code is retained: `MappedFile`, `ReaderTracker`, COW outcomes,
extent unions, allocator reconstruction and the 56-byte descriptor codec.
Mmap orchestration and domain validations remain specialized; their lifetime
rules differ. No further extraction or GeoIdx public API change was justified.

**PR #16 remains unmerged/not merge-ready.** Outstanding review gates:

- Repeat large matrix and investigate the measured historical/sequential
  regressions; archive complete results durably with the PR.
- Extend new-ID chronological growth to long-running fragmentation and
  large repeated-time steady-state exact-search samples.
- Evaluate explicit batching versus bounded group-commit latency (p95/p99)
  and bounded deferred-reclamation evidence without assuming workers are needed.
- Complete rebuild process-interruption and rename/directory-sync fault coverage.
- Finish monotonic-routing/recovery profiling and final benchmark-weighted
  default review. Current fixed 4 KiB default is deliberately unchanged.

Passing the correctness suite is not a substitute for these performance and
rebuild acceptance gates. No queues, relaxed mode, reverse index, extra data
files, runtime namespace replacement or SIMD were introduced.
