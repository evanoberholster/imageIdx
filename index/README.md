# index — unified index Store

`index` is the single public entry point that owns and coordinates the six
imageIdx index families in one root directory:

| Namespace kind | Engine | File(s) |
|---|---|---|
| `KindTimestamp` | `timeidx` | `<hex>.timeidx` per namespace |
| `KindGeo` | `geoidx` | `geo-<hex>.idx` per namespace |
| `KindHashPerceptual` | `simhash` | `sim-<hex>.manifest`, `sim-<hex>-shard-*.bin`, `sim-<hex>.wal` |
| `KindHashExact` | `radixhash` | `radix-<hex>.idx` per namespace |
| `KindVector` | `vectoridx` | `vector-<hex>.idx` (+ `.manifest`/`.wal`/`.delta`) per namespace |
| `KindBitmap*` | `bitmapidx` | `manifest`, `namespaces-*.idx`/`.wal` (one Store, many namespaces) |

The unified manifest is `store.bin` (magic `UIDX`, version 1) and the persisted
schema is `schema.bin`. Everything lives in one directory; no per-family or
per-namespace subdirectories are created.

## Entity model

`common.EntityKind` names ten entity kinds (image, face, person, object, album,
event, place, asset, collection, stack). A namespace declares exactly one entity
kind in its `common.NamespaceDef.Entity`; the entity is never stored per record.
The same numeric `common.ValueID` is independent across entity kinds.

Liveness is a single reserved `bitmapidx` enum namespace (`0xFFF0`) whose values
are the entity-kind names. Marking an entity live adds `(id, kindName)`;
deleting removes it. A search restricted to an entity kind therefore cannot
return a deleted entity, and deleting `ImageID 100` never affects `FaceID 100`.

## Namespace schema

Namespaces are registered in a `common.Schema` before `Create`/`Open` and never
at runtime. The schema is persisted (`schema.bin`) and validated on reopen.
`common.Schema` uses version 2, which stores the entity kind; version-1 blobs
load as `EntityInvalid` and are rejected by `validateSchema`.

Helpers encode engine configuration into `NamespaceDef.Config`:

```go
tags, _ := index.BitmapDef(bitmapidx.NamespaceSpec{Namespace: 6, Kind: common.KindBitmapEnum}, "tags", common.EntityImage)
vec, _  := index.VectorDef(vectoridx.Config{Namespace: 5, Dimensions: vectoridx.Dim512, ElementType: vectoridx.ElementFloat32}, "embedding", common.EntityImage)
```

## Lifecycle

```go
db, err := index.Create(dir, index.Spec{Schema: schema}, index.WithDurability(common.DurabilityOS))
db, err := index.Open(dir)
defer db.Close()
db.Sync(); db.Checkpoint(); db.Compact(); db.Verify()
```

`Create` fails if any namespace cannot be created; `Open` opens each namespace
independently and reports failures through `Health()` / `HealthOf(ns)` without
blocking healthy namespaces. A query against an unavailable namespace fails
explicitly through its accessor (`Time`, `Geo`, `Sim`, `Hash`, `Vector`,
`Bitmap`) returning `false`.

## Mutations

```go
db.Add(index.Entry{ValueID: 100, Times: ..., Bitmaps: ...})
db.Update(index.Change{Before: ..., After: ...})
db.Delete(common.EntityImage, 100)

batch := db.NewBatch(64)
batch.Add(entry); batch.Update(change); batch.Delete(common.EntityImage, 7)
db.Apply(batch)
batch.Reset()
```

Single operations and batches share the same internal pipeline.

- `Add` is an upsert of every supplied value, then marks each involved entity
  live. Multiple values per namespace are supported.
- `Update` reconciles `Before`/`After` by set difference. A namespace present in
  either side is reconciled (values dropped from `Before` and absent in `After`
  are removed; new `After` values are added); a namespace absent from both sides
  is untouched. This is the explicit-removal mechanism and keeps unrelated
  namespaces write-free.
- `Delete` clears liveness first (so the entity disappears from searches
  immediately) and then removes ID-authoritative records where the engine
  supports it (timestamp, vector). Engines without delete (simhash, radixhash)
  rely on the liveness filter; geoidx deletes by key during an update.

## Consistency contract

A `Store` is **not** an atomic multi-engine transaction. A batch is applied
engine by engine under the store write mutex. The complete batch is
prevalidated (routing, family, entity, ValueID, vector dimensions, simhash id
model) before any engine is mutated, so a malformed batch cannot partially
apply.

If an engine fails during application, every namespace the batch intended to
modify is marked **stale** (`NamespaceHealth.Stale`); those namespaces are
returned as unavailable by their accessors and rejected by search compilation
until the process is reopened (each engine is individually consistent after
reopen) or the namespace is rebuilt. Unrelated namespaces are untouched. Value
mutations publish before liveness on additions and after liveness on deletions,
so an incomplete entity is never searchable.

`ApplyWithGeneration` records a per-namespace completeness generation with
checksummed versioned persistence in `store.bin`. The store-wide
`SourceGeneration()` is the **minimum** across declared namespaces, so a partial
namespace change cannot overstate completeness. For timeidx namespaces the
generation is also persisted into the engine's own watermark and validated
against it on reopen; a timeidx engine that recovered an older watermark than
the manifest recorded marks that namespace stale (`ErrIndexStale`), never
corrupt. Empty generation-only batches claim completeness for every healthy
namespace.

## Search integration

```go
eng, err := search.New(db)                 // search package
during, _ := eng.During(1, start, end)
plan, err := eng.Compile(common.EntityImage, during)
ids, err := plan.AppendIDs(ctx, nil, ws)
```

`search.Engine.Compile` prepends the entity liveness restriction, so logically
deleted entities are excluded automatically. `Engine` also exposes typed
`Time`/`Geo`/`Bitmap`/`Sim`/`Hash`/`Vector` accessors. Compiled-query execution
resolves engines by namespace number from the immutable registry — no maps, no
reflection, no string lookups (a routing lookup is 0 B/op and sub-nanosecond).

## Performance

- Namespace routing is a dense `[]route` indexed by `common.Namespace`; lookup
  allocates nothing.
- No maps or reflection appear on the mutation or query hot paths.
- See `benchmark_test.go` (routing, single add, and 1/16/32/64-entity batches).

## Known limitations

- **Cross-engine atomicity.** There is no multi-engine snapshot or transaction.
  A mid-batch failure degrades every namespace the batch intended (see the
  consistency contract); there is no rollback of already-committed engines.
- **Hash updates need `Before`.** Invalidation of a stale `(hash, id)` uses the
  previous hash supplied in `Change.Before`; if the caller omits it, the stale
  association cannot be targeted and remains (liveness filtering is not used for
  an entity that stays live).
- **simhash IDs are dense.** simhash assigns 1-based record IDs, so a simhash
  value must use the next dense `ValueID`; other IDs return
  `ErrNamespaceIDModel`. Prevalidation enforces this for a whole batch before
  any engine is mutated.
- **Search clauses.** The `search` package compiles bitmap, timestamp, and
  geographic clauses. Exact-hash, perceptual-hash, and vector results are
  available through the typed accessors (`Engine.Hash`/`Sim`/`Vector`) and must
  be filtered by liveness by the caller until dedicated clauses are added.
  `Engine.Compile` rejects clause/entity mismatches and clauses bound to another
  Store.
- **Unified-layer allocations.** Operation staging allocates one engine batch
  per affected namespace; batch scratch reuse is not yet complete, so
  zero-allocation single mutations are not guaranteed.
- **geoidx legacy detection.** When a geoidx data file is missing, geoidx
  reports `ErrLegacyFormat` if the directory contains any file named `manifest`
  (for example the bitmapidx manifest). The namespace is unavailable either way.
- **Degradation lifetime.** A stale (degraded) namespace clears only on reopen;
  there is no in-process repair path.

## Validation

```sh
go build ./...
go test ./index/... ./search/...
go test -race ./index/... 
go test ./index/ -run '^$' -bench . -benchmem
sigmap validate
```
