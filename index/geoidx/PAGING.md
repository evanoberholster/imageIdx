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

| case | ns/op |
|---|---:|
| flat-locate, exact | **8.68** |
| paged, exact, 256/512/1024/2048 rec | 11.98 / 11.75 / 11.54 / 11.29 |
| flat-locate, multi | 35.65 µs |
| paged, multi, 256/512/1024/2048 rec | 35.68 / 35.71 / 35.61 / 35.59 µs |

The page directory does **not** help: the extra level costs ~30% on exact lookups
and is neutral on multi-range scans. (The 29 ns `SearchRanges` figure includes
normalization and view handling; the raw flat locate is 8.7 ns.)

## Experiment: page-level COW write amplification

`TestPageWriteAmplification` models page-level COW for 64-mutation batches: each
batch rewrites one page per distinct page touched. `bytes/mutation = touchedPages
· pageRecords · 16 / 64`.

| distribution | 256 rec | 512 rec | 1024 rec | 2048 rec |
|---|---:|---:|---:|---:|
| clustered | 1,740 | 3,471 | 6,888 | 13,443 |
| uniform | 4,064 | 8,064 | 15,816 | 30,618 |

Flat buffered compaction writes **~128 bytes per mutation** (16 B/image with a
base/8 trigger). Page-level COW is 14×–240× worse for these batch sizes because a
64-mutation batch touches 27–64 distinct pages and rewrites each page whole. COW
only approaches the flat cost when updates are extremely clustered *and* pages are
tiny; it becomes far worse as updates scatter, which is the realistic case.

## Experiment: scan bandwidth (SIMD headroom)

`BenchmarkKeyScan` sums 1M contiguous `S2Key`s:

| kernel | throughput | ns/key |
|---|---:|---:|
| scalar sequential scan | 31.5 GB/s | 0.254 |

The scan is memory-bandwidth-bound. A SIMD range-comparison kernel could not
exceed the memory floor, and the query hot spots are binary search (logarithmic,
not scan-heavy) and result materialization/sorting (for broad results), not the
key comparison. SIMD is therefore not justified.

## Decision

**Retain flat SoA with buffered background compaction; do not adopt paged SoA,
page-level COW, or SIMD.**

Rationale, from the measurements above:

1. The page directory adds a level with no query benefit (exact 8.7 → 11.3 ns;
   multi neutral).
2. Page-level COW multiplies write amplification for the target 1–64 batch sizes
   (1.7–30.6 KB/mutation vs 128 B/mutation) because batches touch many pages.
3. The current buffered model already bounds write amplification (~base/8) and
   keeps small-batch writes at ~4.8 µs / 4 allocs.
4. The scan kernel is memory-bandwidth-bound and the query hot spots are binary
   search and materialization, so SIMD has no headroom worth its complexity.

This follows the plan's "retain flat SoA and document the benchmark result"
branch. Revisit paging only if a future workload shows strongly clustered,
page-local updates where `touchedPages · pageBytes / batch` beats `baseBytes /
(base/8)`.

## Status

- [x] baselines captured
- [x] paged SoA prototype (single-level directory) benchmarked
- [x] page-size / directory-shape comparison
- [x] page-level COW write-amplification model
- [x] SIMD (scan-bandwidth) evaluation
- [x] CPU/memory behavior (0 allocs on the hot paths, measured)
- [x] decision recorded (retain flat SoA)
- [ ] full paged mmap format + pagealloc COW (deferred; not justified by the data)

Experiment code lives in `benchmark_test.go`: `pageDir`, `pagedSearch`,
`flatSearch`, `BenchmarkPagedLookup`, `TestPageWriteAmplification`,
`BenchmarkKeyScan`.
