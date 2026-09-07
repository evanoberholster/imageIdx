# Geographic Index Implementation

This document describes the implemented `geoidx` package.

## Purpose and Coordinate Model

`geoidx` maps one image to one S2 cell. Coordinates are normalized to S2
level 16 at ingest. The stored `S2Key` is the level-16 cell ID shifted right by
28 bits; zero is reserved as the absent mutation sentinel.

The index is a conservative candidate index. Search input is S2 cell IDs, not
latitude/longitude bounds. Coarser cells at levels 0 through 16 are expanded to
inclusive ranges of level-16 keys. Exact polygon or coordinate verification
belongs to the canonical-data layer.

## Public API

```go
geo, err := geoidx.New(geoidx.Spec{
    IndexID: 7,
    Flags:   0,
    Entries: []geoidx.Entry{
        {ImageID: 1, Latitude: 37.7749, Longitude: -122.4194},
    },
}, geoidx.Config{})

err = geo.Apply([]geoidx.Mutation{
    {ImageID: 1, From: oldKey, To: newKey},
})
err = geo.Flush()
```

Mutations are explicit transitions:

- `From == 0, To != 0`: insert;
- `From != 0, To == 0`: remove;
- both nonzero: move.

This avoids a database-sized reverse image-to-cell map. The caller supplies
the canonical previous cell and mismatched transitions return
`ErrInvalidMutation`.

`Metadata()` returns `IndexID`, flags, generation, sparse-cell count, and
dense-cell count. Stable identity matches the bitmap and timeidx metadata
contract.

## Read Representation

The immutable view is divided into 256 shards. Each shard contains two SoA
representations:

- sparse cells: sorted keys plus four fixed `uint64` image-ID slots per cell;
- dense cells: sorted keys plus immutable `idset.Ref` values.

Cells containing one through four IDs are sparse; cells containing five or
more IDs are dense. Publication rebuilds only affected shards and atomically
publishes a complete replacement view. Readers never inspect writer-side
pending mutations and never lock.

Small writes become visible when `PublishThreshold` or `PublishInterval` is
reached. `Flush` is the explicit visibility boundary. The index owns an
internal reader tracker: search methods register before loading a view, and
`Close` waits for prior readers before unmapping storage. Callers do not create
or pass read guards.

## Search and Covering

`RangesFromCellIDs` validates, normalizes, sorts, and merges query cells.
`SearchRanges` and `EstimateRanges` operate on those ranges. `WithinCells`
combines conversion and search for standalone use; `search.WithinCells` builds
the cross-index clause.

`CoverRegion` is an optional conversion helper for an `s2.Region`. A caller can
supply `CoverOptions{Workspace: ...}` plus spare destination capacity. In that
configuration the cover implementation never grows storage; insufficient
capacity returns `ErrCoverWorkspaceFull` or `ErrCoverBufferFull`. The region's
own methods may still allocate.

`Nearest` currently scans candidates and uses the caller's coordinate lookup.
An expanding-cell nearest executor is not implemented.

## Persistence

Generation format version 2 has a 128-byte header followed by aligned SoA
regions:

- sparse keys (`uint64`);
- sparse image-ID slots (`4 * uint64` per key);
- dense keys (`uint64`);
- dense metadata (24 bytes per key);
- page-aligned native `idset` payload bytes.

All region and payload starts are multiples of eight. Native bitmap bytes are
not widened or rewritten; trailing bytes outside recorded payload lengths pad
the generation file to an eight-byte boundary. The generation header and each
region have CRC32/IEEE protection.

The manifest is 40 bytes. WAL headers and mutation records are each 24 bytes.
All formats are little-endian and structurally eight-byte aligned. Persistent
constructors accept the shared durability value explicitly.

Version 2 adds the required trailing alignment padding and does not open
version-1 generation files.

## Invariants

- Exactly one canonical cell exists per indexed image.
- `ImageID(0)` and `S2Key(0)` are reserved by this package.
- Reader-visible state is immutable and published atomically.
- Sparse and dense columns remain structure-of-arrays.
- Search takes no locks and allocates nothing with adequate caller storage.
- Search input to the combined scheduler is cell IDs.
- Stable identity and flags survive persistence.
- Structural boundaries are eight-byte aligned without changing native
  element widths or bitmap encodings.
