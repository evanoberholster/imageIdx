# index — unified index Store

`index` is the advanced unified store that owns and coordinates the six imageIdx
index families in one root directory. Applications using the primary database
lifecycle should normally use [`database.DB`](../database/README.md); `index`
remains public for standalone use, import/matcher workflows, and advanced
services.

> **Experimental family:** `KindVector` and `index/vectoridx` are functional but
> experimental in 0.5. Vector APIs, performance, and on-disk formats are not part
> of the 0.5 compatibility promise.

| Namespace kind | Engine | Files |
|---|---|---|
| `KindTimestamp` | `timeidx` | `<hex>.timeidx` per namespace |
| `KindGeo` | `geoidx` | `geo-<hex>.idx` per namespace |
| `KindHashPerceptual` | `simhash` | `sim-<hex>.manifest`, `sim-<hex>-shard-*.bin`, `sim-<hex>.wal` |
| `KindHashExact` | `radixhash` | `radix-<hex>.idx` per namespace |
| `KindVector` | `vectoridx` (experimental) | `vector-<hex>.idx` (+ `.manifest`/`.wal`/`.delta`) |
| `KindBitmap*` | `bitmapidx` | `manifest`, `namespaces-*.idx`/`.wal` (one Store, many namespaces) |

The unified manifest is `store.bin` (magic `UIDX`, version 2). No per-family or
per-namespace subdirectories are created. `index.Create`/`Open` remain
standalone entry points and persist `schema.bin` for independent use.
Database-managed operation uses `CreateWithDefinitions`/`OpenWithDefinitions`,
which consume `schema.dat`'s definitions and never create `schema.bin`; an old
`schema.bin` is validated once and retired.

## Entity model

`schema.EntityKind` names ten entity kinds (image, face, person, object, album,
event, place, asset, collection, stack). A namespace declares exactly one entity
kind in `schema.NamespaceDef.Entity`; the entity is never stored per record, and
the same numeric `schema.ValueID` is independent across entity kinds.

Liveness is a single reserved `bitmapidx` enum namespace (`0xFFF0`) whose values
are the entity-kind names. Marking an entity live adds `(id, kindName)`; deleting
removes it. A search restricted to an entity kind cannot return a deleted
entity, and deleting `ImageID 100` never affects `FaceID 100`.

## Schema

Namespaces are registered in a `schema.Definitions` before `Create`/`Open` and
never at runtime. Standalone stores persist `schema.bin`; database-managed
stores receive definitions from `schema.dat`. `schema.Definitions` uses
version 2, which stores the entity kind; version-1 blobs load as `EntityInvalid`
and are rejected. `validateSchema` accepts only index kinds, so KV definitions
never enter an engine routing table.

Helpers encode engine configuration into `NamespaceDef.Config`:

```go
tags, _ := index.BitmapDef(bitmapidx.NamespaceSpec{Namespace: 6, Kind: schema.KindBitmapEnum}, "tags", schema.EntityImage)
vec, _  := index.VectorDef(vectoridx.Config{Namespace: 5, Model: "my-embedding-v1", Dimensions: vectoridx.Dim512, ElementType: vectoridx.ElementFloat32}, "embedding", schema.EntityImage)
```

## Lifecycle

```go
db, err := index.Create(dir, index.Spec{Schema: schema}, index.WithDurability(schema.DurabilityOS))
db, err := index.Open(dir)
defer db.Close()
db.Sync(); db.Checkpoint(); db.Compact(); db.Verify()
```

`Create` fails if any namespace cannot be created; `Open` opens each namespace
independently and reports failures through `Health()`/`HealthOf(ns)` without
blocking healthy ones. Accessors (`Time`, `Geo`, `Sim`, `Hash`, `Vector`,
`Bitmap`) return `false` for an unavailable namespace.

## Mutations

```go
db.Add(index.Entry{ValueID: 100, Times: ..., Bitmaps: ...})
db.Update(index.Change{Before: ..., After: ...})
db.Delete(schema.EntityImage, 100)

batch := db.NewBatch(64)
batch.Add(entry); batch.Update(change); batch.Delete(schema.EntityImage, 7)
db.Apply(batch)
batch.Reset()
```

Single operations and batches share one internal pipeline.

- `Add` upserts every supplied value, then marks each involved entity live.
- `Update` reconciles `Before`/`After` by set difference: values dropped from
  `Before` and absent from `After` are removed, new `After` values are added, and
  a namespace absent from both sides is untouched. This is the explicit-removal
  mechanism and keeps unrelated namespaces write-free. Geographic associations
  are `(namespace, key)` pairs; a move within a namespace replaces the key (the
  engine's `Insert` is ID-authoritative).
- `Delete` clears liveness first (so the entity leaves searches immediately) and
  then removes ID-authoritative records where the engine supports it (timestamp,
  vector). simhash and radixhash rely on the liveness filter.

## Bit-sliced numeric indexes (BSI)

A `KindBitmapBSI` namespace stores one unsigned integer per `ValueID` in
`bitmapidx`, addressed through reserved existence and value-bit postings. Two
widths are supported: `BSIWidth16` (`uint16`) and `BSIWidth32` (`uint32`); the
width is persisted in the namespace registry and validated on reopen. Values are
never truncated — a value wider than the namespace is rejected. Zero is a real
value, distinguished from absent by the existence bitmap.

```go
def, _ := index.BitmapDef(bitmapidx.NamespaceSpec{
    Namespace: 30, Kind: schema.KindBitmapBSI, BSIWidth: bitmapidx.BSIWidth16,
}, "quality", schema.EntityImage)
bsi, _ := db.Bitmap(30)
bsi.Set(100, 85); bsi.Delete(200)
value, ok, _ := bsi.Get(100)
equal, _ := bsi.Equal(85)             // roar.Bitmap of schema.ValueID
inRange, _ := bsi.BSIRange(50, 90)
candidates, _ := bsi.BSIFilterRange(nil, ids, bitmapidx.ValuesBetween(50, 90))
```

Both single writes and `Batch.BSIMove` (with explicit `HasFrom`/`HasTo`
preconditions) are supported; stale preconditions and duplicate `ValueID`s fail
the whole batch atomically. Queries use bit-sliced comparisons (MSB first, with
early termination), `BSIFilterRange` reconstructs small candidate sets and
materializes a range bitmap otherwise, and results are ordinary `roar.Bitmap`s
of `schema.ValueID` that compose with the rest of the unified search surface.
WAL records are logical value transitions, not per-bit changes, and reuse the
shared checkpoint, compaction, mmap-reopen, and durability machinery.

## Consistency and recovery

For authoritative Pebble records coordinated with this derived store, see
[`database/README.md`](../database/README.md). The database layer owns an
ordered journal and only acknowledges a batch after the unified store's
recoverable source generation confirms it. An interrupted batch without a
complete durable watermark is reported as recovery-required in manual mode.
Automatic database startup recovery may reconstruct a full replacement before
publishing any index pointer; it does not hot-swap live namespaces.

A `Store` is **not** an atomic multi-engine write transaction. A batch is prevalidated
(routing, family, entity, ValueID, vector dimensions, simhash id model) before
any engine is mutated, so a malformed batch cannot partially apply. If an engine
fails during application, every namespace the batch intended is marked **stale**
(`NamespaceHealth.Stale`), rejected by accessors and search compilation until
reopen or rebuild; unrelated namespaces stay usable. Value mutations publish
before liveness on additions and after it on deletions, so an incomplete entity
is never searchable.

Queries through `search.Engine` capture one reader-pinned snapshot for all
participating engines, so one plan execution sees a coherent published
generation while writers continue. Direct engine access and direct
`search.Compile` plans read live views.

`ApplyWithGeneration` records a per-namespace completeness generation in
`store.bin`. Before the manifest records a generation, every touched namespace is
**checkpointed** (a verified durability barrier) so a completeness claim cannot
outrun recoverable engine data. A timeidx batch commits its mutations and its
watermark in one engine transaction. `SourceGeneration()` is the minimum across
declared namespaces; timeidx also persists and validates its own watermark on
reopen (`ErrIndexStale`, never corrupt).

`ApplyWithGenerationAll` is the database-coordinator variant: after securing
touched writes, it advances healthy **untouched** namespaces to the same
source generation in that manifest publication. It does not rewrite unchanged
values. A failed/unavailable namespace still holds the global minimum back.

### Interrupted mutations

A batch that could leave one commit unit durable and another not writes a compact
durable **recovery marker** (`recovery.bin`) listing the intended namespaces
before the first engine can persist, and clears it once the touched engines are
durable. Commit units are the distinct user namespaces (vectoridx, which applies
one operation at a time, counts each operation); liveness shares the bitmap store
and the entity rebuild, so it is not a separate unit. A unit is crash-durable
during apply for `timeidx`, `bitmapidx`, and `vectoridx`; simhash updates are
journaled while new-record appends need the manifest; radixhash and geoidx are
checkpoint-dependent (see `TestEnginePersistenceBoundaries`). A batch is risky
exactly when it spans two or more units with at least one crash-durable unit.

If the process is interrupted, `Open` reads the marker and marks those namespaces
rebuild-required/unavailable (`ErrRecoveryRequired`), preserving healthy
namespaces; `RecoveryNamespaces()` reports them and `ClearRecovery()` removes the
marker after a rebuild. A partially failed mutation therefore never silently
becomes healthy after reopening.

## Search integration

General Search is focused on **TimeIdx, GeoIdx, and BitmapIdx** (VectorIdx is
reachable through the typed accessor):

```go
eng, err := search.New(db)
during, _ := eng.During(1, start, end)
plan, err := eng.Compile(schema.EntityImage, during)
ids, err := plan.AppendIDs(ctx, nil, ws)
```

`search.Engine.Compile` prepends the entity liveness restriction, so deleted
entities are excluded automatically, and resolves engines by namespace number
from the immutable registry — no maps, no reflection, no string lookups.

Exact-content `radixhash` checks are for import-time duplicate/presence
detection, not Search candidate collection or plan stages. Use the unified
import matcher to find existing live images for an incoming digest; it filters
deleted images through entity liveness. The algorithm-to-namespace association
is defined by `MatcherSpec` (the radix index itself stores digest bytes only).
Perceptual `simhash` matching uses the same matcher path.
Vector ANN results can be filtered through `search.Plan.FilterRanked`.

## Import matching

```go
m, err := db.Matcher(index.MatcherSpec{SHA256: 4, BLAKE3: 5, Perceptual: 3})
hits, err := m.Match(dst, items, index.MatchParams{PHashDistance: 4, PDQDistance: 28, DetectIntraBatch: true})
```

- SHA-256 and BLAKE3 are separate `KindHashExact` namespaces; perceptual
  (pHash + PDQ) is a `KindHashPerceptual` namespace. All must carry
  `EntityImage`; requesting an unconfigured family or binding another kind/entity
  returns `ErrMatcherNamespace` (a closed Store returns `ErrClosed`).
- `Match` returns caller-owned `[]Match` with exact `ImageID`s and **native**
  Hamming distances (0 for exact hits); no score normalization or ranking fusion.
  Deleted images and stale hash associations cannot be returned.
- The live-entity bitmap is cached through a **seqlock** (write marks the
  mutation in progress before it can become visible and completes it after
  publishing; a reader accepts only an unchanged even sequence). Until a liveness
  removal is durably recorded the filter is skipped entirely.
- Every required namespace is validated once per batch **before appending any
  result**, so a failure leaves the caller's destination unchanged. A namespace
  degraded after construction is rejected with `ErrNamespaceUnavailable` for the
  operations that need it.
- `MatchWithWorkspace` accepts caller-owned `MatchWorkspace{Exact, Similar}`
  scratch; with adequate capacity, duplicate-heavy exact and perceptual matching
  allocate nothing.
- Results are deterministic (index hits ascending by ID, then intra-batch peers
  by item index). `DetectIntraBatch` reports duplicates via `Match.Intra`/`Peer`.
  The matcher does not create StackIDs/AssetIDs or group images.

## Performance

Namespace routing is a dense `[]route` indexed by `schema.Namespace` and
allocates nothing; no maps or reflection appear on the mutation or query hot
paths. Benchmarks live in `benchmark_test.go`, `matcher_benchmark_test.go`, and
`geo_test.go`.

## Known limitations

- **Cross-engine writes are not atomic.** A mid-batch failure degrades the
  intended namespaces with no rollback. Queries through `search.Engine` still
  capture one coherent reader-pinned snapshot; direct engine access is live.
- **Hash updates need `Before`.** Invalidation of a stale `(hash, id)` uses the
  previous hash in `Change.Before`.
- **SimHash has a dense fast path and a sparse overlay.** Contiguous append IDs
  use the dense pages; arbitrary nonzero external IDs use `ApplyUpdates` and the
  durable sparse segments. Dense append APIs are rejected after the sparse
  overlay is used to preserve one source of truth.
- **Search clauses.** Exact and perceptual hashes are import/matching APIs, not
  general plan predicates. `Engine.Compile` rejects clause/entity mismatches and
  foreign-Store clauses; vector ANN stays outside the plan and is experimental.
- **Generation advancement cost.** `ApplyWithGeneration` is dominated by the
  unified-manifest directory sync (~13 ms vs ~7.8 ms ordinary Apply per 64-entity
  batch); ordinary `Apply` pays it only for risky batches.
- **Recovery-marker conservatism.** Interrupting a risky batch marks its intended
  namespaces rebuild-required even if it had completed in memory.
- **Non-time generation verification.** Non-time namespaces are trusted via the
  checkpoint-before-manifest ordering (timeidx validates its own watermark).
- **Degradation lifetime.** A stale namespace clears only on reopen.
- **Unified-layer allocations.** Staging allocates one engine batch per affected
  namespace, so zero-allocation single mutations are not guaranteed.
- **geoidx legacy detection.** With a missing geoidx data file, geoidx reports
  `ErrLegacyFormat` when the directory contains any `manifest` file; the
  namespace is unavailable either way.
- **BSI range evaluation is not fused.** Each bit-sliced comparison allocates
  intermediate bitmaps (MSB first, with early termination). It is already much
  faster than the continuous index for wide ranges, but a fused kernel could
  cut allocations further; this is deferred until profiling justifies it.

## Validation

```sh
go build ./...
go test ./index/... ./search/...
go test -race ./index/...
go test ./index/ -run '^$' -bench . -benchmem
sigmap validate
```
