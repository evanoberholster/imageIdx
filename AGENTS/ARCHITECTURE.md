# Current architecture

This document describes the library as implemented. It is not a design
proposal. Public API and persistence contracts are summarized in
[`CONTRACTS.md`](CONTRACTS.md); package docs remain authoritative.

## Dependency direction

```text
schema
  ^
  |
store ------> database ------> index ------> search
  ^             ^                ^
  |             |                |
  +-------------+----------------+
       internal/storage, pagealloc
```

Rules:

- `index` packages may import `schema`, `roar`, and `internal/*`.
- index packages must not import `search`.
- `database` owns `schema.Schema`, `store.Store`, and `index.Store`.
- `database` does not import `search`; applications bind search with
  `search.New(db.Index())`.
- `store` is a generic KV primitive. It does not import `index`, `search`, or
  `database`.

## schema

`schema` owns the database's logical model:

- `EntityKind` and entity-scoped identifier aliases (`ImageID`, `UserID`, etc.);
- `Namespace`, `NamespaceDef`, and `Definitions`;
- durable `schema.dat` container and allocation slots;
- entity ValueID allocation: `NextID`, `NextIDs`, `EnsureMinimum`;
- PREPARED/ACTIVE publication for additive namespace changes;
- projection versions and the persisted database config bytes.

The active definitions are immutable after `schema.Open`. Namespace additions
are startup-only and coordinated by `database`. `schema.dat` is the authority;
the legacy Pebble schema key and old `index/schema.bin` are migration inputs
only.

See [`schema/README.md`](../schema/README.md) and
[`schema/FORMAT.md`](../schema/FORMAT.md).

## store

`store` is one Pebble database with a set of registered `KindKV` namespaces.
Every key is 12 bytes:

```text
namespace:uint16 BE | valueID:uint64 BE | flags:uint16 BE
```

The value is opaque application bytes. `Batch` is atomic across registered
namespaces and uses Pebble's synced commit path. `Get` returns owned bytes;
`Scan` values are borrowed for the callback duration. Database-owned keys use
`flags = 0`; namespace zero is reserved for the legacy schema key.

`database` uses `store` for canonical records, the ordered journal, and
generation metadata. Do not open a second store on a live database directory.
See [`store/README.md`](../store/README.md) and
[`database/FORMAT.md`](../database/FORMAT.md).

## database

`database.DB` is the primary application lifecycle.

### Canonical commit

`DB.Commit`:

1. validates mutations and the projection callback;
2. reads prior canonical values;
3. writes canonical values, journal entries, and the next committed generation
   in one synced Pebble batch;
4. returns the last committed generation.

A failed batch consumes no sequence. Inputs are caller-owned and may be reused
after return. Duplicate `(Entity, ValueID)` mutations in one call are rejected.

### Projection modes

- Inline mode stores projection bytes beside the application record.
- External mode stores only the application record and requires a deterministic
  `ProjectFromRecord`/`ProjectInto` callback.
- The mode is persisted in `schema.dat`; a conflicting reopen fails.
- Projection-version migrations use explicit `ProjectionMigration` + external
  projector and rebuild index resources.

### Coordinator

One serialized background coordinator:

1. scans journal records in sequence order;
2. decodes before/after projections;
3. diffs typed association families;
4. coalesces only proven-safe repeated entity events;
5. applies an `index.Batch`;
6. verifies the source generation;
7. durably acknowledges the generation in Pebble;
8. cleans up acknowledged journal records.

The coordinator never acknowledges a generation before the derived index is
durable. `WaitIndexed` broadcasts completions and propagates permanent
coordinator failures. `Sync` waits for all generations committed before the
call.

### Liveness and deletes

Each entity kind has a reserved liveness namespace. Deletions clear liveness
before removing ID-authoritative values; additions publish values before
liveness. This prevents an incomplete entity from being searchable.

A delete writes a canonical tombstone and a journal delete. Reinsertion over a
tombstone is a distinct journal transition: replay restores liveness and uses
the old projection to remove associations absent from the new projection.

### Recovery

`RecoveryManual` fails closed with `ErrRecoveryRequired` for an unhealthy index.
`RecoveryAutomatic` performs a synchronous startup rebuild from canonical
projections and verifies the replacement before publishing it. The recovery
unit is the complete unified index because bitmap namespaces share a physical
store and time namespaces share a physical store. Selective family recovery is
deferred.

See [`database/README.md`](../database/README.md) and
[`database/FORMAT.md`](../database/FORMAT.md).

## index

`index.Store` is the advanced unified index front door. It owns the engine
instances for one root directory, routes namespace values, coordinates liveness,
and writes `store.bin` (unified manifest, version 2).

Writes are serialized. A batch is prevalidated for routing, entity, ID, vector
dimension, and engine ID-model constraints before mutation. Partial engine
failure marks the intended namespaces stale/degraded; unrelated namespaces remain
usable. A risky multi-engine batch writes a durable recovery marker before the
first engine can persist.

Engine families:

| Kind | Engine | Structure |
|---|---|---|
| `KindBitmap*` | `bitmapidx` | One shared bitmap store/WAL across bitmap namespaces |
| `KindTimestamp` | `timeidx` | One COW mmap file per namespace |
| `KindGeo` | `geoidx` | One S2 index file per namespace |
| `KindHashExact` | `radixhash` | One radix tree file per namespace |
| `KindHashPerceptual` | `simhash` | Dense hash pages + sparse segmented overlay |
| `KindVector` | `vectoridx` | Experimental HNSW file/delta/WAL layout |

### Bitmap indexes

`bitmapidx` publishes immutable namespace views containing base postings,
frozen/active deltas, and dictionary state. Readers load one atomic view under a
reader token; writes serialize per engine/store. BSI namespaces use bit-sliced
value postings. The bitmap store is the physical unit shared by all bitmap
namespaces and liveness.

### Time indexes

`timeidx` stores one exact timestamp per `ValueID` per namespace as exact
unsigned nanoseconds since 1900-01-01 UTC. Each namespace is one append-only COW
file with immutable sorted shards and atomic snapshot publication. Persistent
commits synchronously seal a complete generation. Search order is
`(timestamp, ValueID)`.

### Geographic indexes

`geoidx` stores fixed-level S2 keys, never coordinates, with one location per
ID. The base is a sharded pair of sorted `(key, ID)` columns. Updates are
ID-authoritative. Queries use binary search plus contiguous range scans; there
is no reverse ID-to-key index. Compaction and growth preserve immutable views
and retire mappings only after readers drain.

### Exact hashes

`radixhash` stores 256-bit hashes in a COW radix tree with page-allocator
arenas. Duplicate hashes are allowed. Lookups are lock-free; writes serialize
and publish new views. Exact and perceptual hashes are import/matcher APIs, not
general `search.Plan` predicates.

### Perceptual hashes

`simhash` stores pHash64, PDQ256, and PDQ quality. Dense IDs use append-only
hash pages with 65,536 slots and per-block seqlocks for in-place point updates.
Arbitrary nonzero external IDs use immutable CRC-checked sparse segments and an
atomically replaced `sparse.manifest`; sparse records override dense entries and
preserve ascending `ImageID` order. Once sparse storage is used, dense append
APIs are rejected.

### Vector indexes — experimental

`index/vectoridx` implements persistent HNSW with one serialized writer,
atomic snapshots, WAL/delta generations, tombstones, candidate filtering, and
exact-scan fallback. Vector APIs, performance, and file formats are **not**
0.5 contracts.

## search

`search` compiles conjunctive clauses over bitmap, timestamp, geo, and ACL
sources. It does not collect candidates from exact-hash or vector indexes.

### Plans

`search.Compile` canonicalizes clauses and orders stages by a bounded cost model.
`search.Engine.Compile` binds a plan to an `index.Store`, adds the entity
liveness clause, validates namespace ownership, and attaches the snapshot guard.

Plan execution operations include:

- `AppendIDs`/`Search`: sorted unique IDs;
- `AppendPage`: ID-cursor pagination;
- `FilterIDs`: preserve candidate order/duplicates;
- `Count`/`Exists`: avoid full result materialization where possible;
- `Visit`: streaming callback execution;
- `RankTime`/`RankGeo`: native top-k;
- `FilterRanked`: probe externally ranked candidates.

### Snapshots and workspaces

Every `Engine` plan execution waits for an even publication sequence, captures
one reader-pinned snapshot per participating engine, revalidates the sequence,
and retries if a writer overlapped. Snapshots are held through `Visit`
callbacks. Writers continue publishing newer generations; reclamation may wait
for an old pinned mapping to drain.

Direct `search.Compile` plans are lower-level live-view plans and must keep
their indexes open. A `Workspace` is exclusive to one execution.
`search.Executor` pools workspaces for concurrent callers without changing plan
or result ownership. Successive pages or executions may observe later writes.

See [`search/README.md`](../search/README.md).

## Persistence and format ownership

| Artifact | Authority |
|---|---|
| `schema.dat` | `schema`; 0.5 container v3 |
| Canonical values / journal keys | `database`; 0.5 canonical v1/v2, journal v2 |
| Journal and canonical semantics | `database/FORMAT.md` |
| `store.bin` | `index`; derived version 2 |
| Engine files | Each engine package; rebuildable caches |
| Roaring serialization | `roar/FORMAT.md` |

Derived index files are not permanent 0.5 binary contracts. They must carry
version checks and a safe rebuild route. `schema.dat` and canonical semantic
changes require versioning and explicit migration or fail-closed rejection.

## Testing and benchmarks

- Unit/integration tests live beside each package.
- Process-crash tests use subprocess boundaries for schema, journal, index, and
  recovery transitions.
- `database` tests cover commit/replay, projection modes, migration, recovery,
  coalescing, sparse SimHash, and concurrency.
- `search` tests cover plan equivalence, stage filtering, snapshots, ranking,
  metrics, and service load.
- `internal/bench` owns deterministic datasets and latency recording. Package
  benchmarks cover hot paths. Opt-in 10M runs use `IMAGEIDX_BENCH_LARGE`.
- CI runs Linux amd64/arm64 build, full test, race, schema recovery, migration
  crash, and allocator jobs, including the internal SIMD-kernel equivalence
  suites.

## Current limitations

- One serialized index apply writer; no per-family parallel publication.
- Full-index recovery only; no selective namespace/family rebuild.
- Engine reclamation can delay writers while an old snapshot drains.
- Vector functionality is experimental.
