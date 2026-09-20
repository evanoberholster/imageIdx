# Bitmap Index Implementation

`bitmapidx` is a field-oriented, persistent metadata index. The package owns
immutable publication, bounded deltas, typed value encoding, mmap snapshots,
and WAL recovery. Bitmap representation belongs to `roar`; cross-field query
planning and transient mutable bitmap accumulation belong to `search`.

## Public model

`Field` defines one registered field. `FieldIndex` owns all postings for that
field and supports these kinds:

- `KindEnum`: canonical discrete bytes, including tags, camera models, and
  other multi-valued metadata;
- `KindBool`: explicit true/false membership, distinct from missing;
- `KindText`: analyzed token postings with whole-document replacement;
- `KindContinuous`: ordered `int64` grid values and range queries;
- `KindGeo`: level-16 S2 bucket keys and cell-range queries.

One `Field` is registered per `IndexType` in a fixed `Registry`. Values are
canonicalized and hashed or order encoded by the field definition, so callers
do not maintain one bitmap predicate object per value.

```go
tag := bitmapidx.Field{
    Type:       bitmapidx.IndexTypeTag,
    Kind:       bitmapidx.KindEnum,
    Storage:    bitmapidx.StoragePerType,
    Normalizer: bitmapidx.CodecSpec{Code: bitmapidx.NormalizerIdentity},
}

store, err := bitmapidx.NewPersistentFields(
    dir, []bitmapidx.Field{tag}, bitmapidx.FieldConfig{}, bitmapidx.DurabilityOS,
)
if err != nil { /* handle */ }
defer store.Close()

tags, _ := store.Lookup(tag.Type)
err = tags.Add(bitmapidx.Entry{
    ImageID: 100, Value: bitmapidx.StringValue("Trekking"),
})
match, err := tags.MatchValues(bitmapidx.StringValue("Trekking"))
```

`NewPersistentFields` creates a store with an empty fixed registry;
`OpenPersistentFields` restores the persisted registry and WAL tail.
`FieldStore.Lookup` returns store-owned handles. Close the store, not those
handles. A field batch is atomic; transactions across fields are not provided.

Enum/bool/text queries use `MatchValues`, `MatchText`, `ValueIDs`, `CountValues`
and `FilterValues`. Continuous fields use `Move`, `Range`, `CountRange` and
`FilterRange`. Geo fields use `Move`, `GeoRanges`, `FilterGeoRanges`,
`RangesFromCellIDs`, and `WithinCells` through `search`.

`Compile` and `CompileText` produce reusable field `Query` values. Queries read
the current published field view, so writes after compilation are visible.
Text replacement requires caller-supplied before/after text because no reverse
document index is retained.

## Reader and writer model

Each field publishes one immutable `fieldView` through an atomic pointer. The
view contains the base posting table, optional frozen and active correction
layers, dictionary view, field definition, and generation. Reads register with
the internal reader tracker, take no mutex, and never observe partial writes.

Writers serialize through the field writer mutex. Small changes extend Active;
threshold crossings rotate Active to Frozen; background compaction folds
Frozen into Base. The visible bound is always Base + Frozen + Active.

`StoragePerValue` and `StoragePerType` currently share the same lookup and mmap
layout; the distinction is reserved for future cache policy. Continuous and
geo values use explicit `Move` transitions. Geo keys are never zero, so geo
presence flags may be omitted.

## Persistence

`FieldStore` writes one fixed-registry snapshot generation and one active WAL
per generation. Checkpoint writes and validates a successor mmap snapshot,
creates its successor WAL, and publishes the manifest last. Vacuum also removes
obsolete generations and dead dictionary entries. Persisted codec definitions
are immutable; changing a field's kind, normalizer, analyzer, quantizer, or geo
codec requires a rebuild into a new directory.

The field store uses `FieldConfig` for delta and batch limits and the shared
`imageidx.Durability` values. Writes through store-owned fields automatically
use the store WAL. A complete WAL frame is replayed idempotently; only an
incomplete terminal frame is repaired.

## Search integration

The `search` package is the only cross-index consumer. It accepts typed field
clauses:

```go
plan, err := search.Compile(
    search.FieldValues(tags, bitmapidx.StringValue("Trekking")),
    search.FieldText(title, "golden bridge"),
    search.FieldRange(focalLength, bitmapidx.Between(24, 70)),
    geoClause,
)
```

Field stages use field estimates and candidate filters. Geo stages may use the
search workspace's transient `roar.MutableBitmap` to collect covered cells;
that mutable bitmap is search scratch only and is never part of persistent
field state. Search returns sorted, unique `ImageID` values.

There is no generic predicate `Index`, predicate `Store`, caller-assigned
`ValueID`, or bitmap-level `And`/`Or` API in `bitmapidx`. Applications query
typed fields and let `search` combine them.
