# Bitmap Index Implementation

This document describes the implemented `bitmapidx` package. It is an
implementation reference, not a proposal.

## Purpose

An `Index` represents one predicate as a set of database-wide
`imageidx.ImageID` values. Examples are a tag, person, album, or location.
The stable predicate identity is `Key{Type, ValueID}` plus `IndexID`.

The package is responsible for predicate membership, immutable publication,
bounded deltas, packed persistence, and recovery. Bitmap representation belongs
to `idset`; cross-index scheduling belongs to `search`.

## Public API

```go
index, err := bitmapidx.New(bitmapidx.Spec{
    Key:     bitmapidx.Key{Type: bitmapidx.IndexTypeTag, ValueID: 12},
    IndexID: 44,
    Flags:   0,
    IDs:     []imageidx.ImageID{1, 2, 3},
}, bitmapidx.Config{})

err = index.Apply(bitmapidx.Mutation{
    Add:    []imageidx.ImageID{4},
    Remove: []imageidx.ImageID{1},
})
```

`New(Spec, Config)` is the only in-memory constructor. `Metadata()` returns a
copy containing `Key`, `IndexID`, `Flags`, base cardinality, and generation.
Readers use `Contains`, `AppendTo`, `Intersect`, `Union`, or
`FilterCandidates`. If the same ID is both added and removed, removal wins.

`NewPersistent` and `OpenPersistent` return a `Store`, which owns a directory
of indexes and one packed segment generation. Durability is an explicit
constructor argument and uses the shared `imageidx.Durability` type.

## Reader and Writer Model

Each index publishes one private immutable `indexView` through an atomic
pointer. A view contains:

- stable identity and flags;
- base cardinality and generation;
- immutable `idset.Ref` base;
- optional frozen and active correction sets.

The visible bound is always Base + Frozen + Active. Readers load one view and
never lock. Writers serialize through `writerMu`, build complete replacements,
and publish once. Background compaction folds Frozen into Base. Every public
read registers with an internal reader tracker before loading the view, and
reclamation waits for readers that could still hold retired state. Standalone
indexes own independent trackers. Indexes opened from one persistent `Store`
share the store's private tracker, without exposing lifetime policy in
`Config`.

`Apply` routes writes according to the active and hard limits. Small changes
extend Active, threshold crossings rotate Active to Frozen, and changes that
would violate the layer bound are folded into a replacement Base. These limits
are runtime policy, not persisted format.

## Search Semantics

All emitted IDs are ascending and unique. Base cardinality is an ordering hint,
not a correctness value for the effective set. Delta corrections are always
rechecked before candidates are returned.

`FilterCandidates` is the integration path used by `search`: it retains only
IDs present in every supplied bitmap. The operation registers once with each
distinct internal tracker, so indexes from one persistent store share one
registration. With sufficient destination capacity, the read path allocates
nothing for up to eight distinct trackers, including the common store-backed
case.

## Persistence

A persistent store contains (segment format version 2):

- `segment-<generation>.idx`: a 64-byte header, sorted 64-byte directory
  entries, and page-aligned serialized `idset` payloads;
- `bitmap.wal`: 48-byte batch headers followed by `uint64` ID columns;
- `manifest`: a 48-byte atomic generation pointer and WAL offset.

Formats are little-endian and CRC32/IEEE checked. Directory entries and all
structural offsets are multiples of eight. Bitmap payload contents keep their
native `idset` encoding; storage adds trailing padding outside the recorded
payload length so every persisted file ends on an eight-byte boundary. Payload
starts are page aligned (4,096 bytes).

Checkpoint writes a new immutable segment, publishes the manifest with
fsync/rename/directory-sync ordering, and records the covered WAL offset.
Vacuum checkpoints and removes obsolete generations.

Version 2 adds the required trailing alignment padding and does not open
version-1 segment files.

## Invariants

- `ImageID` remains the shared `uint64` alias.
- Published views and `idset.Ref` values are immutable.
- Readers take no mutex, manage lifetime registration internally, and
  steady-state search remains allocation-free.
- Views never exceed Base + Frozen + Active.
- Stable identity survives checkpoint and reopen.
- Persisted structural boundaries are eight-byte aligned; page alignment is a
  stricter compatible boundary.
- Specialized index packages do not import one another or `search`.

The package tests cover atomic visibility, delta precedence, compaction,
reclamation, persistence/recovery, alignment, and zero-allocation reads.
