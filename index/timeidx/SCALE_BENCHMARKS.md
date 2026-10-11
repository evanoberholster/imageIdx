# PR #16 final benchmark evidence

**Final authoritative measurements:** see the opening section of
`FINAL_REVIEW.md`. The tables below retain intermediate-candidate history.
The final ten-process A/B measures historical memory64 −7.56% (p=.002) and
10M sequential mutation1 −16.61% (p<.001), with no unexplained significant
regression above 5%. Final scale preparation and the entire shard/storage
matrix were rerun after both mutation-loop and delete-routing improvements.
Raw evidence is durably archived in `evidence/pr16-final.tar.gz`.

The final optimization pass supersedes the earlier provisional results and
outstanding-work list below. See `FINAL_REVIEW.md` for validation, growth/rebuild
results and the merge recommendation. Baseline remains `4c7847c`; reviewed
starting HEAD is `a41eada`. The candidate adds cached sorted generation extents,
a sorted two-list union fast path, removal of redundant directory clearing,
and a profiled one-pass old-row/addition merge.
No search or persistence-format change was made in this pass.

## Final measurement protocol and regressions

Apple M4 Pro / Go 1.27.0 / darwin-arm64 / GOMAXPROCS=12, same local temporary
filesystem, serial runs. `benchmark_ab.sh` performs ten independent processes
per tree with alternating A/B, B/A order. Benchstat uses x/perf revision
`8e83ce0f7b1c`. The first unchanged-head A/B did not reproduce any reported
regression. Final interleaved A/B medians:

| Operation | Baseline → final | p-value | B/op / allocs/op baseline → final |
|---|---:|---:|---|
| Memory historical64 | 93.27 → 86.62 µs | .001 | ~353 KB / 252 → unchanged |
| Durable historical64 | 9.962 → 10.089 ms | .315 | 103.23 → 96.22 KiB; ~293 → 291 |
| Durable append64 | 8.184 → 8.123 ms | .631 | 21.41 → 18.28 KiB; 12 → 11 |
| 10M sequential mutation1 | 19.49 → 18.38 ms | .052 | 1,983,376 → 1,663,888 bytes; 12 → 11 |
| Reopen | 472.9 → 474.1 µs | .353 | 65.70 → 65.73 KiB; 436 → 436 |
| Exact search | 16.17 → 16.20 ns | .753 | 0 / 0 → 0 / 0 |
| Month search | 854.2 → 851.6 ns | .469 | 0 / 0 → 0 / 0 |

Non-interleaved phase/preparation runs and the short expanded matrix showed
slower single-ID preparation, encoding and some sequential commits. They were
**not discarded**: a supplemental ten-process interleaved A/B retested them:

| Operation | Baseline → final | p-value |
|---|---:|---:|
| 1M sequential prepare1 | 15.97 → 15.88 µs | .796 |
| 1M random prepare1 | 2.773 → 2.767 ms | .579 |
| 10M sequential prepare1 | 130.7 → 122.9 µs | .436 |
| 10M random prepare1 | 11.77 → 11.64 ms | .029 |
| 1M sequential durable1 | 10.243 → 9.448 ms | .011 |
| 1M sequential durable8 | 14.43 → 10.94 ms | <.001 |
| 10M sequential durable1 | 19.50 → 18.40 ms | .089 |
| 10M sequential durable8 | 19.63 → 18.63 ms | .075 |
| 10M directory encoding (no clear) | 386.3 → 386.4 µs | .853 |

Those checks resolved the single-ID/encoding anomalies. A subsequent clean
harness run reproduced memory historical64 +6.18% (p=.015). CPU profiles
located hot work in the old-row/addition merge, including repeated membership/
exhaustion checks, with equal allocations. The final one-pass merge above fixes
that work: memory historical64 −7.13% (p=.001), unchanged 252 allocations.
Original persistent benchmark probes are preserved in their original file;
component probes use `commit_components_benchmark_test.go`.

No significant regression above 5% remains in the final A/B. Day search has a
small significant +1.34% drift (41.56 → 42.12 ns, p<.001), below the threshold,
still zero allocation. Absolute fsync
latencies drifted between runs; comparisons therefore use matched interleaving,
not isolated run medians. Search code did not change: its measured exact-search
difference is not credited to a new search algorithm. Reopen's ~32-byte increase
is a fixed Index field, not per-shard growth. Isolated phase benchmarks identified
additional avoidable directory work without removing correctness checks or
either durability barrier.

Supplemental patterns (repeat the same ten-process alternating protocol):

```sh
-bench '^BenchmarkScalePreparation$/N=(1000000|10000000)$/Random=(false|true)$/Batch=1$' -benchtime=200ms
-bench '^BenchmarkScaleDurableBatches$/N=(1000000|10000000)$/Random=false$/Batch=(1|8)$' -benchtime=10x
-bench '^BenchmarkCommitComponents$/N=10000000$/Encode/Clear=false$' -benchtime=200ms
```

## Directory cost, 10M rows / 4 KiB

Ten phase samples per tree (diagnostics, not an additive model of a full commit):

| Phase | Baseline → final median | Final B/op / allocs/op |
|---|---:|---:|
| Allocate/free directory | 17.42 → 18.55 µs | 0 / 0 |
| Encode (no preliminary clear), interleaved | 386.3 → 386.4 µs | 0 / 0 |
| Directory CRC32C | 187.12 → 189.51 µs | 0 / 0 |
| Build previous extent list | 76.81 µs → eliminated in commits | eliminates 319,488 bytes / 1 allocation |
| Union of sorted retained lists | 1950.71 → 185.88 µs | 630,784 / 1 |
| Allocator reconstruction | 599.74 → 616.04 µs | 73,824 / 2 |
| Dirty unpublished directory, one real sync | 7.627 → 8.040 ms | 0 / 0 |

Encoding already uses precomputed CRCs and ID bounds and writes directly into
an unpublished mmap extent. No heap directory buffer exists to reuse. Removing
the clear avoids a full memory pass; its small speed effect varies across phase
runs and is not claimed as a significant full-commit improvement. The large
measured savings are sorted extent union and the cached prior list. Each new
list is still sorted once, and the allocator is still reconstructed/validated.
The union phase uses two mostly shared sorted lists; actual commits also pay
new-list sorting and two sync barriers, captured by the full durable A/B.

Flat directory write amplification is unchanged: 10M rows at 4 KiB require
~2.19 MB per root directory, even for one-row/source-only commits. This remains
O(shards), and sync is the largest remaining durable cost. No directory tree,
extra data file, persistent reverse lookup or relaxed durability was added.

## Final random-ID scaling

Ten preparation samples; three-sample changed durable-commit matrix. Values in
each tuple are batches **1 / 8 / 32 / 64**. Preparation B/op and allocs/op are
identical before/after:

| Rows | Prepare baseline ms | Prepare final ms | Final durable ms | Preparation bytes / allocations |
|---|---|---|---|---|
| 1M | 2.699 / 10.175 / 23.567 / 29.966 | 2.914 / 3.166 / 1.830 / 2.003 | 12.23 / 13.93 / 13.44 / 14.53 | 32,864 / 33,536 / 35,840 / 38,912 bytes; 2 / 9 / 33 / 65 allocs |
| 10M | 11.648 / 136.541 / 235.388 / 289.631 | 12.043 / 23.897 / 19.419 / 19.755 | 25.38 / 37.77 / 32.47 / 34.44 | 319,584 / 320,256 / 322,560 / 325,632 bytes; 2 / 9 / 33 / 65 allocs |

Single-ID preparation drifts in the ordered phase runs, but the supplemental
interleaved comparison above does not reproduce a regression. Final full
10M random commits allocate 1,663,888 / 1,665,424 / 1,670,800 / 1,677,968 bytes
and 11 / 27 / 77 / 142 objects. Logical rewrite volume is unchanged:
2,192,120 / 2,220,792 / 2,319,096 / 2,450,168 bytes per commit.

Detection still visits 39,063 shards and inspects about 10M IDs at 10M rows,
rewriting only the affected 1/8/32/64 deletion shards. At 1M it visits 3,907.
There is no per-row allocation or unexpected whole-payload rewrite. O(records)
random-ID detection is explicit. Measured batch64 throughput is approximately
1.8K mutations/s at 10M on this machine, not an SLA/percentile guarantee.
Explicit batching is appropriate for this measured workload; no evidence
requires a reverse directory or queue. Higher-latency-sensitive applications
must assess their own load rather than infer arbitrary-ID lookup is constant time.

## Final shard-size decision: retain 4 KiB, configurable

Three samples for all 40 scale/ID/repeated-time/bound combinations, plus new-ID
chronological inserts and batches 1/8/32/64. Representative final random-ID
historical64 medians show why there is no single dominant bound:

| Bound | 1M ms / rewrite bytes | 10M ms / rewrite bytes | 10M wide search (2.5M IDs) | 10M directory bytes / entries |
|---|---:|---:|---:|---:|
| 4 KiB | 11.929 / 478,232 | 32.092 / 2,446,960 | 1.215 ms | 2,187,752 / 39,067 |
| 16 KiB | 11.899 / 1,103,392 | 29.817 / 1,596,024 | .651 ms | 546,952 / 9,767 |
| 64 KiB | 14.191 / 4,160,704 | 32.188 / 4,330,600 | .461 ms | 136,808 / 2,443 |
| 256 KiB | 14.934 / 8,003,984 | 33.574 / 8,152,624 | .388 ms | 34,272 / 612 |
| 1 MiB | 14.919 / 8,001,408 | 35.549 / 9,420,840 | .348 ms | 8,568 / 153 |

Wide searches remain 0 B/op / 0 allocs/op, approximately 2.1 / 3.8 / 5.4 / 6.4 /
7.2 billion returned IDs/s respectively (memory copy throughput, not QPS).
16 KiB is attractive for 10M-row namespaces and larger bounds favor wide reads;
4 KiB has ~2.3× lower logical historical write volume at 1M than 16 KiB, with
similar measured latency. Dense repeated-time historical updates also favor
4 KiB rewrite volume at 1M (328,536 versus 605,696 bytes for random IDs).
Chronological new-ID inserts favor smaller directories at larger bounds; they
do not outweigh scattered-update COW costs universally. Keep compatibility
and the small/selective-update default; recommend evaluating 16 KiB explicitly
for large namespaces. No adaptive sizing is needed.

For 10M random IDs, reopen medians across the five bounds are 1.369 / 1.363 /
1.366 / 1.362 / 1.366 seconds. Sorting IDs for duplicate validation dominates;
temporary O(records) memory remains. File capacity is 274,595,840 bytes at
4 KiB and 268,435,456 at larger bounds; all saw six initial/import remaps.
Storage geometry/rewrite volumes are identical before/after. Sustained
bounded-size growth/reuse is separately verified in `FINAL_REVIEW.md`.

Raw final outputs in the approved session temp directory:
`timeidx-final-ab-{baseline,final}.txt`, `timeidx-single-ab-{baseline,final}.txt`,
and `timeidx-final-{baseline,final}-{CommitComponents,ScalePreparation,ScaleMatrix}.txt`.
The exact harness and A/B script are committed; the tables above permanently
record the decision evidence. Failed preparation-harness output was excluded
and rerun after moving its untimed cardinality assertion outside the loop.

---

## Historical source/lifecycle review (provisional, superseded)

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
