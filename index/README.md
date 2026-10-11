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
checksummed versioned persistence in `store.bin`. Before the manifest records a
generation, every touched namespace is **checkpointed** (a verified durability
barrier): the engine data is made durable first, so a manifest claim of
completeness can never outrun recoverable engine data. A timeidx batch commits
its mutations and its watermark in one engine transaction.

### Interrupted mutations

A batch that could leave one commit unit durable and another not writes a
compact durable **recovery marker**. Commit units are the distinct user
namespaces (each engine's per-namespace `Apply` is atomic; vectoridx, which
applies one operation at a time, counts each vector operation). Liveness shares
the bitmap store and the entity's rebuild, so it is not a separate unit. A unit
is crash-durable during apply for `timeidx` (synchronized COW), `bitmapidx`
(WAL tail replayed on reopen) and `vectoridx` (WAL replayed on reopen); a
simhash *update* is journaled and durable while a new-record append needs the
manifest; radixhash and geoidx are checkpoint-dependent. Verified by
`TestEnginePersistenceBoundaries`. A batch is risky exactly when it spans two or
more units and at least one is crash-durable — a single unit, or a batch of
checkpoint-dependent units only, stays consistent without a marker. A
single-namespace mutation with internal liveness leaves only benign states (the
entity is not live, so it is not searchable, or it is live without a value), so
it does not need a marker (`TestSingleNamespaceLivenessFailureIsBenign`). The
marker (`recovery.bin`) lists the intended
(`recovery.bin`) listing the intended namespaces *before* the first engine can
persist, and clears it only after the touched engines are durable. If the
process is interrupted, `Open` reads the marker and marks those namespaces
rebuild-required and unavailable (`ErrRecoveryRequired`), preserving healthy
namespaces; `RecoveryNamespaces()` reports them and `ClearRecovery()` removes
the marker after a rebuild. A partially failed mutation therefore never
silently becomes healthy after reopening. The store-wide
`SourceGeneration()` is the **minimum** across declared namespaces, so a partial
namespace change cannot overstate completeness. For timeidx namespaces the
generation is also persisted into the engine's own watermark and validated
against it on reopen; an engine that recovered an older watermark than the
manifest recorded marks that namespace stale (`ErrIndexStale`), never corrupt.
Empty generation-only batches checkpoint every healthy namespace and claim
completeness for all of them.

## Search integration

General Search is focused on **TimeIdx, GeoIdx, and BitmapIdx** (VectorIdx
access is available through the typed accessor):

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

RadixHash and SimHash are **not** Search clauses. They are dedicated to image
import and are reached through the import matcher below.

## Import matching

`Store.Matcher` binds the duplicate/similarity engines for import:

```go
m, err := db.Matcher(index.MatcherSpec{SHA256: 4, BLAKE3: 5, Perceptual: 3})
hits, err := m.Match(dst, items, index.MatchParams{PHashDistance: 4, PDQDistance: 28, DetectIntraBatch: true})
```

- SHA-256 and BLAKE3 are separate `KindHashExact` namespaces; perceptual (pHash
  + PDQ) is a `KindHashPerceptual` namespace. All must carry `EntityImage`;
  binding anything else returns `ErrMatcherNamespace`.
- `Match` returns caller-owned `[]Match` with exact `ImageID`s and **native**
  Hamming distances (0 for exact hits); there is no score normalization or
  ranking fusion.
- Deleted images are filtered by the entity-liveness bitmap, and stale hash
  associations are already removed by `Update`, so neither can be returned.
- `MatchItem` carries optional SHA256/BLAKE3/pHash/PDQ inputs; the live-entity
  bitmap is cached through a **seqlock**: the writer marks a liveness mutation
  in progress (odd sequence) before it can become visible and completes it
  (even) after publishing, and a reader accepts only an even sequence whose
  value did not change across its snapshot build. This is stronger than a bare
  before/after version compare, which could pair a post-mutation bitmap with a
  pre-mutation version. Until a liveness removal is durably recorded the filter
  is skipped entirely; the flag is written to the manifest before the first
  removal commits, so reopening a store with no deletes keeps the fast path
  while a store with deletes filters conservatively.
- Matching validates every required namespace once per batch **before appending
  any result**, so a degraded or missing namespace returns an error with the
  caller's destination slice unchanged (never a partial batch).
- `MatchWithWorkspace` accepts caller-owned `MatchWorkspace{Exact, Similar}`
  scratch; with adequate capacity, duplicate-heavy exact and perceptual
  matching allocate nothing (versus a stack/`sync.Pool` fallback in `Match`).
- A namespace degraded after the matcher is built is rejected with
  `ErrNamespaceUnavailable` for the operations that need it (checked lazily, so
  a single-family call pays one lock-free check); unrelated healthy families
  keep working.
- Results are deterministic: index hits in ascending ID order, then intra-batch
  peers in ascending item order. `DetectIntraBatch` reports duplicates among the
  incoming items (`Match.Intra` with `Match.Peer`).
- Grouping decisions and match-and-commit atomicity stay outside the matcher; a
  match does not create StackIDs or AssetIDs.

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
  geographic clauses; exact-hash and perceptual-hash matching is intentionally
  served by the import matcher instead of Search. `Engine.Compile` rejects
  clause/entity mismatches and clauses bound to another Store. The typed
  `Engine.Hash`/`Engine.Sim`/`Engine.Vector` accessors remain for compatibility.
- **Matcher atomicity.** Import matching does not commit; it only reports live
  matches. There is no match-and-commit transaction.
- **Matcher degradation check cost.** Each batch pays one lock-free degradation
  check per required family before appending results, preserving the
  zero-allocation path; the direct engine path is unchanged.
- **First-delete manifest write.** The first liveness removal in a store writes
  the manifest (write-ahead flag) once, adding one directory sync to that
  delete; subsequent deletes and steady-state matching are unaffected.
- **Generation advancement cost.** `ApplyWithGeneration` is dominated by the
  durable unified-manifest write (one directory sync) plus, for risky
  multi-namespace batches, a per-engine checkpoint and recovery-marker write.
  Ordinary `Apply` does not pay this unless the batch is risky. Measured:
  ordinary Apply ≈ 7.7 ms vs `ApplyWithGeneration` ≈ 33 ms per 64-entity batch.
- **Recovery-marker conservatism.** Interrupting a risky batch marks its
  intended namespaces rebuild-required on reopen even if the batch had actually
  completed in memory; the application rebuilds or clears via `ClearRecovery`.
- **Non-time generation verification.** Non-time namespaces are trusted via the
  checkpoint-before-manifest ordering rather than an independent engine
  watermark (timeidx validates its own).
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
