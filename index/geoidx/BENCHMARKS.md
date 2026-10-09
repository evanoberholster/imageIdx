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

## Finalize pass (P0 correctness and bounded writes)

Changes since the first comparison, with the same method/datasets.

### Bounded bitmap collection (collector)

`CollectRanges`/`FilterRanges` accumulate in a stack-backed 256-entry buffer that
flushes at capacity, so the collector's own memory is bounded by the chunk, not
the result. Measured over the same native scan (1M, 2 km cover):

| strategy | ns/op | B/op | allocs/op |
|---|---:|---:|---:|
| bounded chunked AddMany (chosen) | 42.6 µs | 2,339 | 1 |
| fully buffered unsorted AddMany | 45.5 µs | 87,299 | 15 |
| fully buffered sorted AddMany | 48.2 µs | 87,300 | 15 |

Chunked AddMany is both the fastest and the only bounded option; unsorted/sorted
buffering are slower and allocate a result-sized temporary.

### Delta layers and background compaction

The write path no longer rewrites the whole base synchronously on a small delta.
Pending mutations live in geometrically merged sorted layers; reaching the
trigger (≥ base/8) starts a background streaming compaction that snapshots the
layers, folds them into a new mmap base, and leaves newer mutations in fresh
layers. Writers are not blocked by the rewrite; a failed compaction retains the
snapshot and retries; `MaxDelta` provides backpressure.

64-entry insert into a 100k base with rotation active:

| config | ns/op | B/op | allocs/op |
|---|---:|---:|---:|
| compaction trigger base/8, large MaxDelta | ~4.6 µs | 29,516 | 4 |

Correctness: `sortIDs` now returns the sorted+deduplicated slice (bitset, radix,
and comparison paths agree, verified against `Sort`+`Compact` over dense, sparse,
duplicate-heavy, empty, and single domains), and the bitset is sized from the
actual maximum so a stale hint cannot overrun it. Radius results require the
exact cell-center distance; nearest-N uses a bounded top-k heap with a
provable stopping condition.

### Still deferred (documented follow-up)

Keeping bitmap-backed predicates as bitmaps across consecutive search stages
(the full "preserve representation across compatible stages" P1) needs a
planner/executor restructure and is left as an explicit follow-up. The current
geo FilterBy path avoids the double conversion for the common cases and the
`search` count path already filters geo against a bitmap directly.

## Review hardening (P0.1–P0.5)

### Compaction lifecycle

Phases: idle → snapshot frozen → build new generation (off-lock) → sync →
commit manifest (atomic) → publish in-memory view → drain readers → reclaim old
generation → idle. Only one compaction publishes at a time (`compacting`).
The old base stays mapped until prior readers drain (`ReaderTracker`); new writes
go to fresh layers and remain visible; a failed compaction retains the frozen
snapshot and a later trigger retries. Cleanup keeps the manifest-referenced
generation (`RemoveObsolete` with the new path) and is best-effort after commit.
The writer mutex is not held during file construction.

### Mutation admission

Admission runs before publication: a batch that would push pending over the hard
budget waits for a running compaction outside the writer mutex, or
returns `ErrBackpressure` (never silently growing after repeated failures). A
single batch larger than the budget is admitted only into an empty delta (bulk
load), after which compaction bounds growth to one batch. A successful return
always means the batch was published.

### Persistent validation

`loadLocked` uses overflow-checked offsets (`checkedBaseFileSize`), rejects
truncated/oversized files, and runs `validateBase` (valid S2 cells, non-zero IDs,
strict `(key,id)` ordering, consistent max ImageID) returning `ErrCorrupt` rather
than panicking. Corruption tests cover every rule plus malformed manifest/base
files.

### Failure injection

A `persistFail(stage)` seam covers create, write, sync, manifest, publish,
reclaim, and remove. `TestCompactionFailureStages` injects each and checks no
committed data loss, no double-apply, reopen, and non-deadlocking Close.

### Pending-delta read overhead (P1.1)

Queries with pending mutations build a per-view membership bitset and a
key-sorted insert list once (cached), so base filtering is O(1) and only in-range
additions are visited. 1M base, 2 km cover:

| pending | ns/op | B/op | allocs/op |
|---|---:|---:|---:|
| 0 | 5.8 µs | 0 | 0 |
| 64 | 7.5 µs | 0 | 0 |
| 4,096 | 7.8 µs | 0 | 0 |
| 65,536 | 10.3 µs | 0 | 0 |

10M uniform: initial 666 µs, collect 1.42 ms, nearest k=10 16.5 µs.

### Migration (P1.4)

`KindBitmapGeo` bitmap namespaces are removed and are not readable. Rebuild a
geographic index from canonical image metadata: open a `geoidx.Index`, compute
`geoidx.KeyFromLatLng` per image, `InsertBatch`, then `Compact`. Non-geographic
bitmap namespaces are unaffected.

### Deferred (scope control)

Follow-ups, out of scope for PR #14: bitmap-backed candidate execution across
all search stages, SIMD geographic kernels, city/named-area lookup, and any new
persistent allocator or spatial planner.

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
