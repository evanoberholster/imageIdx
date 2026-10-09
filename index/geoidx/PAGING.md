# geoidx paging, storage, and SIMD investigation

Design and results log for the paged-storage work on top of the flat SoA
`geoidx` introduced in #14. This file is the package-facing record of the plan
and its measured outcomes; the PR description carries the full brief.

## Objective

Reduce write amplification and improve cache behaviour for continuous uploads
(batches of 1–64) at ~20–30M images, without regressing the query latency,
lock-free readers, or the public search API. Reuse existing infrastructure
(`internal/storage`, `internal/pagealloc`, `radixhash`, `simhash`, `bitmapidx`,
`timeidx`, `roar`) rather than adding a second allocator or storage abstraction.

## Baseline (flat SoA, #14)

Record before changing storage:

| metric | value |
|---|---:|
| persistent base size | 16 B/image (two `uint64` arrays) |
| mutation memory | ~24 B per pending mutation |
| compaction | full-base streaming rewrite |
| bytes rewritten per mutation | ~base/trigger (≈8×) |
| initial query (1M, 2 km) | ~5.3 µs |
| exact cell | ~28 ns |
| 64-entry write with rotation | ~4.4 µs, 4 allocs |
| nearest k=10 (1M) | ~19 µs |

Reproduce with `go test -run '^$' -bench . -benchmem ./index/geoidx/`.

## Plan

1. **Page directory**: split the sorted `(S2Key, ImageID)` arrays into
   independently replaceable pages behind a compact directory
   (`firstKey`, `firstId`, `offset`, `count`). Binary search page boundaries;
   repeated keys may span pages. Benchmark page sizes 4/8/16/32 KiB and compare
   directory AoS vs SoA. Adopt a second directory level only if measured.
2. **Page-level COW**: replace only affected pages during compaction, reusing
   free pages via `internal/pagealloc`; keep published pages immutable and
   readers lock-free. Compare against the current streaming full-base compaction
   and choose by measured write amplification and total CPU.
3. **Mutation layers / compaction**: keep small active + frozen layers; merge
   affected pages streaming with bounded scratch; avoid a full temporary copy.
4. **mmap/cache**: 64-byte alignment, contiguous arrays, no per-entry heap
   objects, minimize directory lookups during sequential scans.
5. **SIMD**: evaluate AVX2/NEON for contiguous S2Key range comparisons and bulk
   candidate filtering using the repo's existing dispatch conventions; retain
   only where benchmarks justify it. Do not duplicate roaring SIMD kernels.
6. **Profiling**: CPU and memory profiles per workload; guide work by measured
   hotspots.

## Invariants to preserve

- S2 level 19; one `S2Key` per image; many ImageIDs per key.
- No reverse `ImageID -> S2Key` index; no lat/lng columns; no per-cell postings.
- Initial queries return ascending `[]ImageID`; FilterBy operates on a roaring
  bitmap with no global sort; distance queries return
  `Result{ImageID, Distance uint32}` with full-precision internal ordering.
- Page boundaries must not affect results.
- Persistence guarantees and format versioning/recovery tests are mandatory for
  any format change.

## Acceptance

| metric | required |
|---|---|
| exact lookup | no material regression |
| small / broad ranges | no material regression |
| FilterBy | faster than bitmapidx where practical |
| nearest-N | preserve current gains |
| small-batch writes | preserve low latency |
| write amplification | material reduction |
| compaction stalls | reduced |
| peak memory | bounded |
| storage size | comparable or improved |

If paging causes a significant query regression, retain flat SoA and record the
benchmark result here.

## Status

- [ ] baselines captured
- [ ] profiles captured
- [ ] paged SoA prototype
- [ ] page-size / directory-shape comparison
- [ ] page-level COW comparison
- [ ] SIMD evaluation
- [ ] correctness, race, CPU, memory validation
