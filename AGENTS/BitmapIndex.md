# Bitmap Index Implementation

This document describes the implemented `bitmapidx` package. It is an
implementation reference, not a proposal. It merges the former `BitmapIndex.md`
(predicate index), `BitmapIndexV3.md` (unified-fields proposal), and
`BitmapGeoMerge.md` (geo consolidation): those files are
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
deltas, packed predicate/field persistence, and recovery. Bitmap
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
currently share the same lookup and mmap persistence layout. The distinction
is reserved for future cache-policy tuning.

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

### Persistent fields

```go
store, err := bitmapidx.NewPersistentFields(
    dir, []bitmapidx.Field{tag}, bitmapidx.FieldConfig{}, bitmapidx.DurabilityOS,
)
// Handle err, then retain the store for the lifetime of its field handles.
tags, ok := store.Lookup(tag.Type)
err = tags.Add(bitmapidx.Entry{ImageID: 100, Value: bitmapidx.StringValue("Trekking")})
err = store.Checkpoint()
err = store.Close()

store, err = bitmapidx.OpenPersistentFields(dir, bitmapidx.FieldConfig{}, bitmapidx.DurabilityOS)
```

`FieldStore` persists Enum, Bool, Text, Continuous, and Geo fields. The registry
is fixed at creation; reopen obtains definitions, codec parameters and collision
slots from the checkpoint. `FieldStore.Fields` returns copied handle lists;
`Lookup` is lock-free. All child `Add`, `Remove`, `Apply`, `Move`, and
`ReplaceText` calls automatically pass through the store's serialized WAL
writer. Close the store, not individual store-owned fields. Caller-supplied old
values/text remain required; persistence adds no reverse index.

Atomicity is one field batch, not a transaction across fields. Continuous values
and WAL keys preserve the full int64 domain. Text WAL records store canonical
token changes, so replay does not rerun the analyzer. Canonical bytes accompany
hashed WAL groups; hash collisions resolve by canonical value during replay.
Raw collision keys are not durable handles across vacuum/recovery; retain values
or re-resolve them with `KeyForValue`.

`DurabilityOS` (zero) acknowledges after a complete WAL append and atomic root
publication, without fsync. Power loss can lose unsynced acknowledgments.
`DurabilitySync` acknowledges only after WAL fsync and publication. A write or
sync failure returns `ErrRecoveryRequired`, leaves the previous root visible,
and prevents further writes/checkpoints until close/reopen. The failed operation
may appear after recovery if its complete frame reached disk; callers must treat
that outcome as uncertain and reconcile against their canonical source.

Persistent field batches admit at most `MaxFieldBatchUpdates` (4,096) canonical
field-value membership changes and `MaxFieldBatchBytes` (16 MiB) encoded payload.
A two-value move counts twice; atomic batches are never silently split.
Preparation additionally caps raw membership entries at 65,536 and supplied
value/text bytes at 16 MiB to bound work before canonicalization. These byte
defaults are conservative admission limits, not measured recovery-time promises.

Field-store files, all little-endian and CRC32/IEEE checked:

- `manifest`: 48 bytes, magic/version, generation at byte 8, field count at
  byte 16, and CRC at byte 44. It selects a snapshot and same-generation WAL.
- `fields-<generation>.idx`: 64-byte header, versioned registry encoding,
  8-byte-aligned 64-byte sorted value directory, then an initially page-aligned
  payload region containing 8-byte-aligned roaring postings and canonical bytes.
  Header fields are generation (8), field count (16), registry offset/length
  (24/32), value count (40), directory offset (48), body CRC (56), header CRC (60).
  Value records contain Type/Slot (0/2), Hash (8), cardinality (16), posting
  offset/length (24/32), canonical offset/length (40/48); remaining bytes are
  reserved. Dead dictionary entries have zero cardinality and no posting.
- `fields-<generation>.wal`: one active append-only logical log for all fields.
  Frames have 48-byte headers: magic/version (0/4), field type (6), payload
  length (8), group/update counts (16/20), generation (24), frame offset (32),
  payload/header CRCs (40/44). Each 24-byte group header carries hash, canonical
  length, add/remove counts; canonical bytes and sorted unique uint64 ID columns
  follow, with trailing padding to eight bytes. Removal wins within a batch.

`Checkpoint` holds store/field writer locks while building the complete snapshot;
readers continue using old immutable roots. It syncs the preceding WAL, writes
and validates/maps the successor snapshot, creates and syncs an empty successor
WAL, syncs the directory, then publishes the manifest last. Only after successful
manifest publication does it atomically replace each field root. This serialized
writer boundary prevents dropping a concurrent write suffix. Background workers
register their mmap reads and recheck Frozen identity before publication.

After publication, prior readers/workers drain before the old mapping is
unmapped. Old WAL generations are reclaimed; `Vacuum` also removes obsolete
snapshot files and dead dictionary values. Checkpoints are caller-triggered:
schedule them according to WAL growth and the application's recovery budget.
There is no automatic checkpoint scheduler or partial-field disk compaction.

Recovery validates headers, bounds, checksums, field definitions, keys and bitmap
contents; replays frames from the selected WAL; and truncates only a verified
incomplete terminal frame before accepting new writes. Complete corrupt frames
fail closed. Unpublished successor files do not override the manifest. A failed
manifest publication also requires close/reopen, since its durable outcome may
be uncertain.

Both persistent store APIs require an empty directory for creation and enforce
exclusive writable ownership with an OS advisory lock. The lock file is never
unlinked. A second writer gets `ErrStoreBusy`; recreation gets `ErrStoreExists`.
This backend targets the Unix local-filesystem mmap/flock environment already
used by the library; network-filesystem locking/durability is not promised.

Public bitmap results from persistent indexes own their bytes before internal
reader tracking is released, so they remain valid across checkpoint or close.
Contains/filter reads still use the mmap without copying. Dictionary iteration
callbacks borrow read-only bytes for the call only and must not invoke writes,
maintenance, or close while holding that read; use `LookupValue` for an owned copy.

### Persistent predicates

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

Child `Index.Apply` routes through its owning store and WAL. Vacuum generation
selection/deletion is serialized with checkpoint publication. Recovery repairs
verified incomplete WAL tails before appending; failed WAL writers or uncertain
manifest publication cannot checkpoint over an uncommitted mutation.

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
