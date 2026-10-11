# geoidx paging, storage, and SIMD investigation

Design and results log for the paged-storage work proposed on top of the flat SoA
`geoidx` introduced in #14. The PR description carries the full brief.

## Objective

Reduce write amplification and improve cache behaviour for continuous uploads
(batches of 1–64) at ~20–30M images, without regressing query latency, lock-free
readers, or the public search API. Reuse existing infrastructure rather than
adding a second allocator or storage abstraction.

## Reusable infrastructure reviewed

- `internal/storage`: mmap (`CreateMapped`/`OpenMapped`), dual-slot CRC32C
  superblocks, atomic manifest publish, `ReaderTracker`, `WAL`.
- `internal/pagealloc`: 512-byte copy-on-write extent allocator (used by
  `radixhash`), with `Alloc`/`Free`/`Retire`/`Reclaim` and a reader grace period.
- `radixhash`: page directory + pagealloc + COW publication.
- `simhash`: sharded immutable columns + batch redo WAL + manifest commit marker.
- `bitmapidx`: immutable generations, per-key postings, vacuum.
- `timeidx`: sorted range access.
- `roar`: SIMD-enabled roaring operations.

## Baseline (flat SoA, #14), Apple M4 Pro, `-benchmem`

| metric | value |
|---|---:|
| exact cell (`SearchRanges`, 1 range) | 29.2 ns, 0 allocs |
| multi range (2 km cover, 1M) | 5.35 µs, 0 allocs |
| initial results (1M) | 5.46 µs, 0 allocs |
| FilterBy medium (1M) | 2.47 µs, 1 alloc |
| clustered range (1M) | 54.9 µs, 1 alloc |
| nearest k=10 (1M) | 85.4 µs |
| 64-entry write with rotation | 4.77 µs, 4 allocs |
| persistent base size | 16 B/image |
| buffered compaction | rewrites the base every base/8 mutations → ~128 B/mutation |

Reproduce:
`go test -run '^$' -bench 'BenchmarkGeoNew|BenchmarkGeoFilterNew|BenchmarkGeoClusteredNew|BenchmarkGeoInsertRotation' -benchmem ./index/geoidx/`

## Experiment: single-level page directory

`BenchmarkPagedLookup` compares a flat raw `sort.Search` over the 1M sorted key
array against a two-level directory (`firstKey`/`offset` per page, binary search
the directory then the page). Same scan and sort after the locate.

The directory must find the *first* index of a query key even when that key's run
spans several pages. `locate` steps back to the last page whose first key is
`<= key` and binary-searches within it, so a run beginning in that page's tail is
found; a run beginning at the next page's start is returned as this page's end.
`TestPageDirLocateMatchesFlat` builds runs of identical keys whose lengths do not
divide the page size and requires the paged result to equal the flat result for
every page size (1–256) and range, including ranges that start on an absent key.

| case | ns/op |
|---|---:|
| flat-locate, exact | **8.95** |
| paged, exact, 256/512/1024/2048 rec | 12.12 / 11.71 / 11.36 / 11.52 |
| flat-locate, multi | 35.06 µs |
| paged, multi, 256/512/1024/2048 rec | 35.03 / 35.05 / 35.58 / 34.86 µs |

The page directory does **not** help: the extra level costs ~30% on exact lookups
and is neutral on multi-range scans. (The 28 ns `SearchRanges` figure includes
normalization and view handling; the raw flat locate is 8.9 ns.)

## Experiment: direct per-batch page COW (modeled write amplification)

This is a **model of direct per-batch COW, not measured physical writes**.
`TestPageWriteAmplification` derives bytes rewritten from the distinct pages a
64-mutation batch touches: `bytes/mutation = touchedPages · pageRecords · 16 / 64`.
Buffered page-level COW — deferring and coalescing page rewrites the way the
current flat compaction buffers mutations — was **not implemented or independently
measured**, so this bounds the best case for *direct* COW only, not for any paged
design.

| distribution | 256 rec | 512 rec | 1024 rec | 2048 rec |
|---|---:|---:|---:|---:|
| clustered | 1,740 | 3,471 | 6,888 | 13,443 |
| uniform | 4,064 | 8,064 | 15,816 | 30,618 |

For comparison, the current flat buffered compaction rewrites the whole base every
`base/8` mutations; that `~128 bytes/mutation` (16 B/image) is likewise a model
derived from the trigger, not a sampled disk-write count. Direct page COW is
14×–240× worse for these batch sizes because a 64-mutation batch touches 27–64
distinct pages and rewrites each page whole. Direct COW only approaches the flat
cost when updates are extremely clustered *and* pages are tiny; it becomes far
worse as updates scatter, which is the realistic case.

## Experiment: scan bandwidth (SIMD headroom)

`BenchmarkKeyScan` sums 1M contiguous `S2Key`s:

| kernel | throughput | ns/key |
|---|---:|---:|
| scalar sequential scan | 31.5 GB/s | 0.254 |

The scan is memory-bandwidth-bound. A SIMD range-comparison kernel could not
exceed the memory floor, and the query hot spots are binary search (logarithmic,
not scan-heavy) and result materialization/sorting (for broad results), not the
key comparison. SIMD is therefore **deferred**: no representative end-to-end
benchmark demonstrates a benefit, and it stays out until one does.

## Decision

**Retain flat SoA with buffered background compaction; do not adopt paged SoA,
page-level COW, or SIMD.**

Rationale, from the measurements above:

1. The page directory adds a level with no query benefit (exact 8.7 → 11.3 ns;
   multi neutral).
2. Direct page-level COW multiplies write amplification for the target 1–64 batch
   sizes (1.7–30.6 KB/mutation vs ~128 B/mutation) because batches touch many
   pages. Buffered page COW was not implemented, so no paged design was measured
   to beat the flat buffered model.
3. The current buffered model already bounds write amplification (~base/8) and
   keeps small-batch writes at ~4.8 µs / 4 allocs.
4. The scan kernel is memory-bandwidth-bound and the query hot spots are binary
   search and materialization, so SIMD has no headroom worth its complexity.

This follows the plan's "retain flat SoA and document the benchmark result"
branch. Revisit paging only if a future workload shows strongly clustered,
page-local updates where `touchedPages · pageBytes / batch` beats `baseBytes /
(base/8)`.

## Profiling pass (CPU + MEM pprof)

Captured with `go test -run '^$' -bench '...' -cpuprofile -memprofile`.
`BenchmarkGeoInsertRotation` inserts unbounded new ImageIDs, so `-benchtime=1s`
makes Go calibrate to an enormous N and the run can take minutes; captures use a
bounded `-benchtime=Nx`.

### Findings

1. **Compaction was O(base · log Δ).** `prepareBase` called `mutationIndex` (a
   binary search into the id-sorted layer) once per base entry in `exactCount`
   and `mergeInto`, and `baseHasLive` rescanned equal keys per add. On clustered
   data (high key fan-out) a `sample(1)` of a write-heavy run showed the
   background compaction goroutine pinned in `baseHasLive` for the whole window;
   the first `-benchtime=1s` capture ran 108 s before being stopped.
2. **`FilterRanges`/`CollectRanges` heap-allocated a 2 KiB `collector` per
   query** (28.7% of query alloc_space): `newCollector` returned a pointer and
   `AddMany` (another package) made its stack buffer escape.
3. **`sortIDsBitset` allocated a transient dense bitset** (up to ~128 KiB) per
   broad range query (24% of query alloc_space).
4. Remaining CPU is inherent: the initial-query ordering contract
   (`sortIDs` comparison sort, ~14%) and `roar` canonicalization triggered by the
   benchmark's own `Cardinality()` call (~15%); `scanRanges` and
   `roar.Bitmap.Contains` dominate the rest.
5. **Repeated S2Keys made compaction quadratic.** Beyond finding 1, when many
   images share one key `baseHasLive` rescanned the identical-key run once per
   insertion — O(base) per add — so a base of N images at one key compacted in
   O(N²). The same sweep exposed a latent out-of-range in `mergeInto`: after the
   shadow-skip loop exhausted the base while insertions were already exhausted, it
   indexed `adds[addIndex]` out of range.

### Changes

- One `mutationSet` per base rewrite (dense bitset, binary-search fallback for
  sparse domains) replaces the per-entry binary search; the read path's
  `deltaSet` and `Len` reuse the same type.
- `exactCount` and `mergeInto` now share one streaming traversal over the two
  `(key, id)`-sorted sequences, so a run of identical keys is walked once and no
  index-sized temporary is allocated. `baseHasLive`/`searchKey` are removed. The
  traversal also fixes the `mergeInto` out-of-range noted above.
- `collector` is pooled; `Filter*`/`Collect*` are allocation-free.
- The `sortIDsBitset` scratch buffer is pooled, bounded to 256 KiB.

### Pooled-collector safety

`roar.MutableBitmap.AddMany` copies (`m.adds = append(m.adds, ids...)`) and
`fromSorted` encodes into a freshly allocated buffer, so a roaring bitmap never
retains the collector's buffer; the pool may reuse it immediately. Guards:
`roar.TestAddManyCopiesInput` (AddMany copies rather than aliases) plus
`geoidx.TestCollectorPoolReusePreservesBitmaps` (sequential churn) and
`geoidx.TestCollectorPoolConcurrent` (eight goroutines, `-race`).

### Results (Apple M4 Pro)

A/B at `-benchtime=2000x` (same machine, stashed vs applied):

| benchmark | before | after |
|---|---:|---:|
| InsertRotation trigger=1048576 | 2.87 µs / 20,500 B / 4 | 2.81 µs / 20,516 B / 4 |
| InsertRotation trigger=256 | 49.6 µs / 30,298 B / 4 | **9.9 µs** / 30,727 B / 4 |
| Filter collect | 7.88 µs / 2,311 B / 1 | **6.72 µs** / 9 B / 0 |

The `mutationSet` fix took trigger=256 to 25.2 µs; the repeated-key streaming fix
(below) took it to ~9.9 µs. `-count=3` ranges: 2.81–2.98 µs and 9.7–9.9 µs.

Query set at `-benchtime=200ms` (`ns/op · B/op · allocs/op`):

| benchmark | before | after |
|---|---|---|
| MultiRange | 5.33 µs · 0 · 0 | 5.40 µs · 0 · 0 |
| Nearest k=1 / 10 / 100 | 45.0 / 88.4 / 248 µs | 44.8 / 86.6 / 249 µs |
| Filter initial | 5.33 µs · 0 · 0 | 5.39 µs · 0 · 0 |
| Filter collect | 7.09 µs · 2304 B · 1 | 6.72 µs · 0 · 0 |
| Filter small | 3.10 µs · 2304 B · 1 | 2.80 µs · 0 · 0 |
| Filter medium | 2.43 µs · 2304 B · 1 | 2.28 µs · 0 · 0 |
| Filter large | 10.3 µs · 2308 B · 1 | 10.4 µs · 70 B · 0 |
| Clustered range | 56.8 µs · 131 KB · 1 | 54.7 µs · 53 B · 1 |
| Clustered nearest10 | 19.5 µs · 12 KB · 194 | 19.0 µs · 12 KB · 194 |
| Clustered filter/all | 1.23 ms · 2304 B · 1 | 1.20 ms · 21 B · 0 |

Query alloc_space in the aggregate profile: **2041 MB → 985 MB (−52%)**.

### Repeated-key compaction

`BenchmarkRepeatedKeyCompaction` compacts once over a base where every image
shares one S2Key, mixing deletes, moves to a second key, and fresh inserts. A/B at
`-benchtime=1x` (one op, `ns/op`):

| n | before (baseHasLive) | after (streaming) | speedup |
|---:|---:|---:|---:|
| 1,000 | 1.52 ms | 39.7 µs | 38× |
| 10,000 | 68.9 ms | 209 µs | 330× |
| 100,000 | 4.38 s | 2.05 ms | 2,140× |

The old path is quadratic (×45 then ×64 per ×10 data); the new path scales
roughly linearly. Both allocate 4 objects/op (output arrays + `sortedByKey` +
`mutationSet`); no index-sized temporary is added.

### Deferred

- `Nearest` allocates ~29–67 KB / 468–1067 allocs per call, dominated by s2
  coverer internals across the expanding-cap rounds (55.7% of remaining query
  alloc_space). Reducing it needs a different cover strategy, out of scope.
- The comparison sort for small initial results (<8192) is the largest remaining
  geoidx CPU item; the ordering contract requires it, and the dense bitset does
  not beat pdqsort at that size once scratch trade-offs are included.

## Status

- [x] baselines captured
- [x] paged SoA prototype (single-level directory) benchmarked
- [x] page-size / directory-shape comparison
- [x] page-directory repeated-key locate fixed + differential test
- [x] page-level COW write-amplification model (direct per-batch; buffered COW not implemented)
- [x] SIMD (scan-bandwidth) evaluation — deferred, no end-to-end benefit shown
- [x] CPU + MEM pprof captured and acted on
- [x] pooled collector copy/retention verified (sequential + concurrent tests)
- [x] repeated-key compaction made linear + count/merge differential test
- [x] decision recorded (retain flat SoA)
- [ ] full paged mmap format + pagealloc COW (deferred; not justified by the data)

Experiment code lives in `benchmark_test.go`: `pageDir`, `pagedSearch`,
`flatSearch`, `TestPageDirLocateMatchesFlat`, `BenchmarkPagedLookup`,
`TestPageWriteAmplification`, `BenchmarkKeyScan`,
`BenchmarkRepeatedKeyCompaction`. The count/merge agreement and repeated-key
correctness tests live in `compaction_test.go`; collector-pool reuse tests in
`search_test.go`; the AddMany copy contract test in `roar/mutable_test.go`.
