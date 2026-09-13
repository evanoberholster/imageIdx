# Bitmap Index Implementation

This document describes the implemented `bitmapidx` package. It is an
implementation reference, not a proposal. It merges the former `BitmapIndex.md`
(predicate index), `BitmapIndexV3.md` (unified-fields proposal, now implemented
in memory), and `BitmapGeoMerge.md` (geo consolidation): those files are
removed.

## Purpose

`bitmapidx` owns image-metadata membership. It exposes two handles:

- `Index`: one predicate as a set of database-wide `imageidx.ImageID` values
  (tag, person, album, location). Stable identity is `Key{Type, ValueID}` plus
  `IndexID`. Callers assign numeric value IDs.
- `FieldIndex`: one metadata field holding many values (enum, bool, text,
  continuous, geo). Values are hashed or grid-encoded by the index, so callers
  keep no numeric dictionary. One `Field` definition is registered per
  `IndexType` in a `Registry`.

The package is responsible for membership, immutable publication, bounded
deltas, packed persistence (predicate store only), and recovery. Bitmap
representation belongs to `roar`; cross-index scheduling belongs to `search`.

## Public API: predicate index

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
Readers use `Contains`, `AppendTo`, `And`, `Or`, `Count`, or `Filter`. If the
same ID is both added and removed, removal wins.

`NewPersistent` and `OpenPersistent` return a `Store`, which owns a directory
of indexes and one packed segment generation. Durability is an explicit
constructor argument and uses the shared `imageidx.Durability` type.

## Public API: fields

```go
tag := bitmapidx.Field{
    Type:       bitmapidx.IndexTypeTag,
    Kind:       bitmapidx.KindEnum,
    Storage:    bitmapidx.StoragePerType,
    Normalizer: bitmapidx.CodecSpec{Code: bitmapidx.NormalizerIdentity},
}
registry := bitmapidx.NewRegistry()
if err := registry.Register(tag); err != nil { ... }

tags, err := bitmapidx.NewFieldIndex(tag, bitmapidx.FieldConfig{})
err = tags.Add(bitmapidx.Entry{ImageID: 100, Value: bitmapidx.StringValue("Trekking")})
match, err := tags.MatchValues(bitmapidx.StringValue("Trekking")) // AND of values
```

Kinds: `KindEnum` (canonical discrete bytes), `KindBool` (explicit
false/true; missing is neither), `KindText` (analyzer tokens), `KindContinuous`
(`int64` grid values), `KindGeo` (level-16 S2 cell keys). `StoragePerValue`
suits low-cardinality fields, `StoragePerType` high-cardinality ones; both
mutate and query the same way, and the distinction is reserved for the
persisted tier.

- Enum/Bool: `MatchValues` (AND of values; empty matches nothing),
  `CountValues`, `ValueIDs`, `FilterValues`, `Contains`, `ContainsKey`.
  `Compile`/`CompileText` build a reusable `Query` (the zero value matches
  nothing) with `Match`/`Count`/`Filter` execution.
- Text: `MatchText` (AND of analyzed tokens; zero tokens match nothing),
  `ReplaceText` (whole-field diff from caller-supplied before/after).
- Continuous: `Range(Between/AtLeast/AtMost)` (union over occupied grid keys),
  `EstimateRange`, `CountRange`, `FilterRange`; writes use explicit `Move`
  transitions with `HasFrom`/`HasTo` flags because zero is a valid value.
- Geo: see below.
- Dictionary: `KeyForValue`, `LookupValue`, `Iterate`, `Prefix`.
  `Compact` folds deltas; `Vacuum` additionally drops values with no remaining
  posting. `FieldMutation` is Add + Remove atomically; remove wins per
  `(key, ImageID)`.

Value identity: enum/bool/text keys are xxh3-64 hashes with domain separation
over type, kind, length, and canonical bytes. Rare collisions claim a
`FieldKey.Slot`; postings are keyed by full `FieldKey{Hash, Type, Slot}`, so
colliding values never share membership. Continuous keys store the
order-preserving encoding of the grid value with slot zero.

## Geo: one field kind

Geo is a `KindGeo` field in `FieldIndex`, not a separate package or engine.
Each image holds one level-16 S2 bucket key. API lives in `bitmapidx/geo.go`:
`NewGeoField`, `KeyFromLatLng`, `KeyFromCellID`, `CellIDFromKey`,
`CellIDFromLatLng`, `EncodeS2Key`/`DecodeS2Key`, `RangesFromCellIDs`,
`CoverRegion` with `GeoCoverWorkspace`, `GeoRanges`, `FilterGeoRanges`,
`EstimateGeoRanges`, `CountGeoRanges`, `CollectGeoRanges`, `CountGeoCells`,
`Nearest`, `AppendTo`.

Coarser S2 levels are searched lexicographically: `RangesFromCellIDs` expands
levels 0..16 into inclusive level-16 descendant ranges and `GeoRanges` unions
the effective postings inside them. Normalization merges overlapping or
adjacent ranges only; a one-key gap stays split, which loses nothing because
even keys never decode to level 16 (`CellIDFromKey` rejects them). `search.WithinCells` takes the field plus
cell IDs; the geo stage collects via `CollectGeoRanges`, verifies via
`FilterGeoRanges`, and bounds the choice with `CountGeoCells`. `Nearest`
returns the exact top-K using caller-supplied canonical coordinates.
`CoverRegion` converts an `s2.Region` with reusable workspace scratch.

Writes use `Move`/`Add`/`Remove`. Geo keys are never zero, so presence flags
may be omitted. Stale `From` values and duplicate inserts into the same bucket
are rejected before publication. No reverse image-to-cell map is stored, so a
cross-cell insert without `From` is caller discipline.

## Reader and Writer Model

Each predicate index publishes one private immutable `indexView` through an
atomic pointer: stable identity and flags, base cardinality and generation,
immutable `roar.Bitmap` base, optional frozen and active correction sets. Each
field publishes one immutable `fieldView`: field definition, posting table,
frozen/active `fieldDelta` layers, dictionary view, generation. Reads against
a closed index report empty results, as do queries compiled after close;
writes report `ErrClosed`.

The visible bound is always Base + Frozen + Active. Readers load one view and
never lock. Writers serialize through `writerMu`, build complete replacements,
and publish once. Background compaction folds Frozen into Base. Every public
read registers with an internal reader tracker before loading the view, and
reclamation waits for readers that could still hold retired state. Standalone
indexes own independent trackers. Indexes opened from one persistent `Store`
share the store's private tracker, without exposing lifetime policy in
`Config`.

`Apply` routes writes according to the active and hard limits (defaults
`DefaultActiveTarget` 2,048, `DefaultHardDeltaLimit` 4,095; fields add
`DefaultFieldMaxDeltaValues` 256). Small changes extend Active, threshold
crossings rotate Active to Frozen, and changes that would violate the layer
bound are folded into a replacement Base. These limits are runtime policy, not
persisted format.

## Search Semantics

All emitted IDs are ascending and unique. Base cardinality is an ordering hint,
not a correctness value for the effective set. Delta corrections are always
rechecked before candidates are returned.

`Filter` is the integration path used by `search`: it retains only
IDs present in every supplied bitmap. The operation registers once with each
distinct internal tracker, so indexes from one persistent store share one
registration. With sufficient destination capacity, the read path allocates
nothing for up to eight distinct trackers, including the common store-backed
case.

## Persistence

Predicate store (segment format version 2):

- `segment-<generation>.idx`: a 64-byte header, sorted 64-byte directory
  entries, and page-aligned serialized `roar` payloads;
- `bitmap.wal`: 48-byte batch headers followed by `uint64` ID columns;
- `manifest`: a 48-byte atomic generation pointer and WAL offset.

Formats are little-endian and CRC32/IEEE checked. Directory entries and all
structural offsets are multiples of eight. Bitmap payload contents keep their
native `roar` encoding; storage adds trailing padding outside the recorded
payload length so every persisted file ends on an eight-byte boundary. Payload
starts are page aligned (4,096 bytes).

Checkpoint writes a new immutable segment, publishes the manifest with
fsync/rename/directory-sync ordering, and records the covered WAL offset.
Vacuum checkpoints and removes obsolete generations.

Version 2 adds the required trailing alignment padding and does not open
version-1 segment files.

Field indexes (including geo) are in-memory only: field persistence (hash
index, value arena, manifest v2, rotating WAL from the former proposal) is not
implemented.

## Invariants

- `ImageID` remains the shared `uint64` alias.
- Published views and `roar.Bitmap` values are immutable.
- Readers take no mutex, manage lifetime registration internally, and
  steady-state search remains allocation-free.
- Views never exceed Base + Frozen + Active.
- Stable identity survives checkpoint and reopen.
- Persisted structural boundaries are eight-byte aligned; page alignment is a
  stricter compatible boundary.
- Specialized index packages do not import one another or `search`.

The package tests cover atomic visibility, delta precedence, compaction,
reclamation, persistence/recovery, alignment, field kinds, hash collisions,
geo ranges/moves/cover/nearest, and zero-allocation reads.
