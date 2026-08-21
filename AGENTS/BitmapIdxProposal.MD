# Proposal: Lock-Free Bitmap Index for Go

## Status

**Proposed**

This document proposes a specialized bitmap-index architecture for a Go-based, file-backed photo database targeting **100M+ images** and **hundreds of thousands of logical bitmap indexes**. The design uses immutable `sroar`-compatible bitmaps, copy-on-write publication, per-bitmap atomic consistency, batched writes, bounded deltas, and packed storage for high read performance.

The primary query workload is high-performance boolean filtering using `AND`, `OR`, and eventually SIMD-optimized multi-bitmap operations.

---

## 1. Goals

The Bitmap Index should provide:

- Lock-free search access.
- Atomic consistency at the individual logical bitmap level.
- Database-wide `uint64` `ImageID` values.
- Efficient `AND` and `OR` operations across many indexes.
- Immutable, read-only Base bitmaps.
- Small immutable delta bitmaps for recent additions and removals.
- Copy-on-write updates rather than mutation of reader-visible state.
- First-class batch writes such as `Write([]uint64)`.
- Efficient handling of transactions that add or remove hundreds or thousands of ImageIDs at once.
- Hundreds of thousands of tag/value bitmap indexes.
- Physical locality between related indexes.
- Background compaction without blocking readers.
- A future path to custom SIMD bitmap kernels.
- Rebuildability from canonical database metadata.

The implementation should favor fixed structures and predictable execution over a general-purpose LSM design.

---

## 2. Logical Index Model

A `BitmapIndex` represents one tag/value predicate.

Examples:

```text
Bitmap(Location, ParisID)
Bitmap(Location, LondonID)
Bitmap(Person, Person123ID)
Bitmap(Album, Album77ID)
```

Strings must not participate in the hot index path. Types and values are represented by fixed-width integer identifiers.

A logical key is conceptually:

```go
type IndexType uint16

type BitmapKey struct {
    Type    IndexType
    ValueID uint64
}
```

`IndexType` identifies the field/category, such as Location, Person, Album, or Tag. `ValueID` identifies the value within that category.

Each concrete bitmap also has a stable `IndexID uint64`.

All values stored inside the roaring bitmap are database-wide `ImageID` values:

```go
type ImageID uint64
```

`ImageID` must remain `uint64` throughout the database and index formats.

---

## 3. Index Types

Example V1 types:

```go
const (
    IndexTypeLocation IndexType = iota + 1
    IndexTypePerson
    IndexTypeAlbum
    IndexTypeTag
    IndexTypeObject
    IndexTypeEvent
)
```

The exact registry is database-defined and versioned independently of query execution.

---

## 4. Core Consistency Model

Atomic consistency is required **per BitmapIndex only**.

A reader of one bitmap must observe either the complete previous state or the complete newly published state. It must never observe a partially applied batch.

Cross-bitmap queries do not require a common mutation generation. For example:

```text
Location:Paris  generation 120
Person:42       generation 731
Album:7         generation 98
```

may legitimately participate in the same query.

This avoids database-wide transaction snapshots and global publication barriers.

---

## 5. Immutable BitmapIndexView

All reader-visible state for a logical bitmap is contained in one immutable view.

```go
type BitmapIndex struct {
    view atomic.Pointer[BitmapIndexView]

    // Writer-private state is not visible to readers.
    writer BitmapWriterState
}

type BitmapIndexView struct {
    Version   uint16
    IndexType IndexType
    Flags     uint32

    IndexID uint64
    ValueID uint64

    // Cardinality of Base only. Deltas are intentionally ignored
    // by the query planner.
    Cardinality uint64

    Generation uint64

    Base   BitmapRef
    Frozen *BitmapDelta
    Active *BitmapDelta
}
```

A reader performs one atomic load:

```go
view := idx.view.Load()
```

Everything reachable from `view` is immutable for the lifetime of that view.

### Fundamental invariant

> Nothing reachable from a published `BitmapIndexView` may ever be mutated.

This is the primary rule that enables lock-free search access.

---

## 6. BitmapRef

The higher-level index should not depend permanently on `*sroar.Bitmap` as its public internal representation.

V1 may wrap `sroar`:

```go
type BitmapRef struct {
    bitmap *sroar.Bitmap
}
```

The abstraction should allow later migration to an mmap-oriented descriptor such as:

```go
type BitmapRef struct {
    SegmentID uint32
    Offset    uint64
    Length    uint32
}
```

or another immutable representation suitable for custom SIMD kernels.

The purpose of `BitmapRef` is to isolate the rest of the database from the concrete roaring implementation.

---

## 7. Base Bitmap

`Base` is the large stable bitmap.

Properties:

- `sroar`-compatible.
- Immutable after publication.
- Read-only for queries.
- Preferably backed by mmap'd segment data.
- Never modified in place.
- Replaced using copy-on-write.
- Its cardinality is stored directly in `BitmapIndexView.Cardinality`.

The logical Base can contain anything from a few ImageIDs to tens of millions of ImageIDs.

---

## 8. Delta Model

Recent mutations are represented by immutable delta bitmaps.

```go
type BitmapDelta struct {
    Additions BitmapRef
    Removals  BitmapRef
}
```

A delta contains two disjoint logical sets:

```text
Additions
Removals
```

The effective bitmap is conceptually:

```text
(Base OR Additions) AND NOT Removals
```

when only one delta is present.

With both Frozen and Active, mutation precedence is:

```text
Active
  ↓
Frozen
  ↓
Base
```

The newest state wins.

Delta cardinality is **not used by the query planner**. Delta size is intentionally insignificant relative to Base and exists only for mutation correction and compaction policy.

Writer-private counters may still track delta size for rotation thresholds.

---

## 9. Base + Frozen + Active

Each bitmap is bounded to three reader-visible layers:

```text
BitmapIndexView
    |
    +-- Base          immutable
    +-- Frozen        immutable / optional
    +-- Active        immutable / optional
```

The implementation must not allow an unbounded chain of deltas.

### Base

Large, stable, immutable roaring bitmap.

### Frozen

Immutable delta awaiting or undergoing Base compaction.

### Active

Current immutable reader-visible delta containing recently published changes.

The writer may maintain additional private pending state, but it is never visible to readers.

---

## 10. Lock-Free Read Model

No `Mutex` or `RWMutex` is permitted in the normal BitmapIndex search path.

A query:

```go
view := idx.view.Load()
```

then operates exclusively on immutable structures.

The read path should contain only:

```text
query-wide reader epoch entry
atomic BitmapIndexView load(s)
immutable bitmap reads
query-wide reader epoch exit
```

There are no per-bitmap read locks.

---

## 11. Safe Reclamation

Atomic publication does not by itself make old memory safe to reuse.

Example unsafe sequence:

```text
Reader loads View V1
Writer publishes View V2
Writer reuses/unmaps memory referenced by V1
Reader continues reading V1
```

The database therefore requires query-wide epoch/RCU-style reclamation.

Conceptually:

```go
epoch := db.readEpoch.Enter()
defer db.readEpoch.Exit(epoch)

paris := parisIndex.view.Load()
person := personIndex.view.Load()
```

Retired objects are reclaimed only after no reader from the relevant prior epoch can reference them.

Objects requiring delayed reclamation include:

- old `BitmapIndexView` instances;
- old Active deltas;
- old Frozen deltas;
- replaced Base bitmap extents;
- retired mmap segment generations.

Epoch entry/exit should occur once per query, not once per bitmap.

---

## 12. Writer-Private Pending State

Reader-visible Active deltas are immutable. Therefore incoming tiny writes first accumulate in writer-private state.

Example:

```go
type BitmapWriterState struct {
    // Private to the owning writer/shard.
    Pending []BitmapMutationOp

    PendingCount uint32
    LastPublish  int64
}
```

The concrete implementation should use fixed-capacity or pooled storage where practical.

Pending state can cancel repeated operations before publication.

For example:

```text
ADD    100
REMOVE 100
ADD    100
```

can be canonicalized to:

```text
ADD 100
```

before constructing a new immutable delta.

---

## 13. First-Class Batch Writes

Batch writes are a fundamental API, not a loop around single-ID writes.

Example APIs:

```go
func (idx *BitmapIndex) Add(ids []uint64) error
func (idx *BitmapIndex) Remove(ids []uint64) error
func (idx *BitmapIndex) Write(m BitmapMutation) error

type BitmapMutation struct {
    Additions []uint64
    Removals  []uint64
}
```

A call such as:

```go
idx.Add(imageIDs) // 1,000 ImageIDs
```

must be processed as one batch.

All changes in that call become visible to readers through one atomic publication for that BitmapIndex.

A reader therefore sees either:

```text
state before all 1,000 additions
```

or:

```text
state after all 1,000 additions
```

and never a partially applied subset.

---

## 14. Batch Does Not Necessarily Mean Base Rewrite

Atomic batch visibility and physical Base compaction are separate concepts.

For example:

```text
Base   = 20,000,000 ImageIDs
Active =        300 mutations
Batch  =      1,000 additions
```

The writer should normally create a replacement immutable Active delta containing the canonical combined changes:

```text
Before:
    Base B0
    Active D0 (~300)

After:
    Base B0
    Active D1 (~1,300)
```

and atomically publish D1.

It should **not** rebuild a 20M-entry Base merely to make 1,000 additions visible.

---

## 15. Write Size Classes

The writer should recognize different mutation sizes.

### Tiny writes

Examples: individual images or small groups.

Accumulate in writer-private pending state and publish when either:

```text
pending operation threshold reached
OR
publish time threshold reached
```

### Medium batches

Examples: hundreds to a few thousand ImageIDs.

Canonicalize the batch directly into an immutable roaring delta and publish it immediately rather than passing every ImageID through the tiny-write path.

### Very large batches

For sufficiently large transactions, rebuilding a replacement Base may become cheaper than maintaining a large delta.

The crossover must be benchmark-driven and may depend on:

- Base cardinality;
- current Active size;
- current Frozen state;
- batch cardinality;
- expected compaction cost;
- bitmap density/container structure.

---

## 16. Delta Thresholds

Delta rotation should use both size and time thresholds.

The initial design target should keep deltas below the relevant `sroar` array-container threshold, with exact thresholds established by benchmark.

Example experimental ranges:

```text
pending publish threshold:  64-256 operations
Active target:              1,024-2,048 canonical mutations
hard delta bound:           below 4,096 where practical
publish timer:              benchmark-derived
```

These values are not part of the persistent format and must remain tunable.

---

## 17. Delta Rotation

Normal state:

```text
Base B0
Frozen nil
Active D0
```

When Active reaches its size/time/maintenance threshold, the writer privately prepares the next view.

Conceptually:

```text
Base B0
Frozen D0
Active D1
```

The complete new state is published with one atomic pointer store.

Readers that loaded the old view continue using the old immutable objects. New readers use the newly published view.

The old objects are reclaimed only after the reader epoch grace period.

---

## 18. Compaction

Frozen is merged into Base in the background.

Conceptually:

```text
B1 = (B0 OR Frozen.Additions)
     AND NOT Frozen.Removals
```

The result is a completely new immutable Base.

After it is complete, publish another view:

```text
Before:
    Base B0
    Frozen D0
    Active D1

After:
    Base B1
    Frozen nil
    Active D1
```

Again, publication is atomic per BitmapIndex.

Compaction must never mutate B0 or D0.

---

## 19. Bounded Layer Invariant

The system must not turn into an unrestricted LSM tree.

Hard goal:

```text
maximum reader-visible layers per logical bitmap = 3
```

namely:

```text
Base + Frozen + Active
```

If write pressure would require another layer, the implementation should accelerate compaction, merge deltas, or apply local write backpressure rather than create an arbitrary Delta 3, Delta 4, etc.

---

## 20. Cardinality and Query Planning

`BitmapIndexView.Cardinality` represents **Base cardinality**.

```go
Cardinality uint64
```

The planner intentionally ignores delta cardinality.

For an AND query:

```text
Person:123         842
Album:77        14,200
Location:Paris 950,000
Favorite      8,400,000
```

the planner can order stable Base operations from smallest to largest.

Delta changes are treated as a small correction layer and do not complicate planning heuristics.

This keeps planning fast and deterministic.

---

## 21. AND Queries

The primary workload includes multi-index intersections such as:

```text
Location:Paris
AND Person:123
AND Album:77
AND Favorite
```

The long-term executor should avoid repeated materialization such as:

```text
X = A AND B
Y = X AND C
Z = Y AND D
```

where possible.

A future optimized API should support fused operations such as:

```go
IntersectMany(...BitmapRef)
```

or an allocation-free iterator/output-buffer interface.

V1 may use existing `sroar` operations while preserving an abstraction boundary for future SIMD kernels.

---

## 22. OR Queries

OR must be a first-class operation:

```text
Location:Paris OR Location:London
```

Delta removals require operand-aware correction. An ImageID removed from Paris may still be present in London and therefore must remain in the OR result.

The executor must preserve logical membership semantics rather than blindly subtracting the union of all removal deltas.

---

## 23. Delta-Aware Query Correction

The large stable bitmap operation should be performed primarily against Base bitmaps.

For delta-affected ImageIDs, the query engine can re-evaluate exact logical membership using:

```text
Active -> Frozen -> Base
```

precedence.

For AND:

```text
present = Contains(A, id)
       && Contains(B, id)
       && Contains(C, id)
```

For OR:

```text
present = Contains(A, id)
       || Contains(B, id)
       || Contains(C, id)
```

Only IDs touched by relevant deltas require this correction path.

The implementation should benchmark correction-based execution against direct roaring operations involving delta bitmaps.

---

## 24. Storage Scale

The database is expected to contain **hundreds of thousands of logical BitmapIndexes**, potentially growing toward millions.

One file per bitmap is prohibited as the primary storage model because it would create excessive:

- filesystem metadata;
- file descriptors/mappings;
- random I/O;
- page-cache fragmentation;
- startup overhead.

Bitmap indexes should be packed into larger immutable segment files.

---

## 25. Segment Storage

Conceptual layout:

```text
bitmap/
    manifest
    segment-000001.idx
    segment-000002.idx
    segment-000003.idx
    ...
```

Each segment contains:

```text
SegmentHeader
BitmapDirectory[]
Bitmap payloads...
```

Bitmap payloads are immutable serialized `sroar`-compatible data.

Related indexes should be physically near one another where practical.

---

## 26. Bitmap Directory Entry

A packed segment directory entry may resemble:

```go
type BitmapDirectoryEntry struct {
    IndexType IndexType
    Version   uint16
    Flags     uint32

    IndexID uint64
    ValueID uint64

    Cardinality uint64

    Offset uint64
    Length uint32

    Generation uint64
}
```

The exact persistent layout should be explicitly versioned and alignment-aware.

---

## 27. Physical Ordering

Within storage segments, bitmap entries should generally be ordered by:

```text
IndexType
ValueID
```

This keeps related logical indexes close together.

Examples:

```text
Location:Paris
Location:London
Location:Berlin
...

Person:1
Person:2
Person:3
...
```

This improves mmap/page-cache locality when queries commonly access related predicates.

The exact grouping policy should be benchmarked against real query distributions.

---

## 28. Segment Sizing

Initial segment-size experiments should consider ranges such as:

```text
64 MB
128 MB
256 MB
512 MB
```

The correct size should balance:

- sequential locality;
- mmap management;
- rewrite cost;
- COW fragmentation;
- startup directory loading;
- operating-system page-cache behavior.

Segment size is an implementation tuning parameter, not a logical-index constraint.

---

## 29. Copy-on-Write Base Replacement

Base payloads are never overwritten in place.

When compaction produces a new Base:

```text
old Base
    +
Frozen
    ↓
new Base extent
```

write the new extent into COW/replacement segment storage, then publish a new `BitmapIndexView` referencing it.

The old Base remains valid for readers until epoch reclamation determines that it is safe to retire.

---

## 30. Segment Vacuum / Rewrite

COW replacement extents will eventually reduce physical locality.

Periodically rewrite live bitmap Bases into new ordered immutable segments.

Conceptually:

```text
old segments + live replacement extents
                ↓
       sequential rewrite
                ↓
new ordered immutable segments
```

Then atomically publish the updated directory/locations and retire the previous segment generation after reader reclamation.

Vacuum should be triggered by measured fragmentation rather than only elapsed time.

---

## 31. WAL

Bitmap indexes are rebuildable from canonical metadata, but recent writes still require recovery semantics appropriate to the database.

A global WAL may contain typed bitmap mutations:

```text
IndexType
IndexID / ValueID
operation
ImageID batch
```

Batch encoding is important because image import operations may update thousands of IDs at once.

The WAL design should avoid creating an independent WAL file for every logical bitmap.

Because indexes are rebuildable, recovery policy may choose between replay and index rebuild based on corruption/state.

---

## 32. Writer Ownership

Readers are lock-free, but writers do not need to be fully lock-free internally.

A writer or writer shard should exclusively own mutable pending state for its assigned BitmapIndexes.

Conceptually:

```text
many producers
      ↓
batched mutation queue
      ↓
writer/shard
      ↓
private mutation state
      ↓
build immutable replacement
      ↓
atomic BitmapIndexView publication
```

No reader ever accesses writer-private structures.

Sharding policy should preserve locality and avoid excessive goroutine/channel overhead. The exact writer count must be benchmark-driven.

---

## 33. Publication Sequence

For a typical batch update:

```text
1. Receive mutation batch.
2. Resolve/cancel redundant operations privately.
3. Load current BitmapIndexView.
4. Build replacement immutable Active/Frozen/Base state.
5. Calculate new Base cardinality if Base changed.
6. Construct complete new BitmapIndexView.
7. Atomically store the new view pointer.
8. Retire replaced objects through epoch reclamation.
9. Schedule compaction if required.
```

No partially constructed object may be reachable from the published view.

---

## 34. Batch Atomicity Example

Current state:

```text
Bitmap(Location, Paris)

Base   B0
Active D0
```

Transaction:

```text
Add 1,000 ImageIDs
```

Writer privately constructs D1 containing the old outstanding Active state plus the new canonical additions.

Publication:

```text
old view -> Base B0 + Active D0
new view -> Base B0 + Active D1
```

One atomic pointer store switches visibility.

Readers never observe only 437 of the 1,000 additions.

---

## 35. Header / View Metadata

The reader-visible logical header is represented directly by `BitmapIndexView` rather than being independently mutable.

Required V1 fields:

```text
Version       uint16
IndexType     uint16
Flags         uint32
IndexID       uint64
ValueID       uint64
Cardinality   uint64
Generation    uint64
Base          BitmapRef
Frozen        optional BitmapDelta
Active        optional BitmapDelta
```

Persistent segment headers may contain additional offsets, lengths, checksums, and format metadata.

---

## 36. Required Invariants

The implementation must maintain the following invariants:

1. `ImageID` is always `uint64`.
2. A BitmapIndex represents one `IndexType + ValueID` predicate.
3. Each concrete bitmap has a stable `IndexID uint64`.
4. All reader-visible state is reachable from one immutable `BitmapIndexView`.
5. A reader loads the view atomically.
6. Nothing reachable from a published view is mutated.
7. Base is immutable.
8. Frozen is immutable.
9. Active is immutable after publication.
10. Mutable pending state is writer-private.
11. Readers never acquire a BitmapIndex mutex or RWMutex.
12. Old memory is not reused until reader-epoch reclamation permits it.
13. Batch writes are atomically visible at the individual bitmap level.
14. Cross-bitmap atomic generations are not required.
15. Base cardinality is used for planning; delta cardinality is ignored by the planner.
16. At most one Frozen and one Active delta are reader-visible.
17. Unbounded LSM-style delta chains are prohibited.
18. Base replacement is copy-on-write.
19. Logical indexes are packed into larger immutable segment files.
20. Indexes should be physically ordered/grouped for query locality.

---

## 37. Suggested Go Package Layout

```text
bitmapidx/
    index.go          // BitmapIndex and public APIs
    types.go          // ImageID, IndexType, keys, flags
    view.go           // immutable BitmapIndexView
    bitmap_ref.go     // sroar/mmap abstraction
    delta.go          // immutable BitmapDelta
    writer.go         // writer-private pending state
    batch.go          // batch mutation APIs/canonicalization
    search.go         // logical bitmap access
    and.go            // AND execution
    or.go             // OR execution
    planner.go        // Base-cardinality planning
    compact.go        // Frozen -> replacement Base
    segment.go        // packed segment storage
    directory.go      // IndexType/ValueID -> bitmap location
    manifest.go       // segment generations/publication
    mmap.go           // immutable mapping lifecycle
    reclaim.go        // query epochs / retired objects
    wal.go            // global typed bitmap WAL integration
    format.go         // persistent binary ABI
    simd.go           // future optimized kernels/fallback
```

---

## 38. Implementation Order for an Agent

1. Define `ImageID`, `IndexType`, `BitmapKey`, flags, and persistent format constants.
2. Implement `BitmapRef` over `sroar` while keeping the abstraction independent of `sroar` APIs.
3. Implement immutable `BitmapDelta` with Additions and Removals.
4. Implement immutable `BitmapIndexView` and atomic publication.
5. Implement per-bitmap Add/Remove/Write batch APIs.
6. Implement writer-private pending mutation canonicalization and cancellation.
7. Implement Active replacement publication for tiny and medium batches.
8. Implement Frozen rotation with the hard bounded-layer invariant.
9. Implement background Base compaction.
10. Implement query-wide epoch reclamation and race tests.
11. Implement Base-cardinality planner ordering.
12. Implement correct delta-aware AND execution.
13. Implement correct delta-aware OR execution.
14. Implement packed immutable segment files and bitmap directories.
15. Implement COW replacement extents and segment-generation publication.
16. Implement global typed WAL/recovery.
17. Implement segment vacuum/rewrite.
18. Benchmark all thresholds and storage layouts.
19. Add SIMD/fused multi-bitmap kernels only after scalar/sroar correctness is stable.

---

## 39. Required Tests

At minimum:

- atomic visibility of a 1,000-ID batch;
- concurrent readers during repeated view publication;
- race-detector validation of all view/delta/Base lifetimes;
- add/remove cancellation correctness;
- Active/Frozen/Base precedence;
- AND correctness with additions and removals in multiple operands;
- OR correctness where one operand removes an ID retained by another operand;
- Base compaction correctness;
- publication while compaction is running;
- safe epoch reclamation of old deltas and mmap segments;
- no unbounded delta creation under write pressure;
- WAL replay of batch mutations;
- segment replacement/vacuum while readers use old generations;
- hundreds-of-thousands-index directory lookup behavior.

---

## 40. Required Benchmarks

Benchmark at realistic scale, including synthetic 100M-ImageID universes where practical.

Measure:

- atomic view-load overhead;
- single BitmapIndex membership/search;
- 2-, 4-, 8-, and 16-way AND;
- 2-, 4-, 8-, and 16-way OR;
- sparse versus dense Base bitmaps;
- no-delta versus Active/Frozen delta cases;
- 1, 10, 100, 1,000, and 10,000-ID batch writes;
- Active delta thresholds below 4,096;
- direct delta publication versus Base rebuild crossover;
- compaction throughput;
- mmap/page-fault behavior for physically adjacent versus fragmented indexes;
- segment sizes from approximately 64-512 MB;
- allocation counts on query paths;
- query latency under concurrent ingestion;
- epoch enter/exit overhead;
- future scalar versus SIMD/fused kernels.

---

## 41. V1 Acceptance Criteria

V1 is acceptable when:

- all BitmapIndex reads execute without per-index locks;
- every published reader-visible object is immutable;
- per-bitmap batch writes are atomically visible;
- a 1,000-ID batch does not require 1,000 independent publications;
- Base is never modified in place;
- read amplification is bounded to Base + Frozen + Active;
- query planning uses Base cardinality without inspecting delta cardinality;
- AND and OR remain logically correct with delta additions/removals;
- old memory/mappings are safely reclaimed under concurrent readers;
- hundreds of thousands of indexes can be represented without one file per bitmap;
- segment layout preserves useful physical locality;
- persistent structures can be rebuilt from canonical database metadata;
- benchmarks establish the initial delta, batch, compaction, and segment thresholds.

---

## 42. Design Principle

The Bitmap Index is a **copy-on-write, immutable-snapshot bitmap engine**, not a mutable roaring index protected by locks.

The central model is:

```text
writer-private mutations
          ↓
construct immutable state
          ↓
atomic per-bitmap publication
          ↓
Base + Frozen + Active
          ↓
lock-free readers
```

Performance comes from keeping large `sroar` Bases immutable and physically local, keeping deltas small and bounded, making batches first-class, eliminating locks from search, and preserving an abstraction boundary for future SIMD-optimized multi-bitmap execution.
