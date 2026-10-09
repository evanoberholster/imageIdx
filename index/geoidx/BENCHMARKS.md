# geoidx vs bitmapidx geo benchmarks

Compares `index/geoidx` (S2 level 19) with the removed `bitmapidx` geo
implementation (S2 level 16).

## Method

- Old side: pre-removal commit `15e2e0a` in a git worktree, harness at
  `index/bitmapidx/geocompare_benchmark_test.go` (package `bitmapidx_test`).
- New side: `index/geoidx/compare_benchmark_test.go` and
  `index/geoidx/filter_benchmark_test.go`.
- Identical deterministic datasets and query points; indexes compacted before
  timing. Datasets: 200k and 1M images uniform in a 0.4° box around San
  Francisco (seed 1); 1M clustered (8 cities, 0.05° spread + 5% background);
  10M uniform for scale.
- Apple M4 Pro (arm64), Go 1.27, `-benchtime=100ms`.

## Lookups — 200k uniform

| operation | bitmapidx geo | geoidx | speedup |
|---|---:|---:|---:|
| exact cell | 63.9 ns, 1 alloc | 27.7 ns, 0 alloc | 2.3× |
| single level-14 range | 144 ns, 2 allocs | 37.4 ns, 0 alloc | 3.9× |
| 2 km cover, 16 cells | 9.90 µs | 5.18 µs | 1.9× |
| bounding box | 23.1 µs | 10.4 µs | 2.2× |
| polygon | 90.7 µs, 1894 allocs | 19.4 µs, 0 alloc | 4.7× |
| nearest k=1 / 10 / 100 | 36.8 / 35.9 / 36.4 ms | 44.7 / 90.4 / 288 µs | 823× / 397× / 126× |
| repeated key (10k at one cell) | 5.07 µs | 4.16 µs | 1.2× |
| sparse distribution (500k) | 669 ns | 139 ns | 4.8× |
| dense distribution (500k, ~all match) | 532 µs | 512 µs | 1.04× |
| insert batch 1 / 8 / 32 / 64 | 2500 / 4946 / 13130 / 21773 ns | 145 / 444 / 1490 / 2952 ns | 17× / 11× / 8.8× / 7.4× |
| insert allocs 1 / 8 / 32 / 64 | 27 / 61 / 168 / 298 | 4 / 4 / 4 / 4 | — |

## Query modes and FilterBy — 1M uniform

| operation | bitmapidx geo | geoidx | speedup |
|---|---:|---:|---:|
| initial `[]ImageID` | 9.72 µs | 5.14 µs | 1.9× |
| collect geo→bitmap | 6.92 µs, 0 alloc | 8.18 µs, 3 allocs | 0.85× |
| FilterBy small candidates (1k) | 8.22 µs | 3.24 µs | 2.5× |
| FilterBy medium candidates (50k) | 8.49 µs | 2.47 µs | 3.4× |
| FilterBy large candidates (1M) | 8.79 µs | 11.4 µs | 0.77× |
| clustered range (2 km) | 246 µs, 866 KB | 41.8 µs, 131 KB | 5.9× |
| clustered FilterBy (all) | — | 402 µs | — |
| clustered nearest k=10 | 200.5 ms, 112 MB | 18.9 µs, 12 KB | 10,600× |

## Scale — 10M uniform (geoidx)

| operation | geoidx |
|---|---:|
| initial `[]ImageID` (2 km cover) | 633 µs |
| collect geo→bitmap | 754 µs |
| nearest k=10 | 15.8 µs |

## Storage — 1M uniform persistent

| implementation | bytes | per image |
|---|---:|---:|
| geoidx (level 19) | 16,001,050 | 16 B |
| bitmapidx geo (level 16) | 12,302,064 | 12 B |

geoidx stores exactly two `uint64` arrays regardless of cell fan-out (16 B/image).
bitmapidx stores one posting per occupied level-16 cell; the coarse level
collapses this grid into fewer, denser cells, so it is 1.3× smaller here. At
equal precision bitmapidx's per-cell postings would grow with the (4× larger)
level-19 cell count, while geoidx stays flat.

## Optimizations made from this comparison

- `FilterRanges`/`FilterCell`/`FilterCover`/`FilterRect`/`FilterPolygon`/
  `FilterRadius`: geography filters an existing `roar.Bitmap` in native S2 order
  with a membership test, no sorted `[]ImageID` and no global sort.
- `CollectRanges` and cover/cell/rect/polygon/radius variants: geography to a
  `roar.MutableBitmap` directly.
- The initial-query suffix sort now picks, in order: no sort when already
  ordered, a bounded dense bitset sort when the ImageID domain is compact, an
  LSD radix sort otherwise, and a comparison sort only for small results. This
  removed the broad-query regression: dense 512 µs (parity with bitmapidx) and
  clustered range 42 µs (5.9× faster than bitmapidx).
- Bounded write delta: `Config.TargetDelta` (64) and `Config.MaxDelta` (4096).
  Reaching `MaxDelta` folds the delta synchronously, so a caller that forgets
  `Compact` never degrades reads without bound. Small batches use stack scratch,
  so `InsertBatch(1..64)` allocates a constant 4/op.
- Correctness: `SearchRadius`/`SearchRadiusResults` now require the exact
  cell-center distance to be within the radius (the cover is only a candidate
  superset); `Nearest` terminates only when the k-th cell-center distance is
  inside the covered cap, which proves no unvisited cell can be closer. Both are
  verified by brute-force differential tests.

## Remaining trade-offs

- **Large-candidate FilterBy** (candidates are a superset of the geo set, e.g.
  all images): 11.4 µs vs 8.79 µs for bitmapidx's collect+And. The extra work is
  the per-entry membership test and the result sort. The planner avoids this by
  keeping geography as the source (or collecting) when its estimate is not
  smaller than the candidates.
- **Collect** (geo → bitmap) is 1.18× slower than bitmapidx's pre-sorted
  per-cell fold, because geoidx must sort the matched IDs once; it stays within a
  few microseconds and allocates 3 times.
- Storage is 1.3× larger at this precision due to the finer level-19 keys
  (16 B/image flat).
