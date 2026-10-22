# Phase 2 Follow-up: sparse SimHash design checkpoint

## Chosen design

Bounded in-memory immutable sorted segments plus a small writer delta segment,
persisted as immutable CRC-checked segment files and an atomically replaced
manifest.

## Reader lifetime

`Store.sparseView` is an atomic pointer to an immutable view:

```go
type sparseView struct {
    base    sparseSegment  // oldest on-disk segment
    overlay []sparseRecord // newest-wins merged deltas, including tombstones
    live    uint64
}
```

Readers load the pointer once per query and merge two sorted streams. Segment
record slices are never mutated after publication. Ordinary updates rebuild only
the bounded overlay, never the base population. Files are not mmap'd or read by
query paths, so old segment files can be retired immediately after a new
manifest is durable; only old in-memory slices remain until readers release the
view.

## Publication

For an ordinary update batch:

1. Build a small sorted/unique delta segment, newest-wins within the batch.
2. Write a temporary segment file, checksum it, fsync it, rename it to its
   immutable name, and fsync the directory when durability requires it.
3. If compaction is due, stream-merge old segments plus the delta and write one
   replacement segment instead.
4. Atomically replace `sparse.manifest` with the new segment ID list, live count
   and source generation; fsync the manifest and directory when required.
5. Publish the new immutable in-memory view.
6. Retire segment files no longer referenced by the manifest.

A crash before the manifest replace leaves an orphan segment and an old valid
manifest. A crash after the manifest replace recovers the new view. A fully
written corrupt segment is an error, not a partial-tail truncation. A partial
temporary file is never referenced by a manifest.

## Segment and manifest layout

All integers little-endian.

Segment header (24 bytes): magic `SSG2`, version, header size, u64 segment ID,
u32 record count, reserved. Records are 50 bytes: u64 ImageID, u64 pHash,
32-byte PDQ, quality, flags (0 live, 1 tombstone). A trailing CRC32 covers the
header and all records.

Manifest: magic `SSM2`, version, flags, u64 generation, u64 source generation,
u64 live count, u32 segment count, reserved, segment IDs, trailing CRC32.
Segment IDs are strictly increasing and unique; the manifest generation equals
the newest segment ID. At most `maxSparseSegments` (currently 64) immutable segments are active; the
in-memory overlay is bounded by that segment count and batch size.

## Compaction

Trigger when adding a segment would exceed `maxSparseSegments`, or when stale
records exceed a measurable threshold. The writer first merges only the delta
tier when the segment count is exhausted; a full base+delta merge is reserved for
the stale/delta-record threshold. This avoids copying a large base on every few
updates while keeping the overlay bounded. Merges use a bounded multi-cursor
newest-wins pass: same-ID records choose the highest segment generation.
Tombstones suppress older records. A tombstone at or below the dense high-water
mark is retained after compaction so it continues to hide dense storage;
tombstones above the high-water mark are retired. The full merge is the only
whole-sparse-population copy boundary.

## Replay and generations

Each manifest carries the database/index source generation that the sparse
state represents. Re-applying an interval with `sourceGeneration <= persisted`
is a no-op. A durable segment/manifest with a source generation newer than the
index manifest is safe to retain; replay skips it. A crash before manifest
publication leaves an orphan and replay applies the interval again. Old
`sparse.bin`/`*.sparse` files are rejected with an explicit rebuild error.

## Benchmark plan

Compare the current full-slice COW implementation and the segmented design at
10K, 100K and (opt-in) 1M sparse records for apply batches of 1/16/64, hot-ID
updates, mixed dense/sparse search, pHash/PDQ scalar and bulk search, reopen,
and compaction. Report ns/op, B/op, allocs, retained memory, segment count,
compaction bytes and recovery time. Keep physical-device claims separate from
logical `Stats()` counters.
