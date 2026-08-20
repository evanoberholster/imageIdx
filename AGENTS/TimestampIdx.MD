# Timestamp Index Design for Go

Implementation specification for a 100M+ image, mmap-oriented, copy-on-write database index

Status: Proposed V1 architecture. This document is intended to be sufficiently explicit for an implementation agent to build the timestamp subsystem, tests, benchmarks, and persistence lifecycle without redesigning the core model.

## Goals and Non-Goals

Build a specialized timestamp index for a photo database targeting at least 100 million images. The design prioritizes predictable search latency, sequential storage, zero-allocation reads, bounded read amplification, inexpensive append-oriented writes, and simple crash-safe copy-on-write publication.

- ImageID is a database-wide uint64 and must remain uint64 in every timestamp structure, WAL record, delta, and persisted base.
- Timestamp search must avoid heap allocation in the steady-state query path.
- Stable data must be immutable and suitable for memory mapping.
- Each logical block may expose at most three searchable layers: Base, Frozen delta, and Active delta.
- Stable blocks should be stored in logical timestamp order and as physically sequentially as practical.
- The design is not a general-purpose LSM tree, B-tree, or key/value store.
## Fundamental Types and Timestamp Compression

```go
type ImageID uint64
type Timemarker uint32

const TimestampShift uint8 = 24

func EncodeTimemarker(timestamp, offset uint64) Timemarker {
    return Timemarker((timestamp - offset) >> TimestampShift)
}

func DecodeTimemarker(marker Timemarker, offset uint64) uint64 {
    return offset + (uint64(marker) << TimestampShift)
}
```

The Timemarker is lossy. The low 24 bits of the timestamp delta are discarded. OffsetTime is required so the uint32 conversion represents a bounded delta rather than truncating significant high bits of an absolute uint64 timestamp.

Exact timestamps must remain available in the canonical image metadata/column when exact boundary semantics are required. The compressed index is a candidate locator.

## Structure of Arrays

Do not persist timestamp entries as an array of structs. Persist markers and ImageIDs independently.

```go
type TimestampBase struct {
    Markers  []Timemarker // persisted as uint32
    ImageIDs []ImageID    // persisted as uint64
}
```

The correspondence invariant is Markers[i] -> ImageIDs[i]. The Base arrays are sorted by Timemarker ascending; ImageID ascending should be used as the deterministic secondary ordering for equal markers.

At 100M entries the raw arrays are approximately 400 MB of markers and 800 MB of ImageIDs, or 1.2 GB total before metadata, alignment, and deltas.

## Index and Block Metadata

```go
type TimestampIndexHeader struct {
    Magic       uint32
    Version     uint16
    Flags       uint16

    Count       uint64
    StartTime   uint64
    EndTime     uint64
    OffsetTime  uint64

    Shift       uint8 // V1 must be 24
    _           [7]byte

    Generation  uint64
}

type TimestampBlockMeta struct {
    StartTime    uint64
    EndTime      uint64

    MarkerOffset uint64
    IDOffset     uint64

    Count        uint32
    Flags        uint32

    Generation   uint64
}
```

The block directory is immutable for readers and sorted by StartTime. For approximately 65,536 entries per block, 100M images require roughly 1,526 blocks, so the directory should remain small enough to keep resident in RAM.

## Logical Block Size

Use an entry-count target rather than a calendar-duration target. V1 should begin with a Base target of 65,536 timestamp entries per block and tune only through benchmarks.

- 65,536 markers x 4 bytes = 256 KiB.
- 65,536 ImageIDs x 8 bytes = 512 KiB.
- Raw full Base payload is approximately 768 KiB plus headers/alignment.
- Entry-count sizing keeps search and compaction costs predictable despite uneven photo density over time.
## Base + Double-Delta Model

```go
type TimestampBlock struct {
    View atomic.Pointer[TimestampBlockView]
}

type TimestampBlockView struct {
    Base       *TimestampBase
    Frozen     *TimestampDelta
    Active     *TimestampDelta
    Generation uint64
}
```

Base is immutable and sorted. Frozen is an immutable delta currently being folded into a replacement Base. Active is the append-only write target. A reader loads TimestampBlock.View exactly once and uses only references reachable from that snapshot.

Hard invariant: there is never more than one Frozen delta and one Active delta for a block. The implementation must not grow arbitrary LSM layers.

## Delta Representation

```go
type TimestampDelta struct {
    AddMarkers  []Timemarker
    AddImageIDs []ImageID
    DeleteIDs   []ImageID

    // Implementation may use fixed backing arrays and published counts.
}
```

V1 delta capacity should begin at 2,048 entries and benchmark up to 4,096. Deltas should use preallocated/fixed backing storage and must not grow slices on the write path.

The Active delta does not need to remain timestamp-sorted. Append in arrival order. Search the small marker array with a linear/SIMD range scan. Sorting is deferred to compaction.

Timestamp updates are modeled as delete old ImageID plus add ImageID at the new Timemarker.

## Delta State Machine

```text
Initial:
    Base=B0, Frozen=nil, Active=D0

D0 fills:
    atomically publish Base=B0, Frozen=D0, Active=D1
    enqueue compaction(B0, D0)

Compactor:
    B1 = merge(B0 - D0.Deletes + D0.Adds)

Publish:
    Base=B1, Frozen=nil, Active=D1

Next rotation:
    D1 becomes Frozen and D0 is reused only after safe reclamation.
```

If Active becomes full while Frozen is still outstanding, apply backpressure to writes targeting that block. Do not allocate a third delta. Unrelated blocks and readers must continue normally.

## Writer Concurrency Model

V1 should use one write-owner goroutine for the timestamp index, fed by a bounded batch channel. The writer serializes Active-delta mutation and delta rotation, eliminating mutexes and CAS reservation from the normal append path.

```go
type TimestampWrite struct {
    Timestamp uint64
    ImageID   ImageID
    Op        WriteOp
}

type TimestampWriteBatch struct {
    // Prefer pooled or fixed-capacity batches.
    Writes []TimestampWrite
}
```

- Batch writes (for example 128-256 operations) to amortize channel and WAL overhead.
- Inside the writer, use direct function calls; do not create channels between internal index components.
- Do not shard the writer until benchmarks show it is a bottleneck.
- If sharding is later required, shard by timestamp/block ownership rather than hashing ImageID, preserving locality.
## Compaction

Compaction runs outside the write-owner goroutine. The writer freezes a delta and enqueues a compaction task. A compactor reads immutable Base + Frozen, creates a new sorted Base, and publishes a replacement view only if the expected generation is still valid.

Compaction must be streaming where practical. Merge sorted Base data with sorted/temporarily sorted Frozen additions while excluding deletes. Avoid constructing object graphs or generic maps for the full block.

The write owner must never perform the expensive Base rebuild synchronously.

## Reader and Search Path

A timestamp range query T1 <= timestamp < T2 follows this path:

- Encode T1 and T2 into conservative Timemarker boundaries using the applicable OffsetTime.
- Binary-search the resident BlockDirectory to find overlapping logical blocks.
- For each block, perform exactly one atomic View load.
- Binary-search the sorted Base Markers array to identify [lo, hi).
- Return/read only Base ImageIDs[lo:hi].
- SIMD/linear scan Frozen and Active marker arrays and emit matching ImageIDs while applying delta delete/update semantics.
- Verify exact timestamps only for candidates in lossy boundary markers when exact range semantics are required.
- Use caller/worker-owned scratch and output buffers; steady-state search target is 0 allocs/op.
## Physical Storage for Sequential Reads

Use global Structure-of-Arrays files so marker data and ImageID data are each physically ordered by logical block:

```text
timestamp/
    manifest
    blocks.idx
    markers.base
    ids.base
    markers.cow
    ids.cow
```

markers.base contains [B0 markers][B1 markers]...[BN markers]. ids.base contains [B0 IDs][B1 IDs]...[BN IDs]. This allows broad timestamp searches to traverse compact marker pages without pulling the 8-byte ImageID stream into cache until matches are known.

Block starts should be page aligned (at least 4 KiB). Hot arrays/regions should preserve at least 64-byte alignment where useful for cache lines and SIMD. Avoid excessive padding unless benchmarks demonstrate a benefit.

## Copy-on-Write Replacement Storage

Never overwrite a Base block currently visible to readers. Replacement block arrays are appended to markers.cow and ids.cow. A new immutable block directory/view points the logical block to the replacement extents.

This preserves cheap updates but gradually fragments physical timestamp order. Fragmentation is repaired by a sequential vacuum/rewrite.

## Sequential Vacuum / Generation Rewrite

Periodically build a new generation by streaming the current logical blocks in timestamp order into new sequential markers.base and ids.base files. For each logical block, copy the current live Base whether it resides in BASE or COW.

```text
old BASE + live COW replacements
            |
            v
new markers.base: [B0][B1][B2']...[BN]
new ids.base:     [B0][B1][B2']...[BN]

fsync new files
publish new manifest/directory generation atomically
retire old generation after readers are safe
```

Trigger rewrites from measured fragmentation, not merely time. A starting policy may consider COW live bytes around 10-20% of live Base bytes, but the final threshold must be benchmark-driven. Also track physical runs per representative range query.

## Memory Mapping and Lifetime

Stable Base files are mmap-oriented and addressed by offsets. Persist offsets, counts, and generation metadata rather than Go pointers. Mapped bytes must be interpreted through fixed-width structures with explicit format versioning.

Atomic publication does not make old memory immediately reusable. The implementation needs epoch-based reclamation, RCU-style reader epochs, or another safe mechanism so an old Base/delta/mmap generation is not unmapped or recycled while any reader can still reference it.

## Crash Safety and Durability

Writes must be recoverable through a WAL. The implementation should define durability levels, but the durable ordering for an acknowledged durable write is conceptually: append WAL record/batch, satisfy required sync policy, mutate/publish delta state, then acknowledge.

Base/COW generation changes use copy-on-write publication: fully write replacement data and metadata, sync as required, atomically publish the new manifest/current generation, and retire old files only after safe reader reclamation.

Never depend on partially overwritten index structures being recoverable.

## Required Invariants

## Suggested Go Package Boundaries

```text
timestamp/
    index.go          // public index API
    types.go          // ImageID references, Timemarker, headers
    directory.go      // immutable block directory
    block.go          // block view/state
    delta.go          // fixed-capacity active/frozen deltas
    writer.go         // single-owner batch writer
    search.go         // range execution
    compact.go        // Base + Frozen merge
    storage.go        // base/cow files and offsets
    mmap.go           // mapping helpers
    manifest.go       // generations/publication
    wal.go            // timestamp WAL integration
    reclaim.go        // reader epochs / retired generations
    simd.go           // kernel abstraction/fallback
    format.go         // binary ABI constants and validation
```

## Agent Implementation Order

1. Define binary format constants, typed IDs, headers, block metadata, validation, and endian policy.

2. Implement in-memory sorted Base and fixed-capacity unsorted Delta with unit tests.

3. Implement zero-allocation Base range search and delta range scan with scalar kernels first.

4. Implement TimestampBlockView atomic publication and reader snapshot tests under the race detector.

5. Implement the single-owner batch writer and Base/Frozen/Active rotation state machine.

6. Implement background block compaction and deterministic merge semantics for adds/deletes.

7. Implement block directory binary search and multi-block range queries.

8. Implement base/cow persistence and mmap readers using offsets, not serialized Go pointers.

9. Implement WAL/recovery and generation-safe manifest publication.

10. Implement safe reclamation for old deltas, mappings, and generations.

11. Implement sequential vacuum/rewrite and fragmentation metrics.

12. Add SIMD kernels only after scalar correctness and benchmarks are stable.

## Tests and Benchmarks Required Before V1 Acceptance

- Encoding/decoding and overflow/range tests for Timemarker and OffsetTime.
- Exact range semantics across lossy first/last marker boundaries.
- Duplicate Timemarker ordering and deterministic compaction.
- Add, delete, and timestamp-update correctness across Base/Frozen/Active.
- Delta rotation while concurrent readers continuously query the block.
- Compaction publication under the Go race detector.
- Crash/recovery tests at every publication boundary.
- Vacuum generation swap with readers pinned to the previous generation.
- Benchmarks at 1M, 10M, and synthetic 100M entries.
- Narrow, medium, and broad timestamp range benchmarks.
- 0 allocs/op assertion for steady-state range searches.
- Comparison of 2,048 vs 4,096 delta capacities.
- Comparison of block sizes around 32K, 64K, and 128K entries.
- Sequential Base versus intentionally fragmented COW query benchmarks.
- Scalar versus SIMD delta scan once SIMD is implemented.
## V1 Acceptance Criteria

- Correct operation with uint64 ImageID and uint32 Timemarker persistence.
- Bounded three-layer block reads under all write/compaction states.
- No reader-visible mutation of Base data.
- No unbounded delta creation.
- Zero heap allocations for the normal range-search path after initialization.
- Crash-safe WAL recovery and atomic generation publication.
- Safe reclamation verified under concurrent/race testing.
- 100M-entry synthetic index can be built, opened, queried, compacted, and vacuumed within documented resource targets.
- Benchmarks and profiles are committed alongside the implementation so future optimizations are evidence-driven.
## Design Principle

Treat the timestamp subsystem as a bounded, block-local LSM specialized for compressed time rather than as a generic database tree. The performance advantage comes from fixed-width data, immutable sequential Base storage, global SoA layout, tiny append-only deltas, a hard cap on read layers, and atomic copy-on-write publication.

| Invariant | Requirement |
| --- | --- |
| ImageID | Always uint64 database-wide. |
| Timemarker | Always uint32 in V1; encoded from timestamp delta with >> 24. |
| Layout | Structure of Arrays; marker and ImageID streams remain separate. |
| Base | Immutable and sorted by Timemarker, then ImageID. |
| Active | Append-only, fixed capacity, no allocation on append. |
| Frozen | At most one per block. |
| Read layers | Maximum Base + Frozen + Active. |
| Readers | No write lock; exactly one BlockView atomic load per block. |
| Updates | Copy-on-write; never overwrite reader-visible Base data. |
| Physical order | Stable Base blocks stored in logical timestamp order. |
| Compaction | Background and block-local. |
| Overload | Backpressure one block rather than creating unbounded deltas. |
| Search | Target 0 heap allocations per steady-state query. |
