# geoidx vs bitmapidx geo benchmarks

This compares `index/geoidx` (S2 level 19, this branch) with the former
`bitmapidx` geo implementation (S2 level 16, removed in this branch).

## Method

- Old side: the pre-removal commit `15e2e0a` in a separate git worktree, with a
  harness at `index/bitmapidx/geocompare_benchmark_test.go` (package
  `bitmapidx_test`). It builds a `KindBitmapGeo` namespace via
  `bitmapidx.NewGeoNamespace` and queries `GeoRanges`, `FilterGeoRanges`,
  `Nearest`, `CoverRegion`.
- New side: `index/geoidx/compare_benchmark_test.go`, the same workloads against
  `geoidx.Index`.
- Both use the same deterministic dataset: `geoBenchN = 200_000` images uniform
  in a 0.4° box around San Francisco (seed 1); sparse/dense use 500_000. All
  indexes are compacted before timing.
- Apple M4 Pro (arm64), Go 1.27, `-benchtime=100ms`. `ns/op` / `B/op` /
  `allocs/op` from `go test -bench`.

## Results

| operation | bitmapidx geo | geoidx | speedup |
|---|---:|---:|---:|
| exact cell (1 key, ~1 image) | 66.0 ns, 1 alloc | 26.5 ns, 0 alloc | **2.5×** |
| single range (one level-14 cell) | 144 ns, 2 allocs | 36.2 ns, 0 alloc | **4.0×** |
| multi range (2 km cap, 16 cells) | 9.90 µs | 4.96 µs | **2.0×** |
| bounding box (0.05°) | 23.1 µs | 10.1 µs | **2.3×** |
| polygon (0.06° square) | 90.7 µs, 1894 allocs | 19.3 µs, 0 alloc | **4.7×** |
| nearest k=1 | 36.2 ms | 45.2 µs | **800×** |
| nearest k=10 | 35.9 ms | 90.0 µs | **399×** |
| nearest k=100 | 36.0 ms | 90.4 µs | **399×** |
| repeated key (10k images, 1 cell) | 5.07 µs | 5.48 µs | 0.93× |
| sparse distribution (500k, 500 m cap) | 669 ns | 133 ns | **5.0×** |
| dense distribution (500k, 500 m cap) | 532 µs | 21.4 ms | 0.025× |
| global→bitmap filter small / medium / large | 1.90 / 1.43 / 3.06 µs | 1.97 / 1.46 / 3.26 µs | ~1.0× |
| probe filter small / medium / large | 183 µs / 9.1 ms / 181 ms | n/a (no probe) | — |
| insert batch 1 / 8 / 32 / 64 | 2213 / 4291 / 10929 / 20248 ns | 123 / 434 / 1500 / 2917 ns | **18× / 9.9× / 7.3× / 6.9×** |
| compact 200k base | 451 µs | n/a (streaming, see note) | — |

### Findings

- **Lookups are 2–5× faster and allocation-free.** geoidx's contiguous
  `(key, id)` arrays turn a cover into binary searches plus bulk contiguous
  copies, where bitmapidx locates a posting per level-16 key and unions roaring
  bitmaps. The polygon case is worst for bitmapidx (1894 allocs).
- **Nearest is the headline.** bitmapidx documented `Nearest` as an O(N log N)
  full scan; geoidx grows an S2 cap cover until it holds `k`, so `k=1` is ~800×
  faster and `k` scales sub-linearly.
- **Small-batch inserts are 7–18× faster** because geoidx appends to two arrays
  and a small sorted delta, instead of resolving/building postings per cell.
- **Repeated-key lookups are at parity (~5 µs for 10k images at one cell).** This
  needed a fast path (below): a range with no delta is a contiguous base run
  copied in bulk; the earlier per-entry scan was ~9× slower.
- **Candidate filtering is at parity with bitmapidx's collect path** and avoids
  its *probe* path entirely. bitmapidx could verify each candidate against a cell
  (up to 181 ms for the large candidate set); geoidx has no reverse index, so it
  always collects the cover and intersects, which measures 2–3 µs.
- **Dense "return almost everything" is ~40× slower.** With 500k of 500k rows
  returned, geoidx must sort the result to ascending `ImageID`, while bitmapidx
  returns an already-ordered roaring bitmap. This only bites when a query returns
  a large fraction of the whole index; realistic radii return a small fraction.
  It is the main known trade-off of the SoA-by-key representation.

### Changes made from this comparison

- `SearchRanges` bulk-copies contiguous base runs when there are no pending
  mutations (was a per-entry callback).
- Sorting is skipped when the result is already ascending, and de-duplication is
  skipped for a base-only scan. These brought the repeated-key case from ~58 µs
  to ~5.5 µs.

### Storage

bitmapidx stores one posting (tiny or roaring) per occupied S2 cell plus a
dictionary; geoidx stores exactly two `uint64` arrays of length equal to the
image count (16 B/image), independent of cell fan-out. The level also changed
from 16 (~142 m cells) to 19 (~18 m cells), so geoidx has ~64× more distinct
cells but stores them as flat sorted arrays rather than per-cell postings.

## Reproducing

```sh
# new side
go test -run '^$' -bench BenchmarkGeoNew -benchtime=100ms ./index/geoidx/

# old side (pre-removal commit)
git worktree add /tmp/geobench-old 15e2e0a
# add the bitmapidx harness, then:
cd /tmp/geobench-old && go test -run '^$' -bench BenchmarkGeoOld -benchtime=100ms ./index/bitmapidx/
```
