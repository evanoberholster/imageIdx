# Schema — definitions, entities, and IDs

**Status:** current advanced-package guide. Authoritative documentation:
[`schema/README.md`](../schema/README.md) and
[`schema/FORMAT.md`](../schema/FORMAT.md). This is not the retired MessagePack
proposal.

## Responsibilities

`schema` owns:

- entity identity (`EntityKind`) and identifier aliases (`ImageID`, `UserID`,
  `GroupID`, and the entity ID aliases);
- namespace definitions and uniqueness (`Namespace`, name, kind, owner, config,
  projection version);
- durable `schema.dat` container v3, allocation slots, PREPARED/ACTIVE
  publication, and legacy retirement markers;
- entity-scoped ValueID allocation through `NextID`, `NextIDs`,
  `EnsureMinimum`, and `PrepareImport` coordination in `database`.

It does not own application records, index projections, or KV value encoding.
Those belong to the application, `store`, and `database`.

## Definitions

Build a `schema.Definitions` before creating an index or opening a database.
Each namespace has one kind and one owning entity. Namespace numbers and names
are unique; the zero namespace is reserved.

```go
defs := schema.NewDefinitions()
tag, err := index.BitmapDef(bitmapidx.NamespaceSpec{
    Namespace: 101, Kind: schema.KindBitmapEnum,
}, "tags", schema.EntityImage)
if err != nil { return err }
if err := defs.Register(tag); err != nil { return err }
```

The active model is immutable after `schema.Open`. New definitions are an
explicit closed-store operation. `database.Open` coordinates index and KV
additions and publishes PREPARED before rebuilding derived resources.

## Persistence

- `schema.dat` container v3 is the V1 baseline.
- Container v2 is read as ACTIVE and upgraded on the next publication.
- Container v1 and allocator v1 are rejected; no silent reinterpretation.
- Definitions blobs v1/v2 are read and rewritten as v3.
- Both superblocks, allocation slots, directories, and definitions are
  checksummed; corrupt or unsupported state fails closed.
- The file lock is exclusive for the lifetime of `Schema`.

Do not add a parallel registry or an `index/idalloc` counter store. Entity
ValueIDs are allocated by `schema.dat` and shared by all namespaces of an
entity. Different entity kinds may use the same numeric ID.

## Relationship to database

`database.DB` is the stable application entry point. It owns a `schema.Schema`,
writes `schema.dat`, and uses the persisted definitions for canonical namespaces
and index recovery. Direct `schema.Open` is an advanced tool/migration path.

See [`schema/FORMAT.md`](../schema/FORMAT.md) for byte layouts, fixtures, and
upgrade rules.
