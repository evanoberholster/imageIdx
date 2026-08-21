# GeoIndex Proposal

## Status

Proposed design for a high-performance geographic index in Go.

The GeoIndex combines ideas from the TimestampIndex and BitmapIndex designs:

- fixed-width, sorted Structure-of-Arrays key storage similar to TimestampIndex;
- immutable bitmap bases and copy-on-write publication similar to BitmapIndex;
- no locks on the search path;
- fixed S2 level 16 cells;
- optimized storage for sparse cells;
- roaring/sroar storage for dense cells;
- background mutation folding so searches do not inspect deltas.

The target scale is at least 100 million images.

---

## 1. Goals

The GeoIndex maps one geographic coordinate per image to an S2 level-16 cell and supports:

- bounding-box search;
- arbitrary S2 region search;
- city/country/polygon candidate search;
- approximate-to-near-exact spatial filtering;
- efficient OR across many S2 cells;
- future nearest-photo search;
- batch insertion and coordinate updates;
- mmap-oriented immutable storage;
- SIMD-friendly key and sparse-value access;
- zero-lock reads.

Every database ImageID is a `uint64`.

`ImageID(0)` is permanently invalid and reserved as a sentinel/padding value.

---

## 2. S2 Storage Level

All indexed coordinates are normalized to:

```go
const GeoS2Level = 16
```

No S2 level needs to be stored per entry.

An S2 CellID is a `uint64`. At level 16, the low 28 bits are redundant for this fixed-level index.

The index may therefore normalize keys as:

```go
type S2Key uint64

func EncodeS2Key(cellID uint64) S2Key {
    return S2Key(cellID >> 28)
}

func DecodeS2Key(key S2Key) uint64 {
    return uint64(key) << 28
}
```

Only 36 bits are meaningful after the shift, but the persisted/in-memory representation remains `uint64`.

This intentionally trades approximately 3 bytes of theoretical packing efficiency for:

- natural alignment;
- direct mmap access;
- inexpensive comparisons;
- simple binary/range search;
- SIMD-friendly 64-bit lanes;
- no unpacking step.

---

## 3. One Coordinate per Image

Each ImageID has at most one indexed geographic coordinate.

Therefore, in a published GeoIndex generation:

```text
ImageID -> exactly one S2 level-16 cell
```

A coordinate update is logically:

```text
remove ImageID from old cell
add ImageID to new cell
```

The canonical coordinate should remain available elsewhere in image metadata for exact geometric verification.

The GeoIndex is primarily a high-performance candidate index.

---

## 4. Two Physical Storage Classes

The GeoIndex is one logical index with two physical partitions:

```text
GeoIndex
|
+-- Single/Sparse Index
|     S2Key -> up to 4 ImageIDs
|
+-- Bitmap Index
      S2Key -> immutable roaring bitmap
```

A level-16 S2 cell exists in exactly one partition in a published generation.

### Sparse

Cells containing 1 through 4 ImageIDs are stored in the sparse index.

### Bitmap

Cells containing 5 or more ImageIDs are stored as roaring/sroar bitmaps.

The threshold should be benchmarked, but 4 is the proposed V1 threshold because four `uint64` ImageIDs occupy exactly 256 bits and provide a simple fixed-width representation.

---

## 5. Sparse Index Layout

The sparse index uses a Structure-of-Arrays layout.

```go
type GeoSparseIndex struct {
    Keys     []S2Key
    ImageIDs []uint64
}
```

`Keys` are sorted ascending.

`ImageIDs` contains exactly four `uint64` slots for every key:

```text
Keys:
[K0][K1][K2][K3]...

ImageIDs:
[I0 I1 I2 I3]
[I0 I1 I2 I3]
[I0 I1 I2 I3]
...
```

The value offset for key index `i` is:

```go
offset := i * 4
ids := imageIDs[offset : offset+4]
```

Unused slots contain `ImageID(0)`.

Valid IDs are packed from the first slot forward.

Examples:

```text
one image:
[42, 0, 0, 0]

two images:
[42, 91, 0, 0]

four images:
[42, 91, 123, 500]
```

No count field is required because zero is permanently reserved as an invalid ImageID.

---

## 6. Sparse Storage Cost

Each populated sparse S2 cell consumes:

```text
S2Key              8 bytes
4 x ImageID        32 bytes
---------------------------
                    40 bytes
```

File headers and alignment are excluded.

At 100 million images, the pathological case where every image occupies a different level-16 cell is:

```text
100,000,000 * 40 bytes
= 4.0 GB decimal
~= 3.73 GiB
```

Examples assuming all cells remain sparse:

| Average images per populated cell | Populated cells | Approximate storage |
|---:|---:|---:|
| 1 | 100.0M | 4.00 GB |
| 2 | 50.0M | 2.00 GB |
| 3 | 33.3M | 1.33 GB |
| 4 | 25.0M | 1.00 GB |

The key array itself is only 8 bytes per populated cell, so searches can inspect the compact key stream before touching the larger ImageID stream.

---

## 7. Why Four ImageIDs

Four `uint64` values occupy exactly:

```text
4 * 64 bits = 256 bits
```

This is intentionally convenient for future SIMD implementations.

The sparse representation provides:

- fixed stride;
- no pointers;
- no variable-length records;
- no bitmap/container metadata;
- no per-cell cardinality field;
- direct 256-bit value loads;
- predictable prefetch behavior;
- simple mmap access.

V1 does not require SIMD. Scalar implementations must be correct first.

---

## 8. Bitmap Partition

Dense cells use the existing BitmapIndex concepts.

Conceptually:

```text
S2Key -> BitmapRef
```

The bitmap is an immutable roaring/sroar Base containing database ImageIDs.

The GeoIndex should reuse the common bitmap execution/storage abstraction rather than duplicate roaring implementation code.

A bitmap directory may use:

```go
type GeoBitmapMeta struct {
    Key         S2Key
    Cardinality uint64
    Bitmap      BitmapRef
}
```

Persisted layout should remain SoA-oriented where practical:

```text
geo.bitmap.keys
    []uint64

geo.bitmap.meta
    offsets / lengths / cardinality

geo.bitmap.data
    serialized immutable sroar payloads
```

Bitmap keys are sorted ascending by S2Key.

---

## 9. Why Sparse Cells Should Not Use Stock sroar

Very small stock sroar bitmaps have substantial structural overhead relative to their logical payload.

A cell containing one ImageID logically needs:

```text
8 bytes
```

whereas a standalone stock sroar representation can require far more metadata/storage.

The sparse partition avoids this cost while retaining a uniform logical GeoIndex.

Dense cells continue to benefit from roaring compression and fast set operations.

---

## 10. GeoIndexView

The reader-visible state is immutable and published atomically.

```go
type GeoIndex struct {
    view atomic.Pointer[GeoIndexView]
}

type GeoIndexView struct {
    Version    uint16
    Flags      uint32

    IndexID    uint64
    Generation uint64

    Sparse GeoSparseRef
    Bitmap GeoBitmapRef
}
```

Everything reachable from a published `GeoIndexView` is immutable.

A reader performs one top-level load:

```go
view := geo.view.Load()
```

The reader then uses only data reachable from that view.

No search-side mutex is permitted.

---

## 11. Atomicity Boundary

Atomic consistency is required at the GeoIndex level because cells can migrate between the sparse and bitmap partitions.

Example:

```text
old:
S2 X -> [10, 20, 30, 40] in Sparse

add:
50

new:
S2 X -> roaring{10,20,30,40,50} in Bitmap
```

The writer must privately construct the replacement state and publish a new `GeoIndexView`.

A reader must observe either:

```text
Sparse: {10,20,30,40}
```

or:

```text
Bitmap: {10,20,30,40,50}
```

and never a state where the cell is absent or duplicated because the two files were observed at different publication generations.

This atomicity requirement applies only to GeoIndex, not across unrelated database indexes.

---

## 12. Search Path

A region query follows:

```text
Bounding box / S2 Region / polygon
        |
        v
S2 RegionCoverer
        |
        v
normalize to level-16 cell coverage/ranges
        |
        v
sorted S2Key ranges
        |
        +----------------------+
        |                      |
        v                      v
Sparse key search        Bitmap key search
        |                      |
        v                      v
emit 1-4 ImageIDs        OR immutable bitmaps
        |                      |
        +-----------+----------+
                    |
                    v
              candidate result
                    |
                    v
        optional exact geometry test
```

Search does not inspect write deltas.

---

## 13. Range Search Rather Than Hash Lookup

The GeoIndex should not use a Go `map[uint64]...` as its primary persisted/read representation.

Both sparse and bitmap key arrays are sorted.

The S2 cover should be transformed into sorted key ranges.

For each range:

```go
lo := lowerBound(keys, rangeStart)
hi := upperBound(keys, rangeEnd)
```

Then matching entries are traversed sequentially.

For many sorted ranges, the implementation should benchmark a forward two-pointer merge between:

```text
sorted query ranges
+
sorted S2 keys
```

against repeated binary searches.

Large city/country searches may benefit substantially from sequential scans and prefetching.

---

## 14. Region Covering

The region coverer produces cells representing the requested geometry.

Because storage is fixed at level 16, query coverage must ultimately resolve to level-16 key ranges.

The implementation should avoid unnecessary materialization of enormous lists of individual S2 keys when a hierarchical S2 cell can instead be represented as a contiguous descendant range.

The exact normalization algorithm should preserve sorted, non-overlapping ranges whenever possible.

---

## 15. Exactness

The bitmap union is a candidate result.

For queries requiring near-exact or exact geometry:

```text
GeoIndex candidates
        |
        v
load canonical coordinate for candidate ImageID
        |
        v
point-in-region / bounding-box / distance check
```

Only boundary/ambiguous candidates should require exact verification where an optimization can safely determine that interior cells are fully contained.

This is particularly useful for:

- arbitrary polygons;
- country boundaries;
- city boundaries;
- radius queries.

---

## 16. Search-Time Delta Policy

Unlike the generic BitmapIndex, GeoIndex searches should not traverse Active or Frozen deltas.

The search-visible representation is:

```text
Sparse immutable Base
+
Bitmap immutable Base
```

only.

Writer deltas are private maintenance structures.

This avoids multiplying delta work across potentially hundreds or thousands of S2 cells in a geographic OR query.

---

## 17. Writer-Side Delta

Updates are accumulated privately by the GeoIndex writer.

Conceptually:

```go
type GeoCellMutation struct {
    Key     S2Key
    ImageID uint64
    Op      GeoOp
}
```

The implementation may batch mutations by S2 key.

The writer-side delta is not reachable from `GeoIndexView`.

It may use reusable arrays, temporary roaring structures, or another representation optimized for background rebuilding.

The search path must not depend on its representation.

---

## 18. Background Base Updates

Coordinate changes are expected to be relatively uncommon after initial indexing.

The normal lifecycle is:

```text
immutable published Base
        |
        +-- writer-private mutations
                    |
           size/time threshold
                    |
                    v
          build replacement data
                    |
                    v
          atomic GeoIndexView swap
                    |
                    v
             retire old data
```

A configurable size threshold and time threshold determine visibility latency.

This intentionally favors extremely fast searches over immediate per-operation read visibility.

---

## 19. Batch Writes

Batch insertion must be first-class.

Examples:

```go
func (g *GeoIndexWriter) AddBatch(entries []GeoEntry) error
func (g *GeoIndexWriter) UpdateBatch(entries []GeoUpdate) error
```

A transaction importing 1,000 images should group entries by S2Key and rebuild affected cells efficiently rather than execute 1,000 independent full publication operations.

Batching should amortize:

- sorting;
- S2 conversion;
- WAL overhead;
- cell lookup;
- bitmap construction;
- COW publication.

---

## 20. Sparse Cell Update

If a cell remains at four or fewer ImageIDs, its replacement sparse value is rebuilt privately:

```text
old:
[10,20,0,0]

add 30

new:
[10,20,30,0]
```

The published sparse storage remains immutable.

---

## 21. Sparse-to-Bitmap Promotion

Adding a fifth ImageID promotes the cell:

```text
Sparse:
[10,20,30,40]

+ 50

Bitmap:
roaring{10,20,30,40,50}
```

Promotion occurs during background/replacement construction.

The new GeoIndex generation removes the S2Key from the sparse partition and adds it to the bitmap partition.

The top-level atomic view publication makes this transition consistent.

---

## 22. Bitmap-to-Sparse Demotion

After removals, a bitmap whose cardinality falls to four or fewer ImageIDs may be demoted:

```text
Bitmap:
{10,20,30,40}

->

Sparse:
[10,20,30,40]
```

Demotion should occur during background compaction/rewrite, not as expensive foreground mutation logic.

---

## 23. Physical Storage

Proposed files:

```text
geo/
    manifest

    sparse.keys.base
    sparse.values.base

    bitmap.keys.base
    bitmap.meta.base
    bitmap.data.base

    *.cow
```

Sparse files:

```text
sparse.keys.base
    []uint64 S2Key

sparse.values.base
    []uint64 ImageID
    exactly four values per key
```

Bitmap files:

```text
bitmap.keys.base
    []uint64 S2Key

bitmap.meta.base
    offset/length/cardinality

bitmap.data.base
    immutable serialized sroar
```

The implementation may consolidate these into larger segment files if that improves file management without sacrificing SoA access.

---

## 24. Physical Ordering

Both partitions are stored in ascending S2Key order.

Bitmap payloads should also be written in key order where practical.

This gives geographic queries:

- compact key searches;
- sequential metadata traversal;
- improved page-cache locality;
- predictable readahead;
- fewer random accesses.

Copy-on-write replacement extents may temporarily fragment this ordering.

A periodic generation rewrite/vacuum restores sequential physical layout.

---

## 25. Copy-on-Write

Published Base data is never modified.

Updates create replacement sparse pages, bitmap extents, or index generations.

Publication order is conceptually:

```text
build replacement
write replacement
sync according to durability policy
construct immutable GeoIndexView
atomic Store(view)
retire old generation
```

Old mmap regions/storage cannot be reclaimed until readers that may reference them have completed.

Use the same epoch/RCU-style reclamation mechanism as the other index types.

---

## 26. Memory Policy

The design should avoid persistent duplicate bitmap Bases.

Temporary memory is allowed while rebuilding an affected cell or output generation, but should be:

- bounded;
- reusable;
- worker-owned or pooled;
- absent from the search path.

For large bitmap rebuilds, prefer writing replacement serialized data directly to COW storage when practical.

---

## 27. OR Execution

Geographic search is primarily an OR workload.

For bitmap cells, the implementation should eventually provide an OR-many operation that avoids repeated intermediate bitmap allocation:

```text
undesirable:

R1 = A OR B
R2 = R1 OR C
R3 = R2 OR D
```

Desired future API:

```go
UnionManyInto(dst, bitmaps...)
```

or an equivalent fused/streaming kernel.

Sparse cell IDs can be inserted directly into the result.

V1 should prioritize correctness and allocation profiling before custom SIMD kernels.

---

## 28. Nearest-Photo Search

Nearest-photo search can be added without changing the storage model.

Conceptual algorithm:

```text
query point
    |
    v
small S2 region around point
    |
    v
search matching level-16 cells
    |
    v
collect candidates
    |
if candidates < K:
    expand region
    |
    v
calculate exact distances
    |
    v
Top-K nearest
```

Expansion can stop when unexplored space cannot contain a point closer than the current worst Top-K result.

This should be implemented as a later query feature rather than a different index format.

---

## 29. Headers

A proposed persisted GeoIndex header:

```go
type GeoIndexHeader struct {
    Version uint16
    Flags   uint16

    IndexID uint64

    S2Level uint8 // must equal 16 in V1
    _       [7]byte

    SparseCellCount uint64
    BitmapCellCount uint64

    Generation uint64
}
```

`S2Level` is stored for format validation even though it is constant in V1.

---

## 30. Core Invariants

1. `ImageID` is always `uint64`.
2. `ImageID(0)` is permanently invalid/reserved.
3. Every indexed image has at most one coordinate.
4. Every stored coordinate is normalized to S2 level 16.
5. S2 keys are stored as `uint64(cellID >> 28)`.
6. Every populated S2 cell exists in exactly one published storage partition.
7. Cells with 1-4 ImageIDs use sparse fixed-width storage.
8. Cells with 5+ ImageIDs use immutable roaring/sroar storage.
9. Sparse values always contain exactly four `uint64` slots.
10. Unused sparse slots are zero.
11. Sparse ImageIDs are packed from slot zero upward.
12. Both key arrays are sorted ascending.
13. Reader-visible data is immutable.
14. Searches never inspect writer deltas.
15. Searches never acquire a mutex.
16. The GeoIndex view is the atomic publication boundary.
17. Old generations are reclaimed only after readers are safe.
18. Bitmap operations should avoid intermediate allocations where practical.

---

## 31. Suggested Go Package Layout

```text
geoindex/
    index.go
    types.go
    s2key.go
    cover.go
    ranges.go

    sparse.go
    sparse_search.go

    bitmap.go
    bitmap_search.go

    search.go
    nearest.go

    writer.go
    mutation.go
    compact.go

    storage.go
    manifest.go
    mmap.go
    reclaim.go

    simd.go
    format.go
```

Shared BitmapIndex primitives should live in a common package rather than being copied into GeoIndex.

---

## 32. Implementation Order

1. Define `S2Key`, level-16 normalization, and binary-format invariants.
2. Implement immutable sparse SoA storage.
3. Implement sparse exact-key and range lookup.
4. Implement level-16 S2 cover-to-key-range conversion.
5. Integrate immutable `BitmapRef` values for dense cells.
6. Implement region OR across sparse and bitmap partitions.
7. Add optional exact coordinate verification.
8. Implement private batched writer mutations.
9. Implement sparse replacement and sparse-to-bitmap promotion.
10. Implement bitmap replacement and bitmap-to-sparse demotion.
11. Implement atomic `GeoIndexView` publication.
12. Implement WAL/recovery integration.
13. Implement epoch-safe reclamation.
14. Implement COW storage and sequential generation rewrite.
15. Benchmark and optimize OR-many.
16. Add SIMD only where profiles demonstrate value.
17. Add nearest-photo expansion/search.

---

## 33. Required Tests

- S2 level-16 key encode/decode.
- Invalid S2 level rejection.
- `ImageID(0)` rejection.
- Sparse cells containing 1, 2, 3, and 4 IDs.
- Sparse sentinel termination.
- Sparse key range lookup.
- Promotion from 4 IDs to bitmap.
- Demotion from bitmap to 4 IDs.
- Coordinate move between two S2 cells.
- Region searches crossing S2 cell boundaries.
- Bounding-box searches.
- Arbitrary polygon/country-style searches.
- Exact verification of boundary candidates.
- Atomic promotion while concurrent readers search.
- Atomic generation replacement under Go race detector.
- Recovery at publication boundaries.
- Old-generation reader reclamation.
- Large batch ingestion.
- 100M-image synthetic build/search benchmark.

---

## 34. Required Benchmarks

Measure at minimum:

- exact single-cell lookup;
- small bounding box;
- city-sized region;
- country-sized region;
- sparse-only region;
- bitmap-heavy region;
- mixed sparse/bitmap region;
- number of S2 key ranges per query;
- repeated binary search versus forward range merge;
- scalar sparse emission;
- future SIMD sparse emission;
- pairwise OR versus OR-many;
- allocations per query;
- page faults/cache behavior where measurable;
- promotion/rebuild throughput;
- batch ingest throughput;
- fragmented COW versus sequential rewritten generation.

The steady-state search target is zero heap allocations except for explicitly requested materialized output whose ownership requires allocation.

---

## 35. Design Summary

The proposed GeoIndex is a fixed-level spatial index specialized for photo coordinates:

```text
                         GeoIndex
                            |
                  atomic immutable view
                            |
             +--------------+--------------+
             |                             |
             v                             v
       Sparse S2 Index                Bitmap S2 Index
       sorted uint64 keys             sorted uint64 keys
       4 x uint64 IDs/cell            sroar BitmapRef/cell
       cardinality 1-4                cardinality 5+
             |                             |
             +--------------+--------------+
                            |
                     region OR search
                            |
                            v
                       candidates
                            |
                            v
                 optional exact geometry
```

The central performance principle is:

> Keep geographic search entirely on immutable Base structures. Use fixed-width SIMD-friendly storage for sparse level-16 cells, roaring bitmaps for dense cells, and fold coordinate mutations into replacement Bases in the background.

This preserves the TimestampIndex advantages of sorted sequential SoA access while reusing the BitmapIndex advantages of immutable roaring storage, copy-on-write publication, and lock-free reads.
