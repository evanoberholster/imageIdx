# imageIdx

Persistent, namespace-aware image indexing and search in Go. The primary public
API is `index.Store` (all index families in one schema-defined root directory)
plus `search.Engine` (entity-scoped, reusable conjunctive queries). The six
engine packages also expose standalone APIs; `store` is a separate Pebble-backed
key-value collection, **not** a namespace routed by `index.Store`.

## Packages at a glance

| Import suffix | Role | Unified namespace kind |
|---|---|---|
| `schema` | entity/canonical namespace relationships, authoritative namespace definitions, schema versions, persistent ValueID allocation, durability | — |
| `index` | unified persistent store, typed routing, mutations, liveness, recovery, import matcher | owns the six families below |
| `database` | lifecycle coordination, canonical commits, mutation journal, reconciliation and generation waits | owns a separate canonical `store.Store`, `schema.Schema`, and `index.Store` |
| `index/bitmapidx` | enum, bool, analyzed text, signed continuous grid, unsigned 16/32-bit bit-sliced index (BSI) | `KindBitmapEnum`, `KindBitmapBool`, `KindBitmapString`, `KindBitmapContinuous`, `KindBitmapBSI` |
| `index/timeidx` | timestamp range index | `KindTimestamp` |
| `index/geoidx` | level-19 S2 geographic index | `KindGeo` |
| `index/radixhash` | exact 256-bit content-hash lookup | `KindHashExact` |
| `index/simhash` | pHash64/PDQ256 perceptual matching | `KindHashPerceptual` |
| `index/vectoridx` | HNSW vector search and exact reference search | `KindVector` |
| `search`, `search/ranking` | AND query planning/execution, ranking and fusion | — |
| `roar` | immutable, serialized bitmap sets | — |
| `store` | Pebble lifecycle, persisted canonical values and KV batches | `KindKV`; database validates compatibility metadata against the central schema |

All imports start with `github.com/evanoberholster/imageIdx/`. A
`schema.Namespace` is a nonzero `uint16`; its number and name must be unique
within a `schema.Definitions`. Unified namespaces additionally have exactly one
`schema.EntityKind` (`EntityImage`, `EntityFace`, `EntityPerson`, `EntityObject`,
`EntityAlbum`, `EntityEvent`, `EntityPlace`, `EntityAsset`, `EntityCollection`,
or `EntityStack`). `schema.ValueID` and `schema.ImageID` alias `uint64`; zero
is reserved. Numeric IDs can overlap across entity kinds without mixing their
liveness. `schema.UserID` and `schema.GroupID` are separate defined types.

## Quick start: unified store and search

When metadata must survive and drive eventual index updates, use the
[`database` integration layer](database/README.md) instead of directly
mutating `index.Store`. It commits authoritative Pebble records and a mutation
journal atomically, then indexes them asynchronously. `WaitIndexed` establishes
the requested search-completeness generation. `database.Options{Recovery:
database.RecoveryAutomatic}` enables synchronous startup-only rebuilding of a
missing or incomplete index from canonical records; manual recovery remains
the default. See the documented rebuild and mmap-reader limitations.

The snippets below use these imports:

```go
import (
    "context"

    "github.com/evanoberholster/imageIdx/schema"
    "github.com/evanoberholster/imageIdx/index"
    "github.com/evanoberholster/imageIdx/index/bitmapidx"
    "github.com/evanoberholster/imageIdx/index/geoidx"
    "github.com/evanoberholster/imageIdx/index/timeidx"
    "github.com/evanoberholster/imageIdx/search"
)
```

The following fragments go inside a function returning `error`; `Create` is
for a new directory (use `Open` on later runs).

Declare **all** namespaces before creating the store. `index.BitmapDef` and
`index.VectorDef` encode validated family-specific configuration into the
persisted definition; the time, geo and hash families use `NamespaceDef`
directly. Check every returned error in application code.

```go
defs := schema.NewDefinitions()
tags, err := index.BitmapDef(bitmapidx.NamespaceSpec{
    Namespace: schema.NamespaceTag, Kind: schema.KindBitmapEnum,
}, "tags", schema.EntityImage)
if err != nil { return err }
quality, err := index.BitmapDef(bitmapidx.NamespaceSpec{
    Namespace: 40, Kind: schema.KindBitmapBSI, BSIWidth: bitmapidx.BSIWidth16,
}, "quality", schema.EntityImage)
if err != nil { return err }
err = defs.RegisterAll(tags, quality,
    schema.NamespaceDef{Namespace: 90, Name: "taken-at", Kind: schema.KindTimestamp, Entity: schema.EntityImage},
    schema.NamespaceDef{Namespace: 91, Name: "location", Kind: schema.KindGeo, Entity: schema.EntityImage},
    schema.NamespaceDef{Namespace: 92, Name: "sha256", Kind: schema.KindHashExact, Entity: schema.EntityImage},
)
if err != nil { return err }

db, err := index.Create("./data/index", index.Spec{Schema: defs},
    index.WithDurability(schema.DurabilityOS))
if err != nil { return err }
defer db.Close()

key, err := geoidx.KeyFromLatLng(37.7749, -122.4194)
if err != nil { return err }
takenAt := uint64(1) << timeidx.TimestampShift
err = db.Add(index.Entry{ValueID: 100,
    Times:   []index.TimeValue{{Namespace: 90, Timestamp: takenAt}},
    Geos:    []index.GeoValue{{Namespace: 91, Key: key}},
    Bitmaps: []index.BitmapValue{
        {Namespace: schema.NamespaceTag, Value: bitmapidx.EnumStringValue("favorite")},
        {Namespace: 40, Value: bitmapidx.Uint16Value(85)},
    },
})
if err != nil { return err }

engine, err := search.New(db)
if err != nil { return err }
favorite, err := engine.Values(schema.NamespaceTag, bitmapidx.EnumStringValue("favorite"))
if err != nil { return err }
score, err := engine.BSIRange(40, bitmapidx.ValuesBetween(50, 90)) // inclusive
if err != nil { return err }
plan, err := engine.Compile(schema.EntityImage, favorite, score)
if err != nil { return err }
ws := search.NewWorkspace(256) // caller-owned; not shared concurrently
ids, err := plan.AppendIDs(context.Background(), nil, ws) // [100]
if err != nil { return err }
_ = ids
```

For a complete runnable example with time and geo predicates, see
`cmd/imageidx/examples/basic`; `cmd/imageidx` provides one-shot JSON and an
HTTP server, `examples/service` demonstrates `search.Executor`, and
`examples/bitmaponly` covers BSI and pagination.

### Schema and store configuration reference

| API | Purpose / options |
|---|---|
| `schema.NewDefinitions`, `Register`, `RegisterAll`, `Lookup`, `LookupName`, `Defs`, `Len` | Define unique namespace numbers/names before `index.Create`. `NamespaceDef` contains `Namespace`, `Name`, `Kind`, `Entity`, and optional persisted `Config`. |
| `index.BitmapDef(spec, name, entity)` | Bitmap enum/bool/text/continuous/BSI; `NamespaceSpec` selects `Kind`, optional `Normalizer` (enum), `Analyzer` (text), `Quant` (continuous), or `BSIWidth` (BSI, 16 or 32). Geography uses `KindGeo`, **not** the retired `KindBitmapGeo`. |
| `index.VectorDef(cfg, name, entity)` | Persists `Namespace`, `Dimensions`, `ElementType`, `Metric` from `vectoridx.Config`. `Dimensions` and `ElementType` must be valid. |
| `index.Create(dir, index.Spec{Schema: defs}, opts...)` | Creates one directory and all declared indexes; schema cannot be extended at runtime. |
| `index.Open(dir, opts...)` | Opens an existing store and its persisted schema. Inspect `Health()`; an unavailable namespace need not prevent other namespaces from opening. Keep the store open for any plans or matcher bound to it. |
| `index.WithDurability(d)` | `schema.DurabilityOS` (default) or `schema.DurabilitySync`; timeidx commits synchronously independently of this setting. |
| `index.WithRadixArenaBytes(n)` | Runtime capacity for each exact-hash namespace; default 64 MiB. |
| `index.Option` / `index.Options.Vector` | An `Option` can configure vector runtime policy, e.g. `func(o *index.Options) { o.Vector.EfSearch = 128 }`. The persisted dimensions/type/metric come from `VectorDef`, not runtime overrides. |

The reserved liveness bitmap namespace is internal; do not register it yourself.
`Store.Schema()` and `Store.NamespaceDef(ns)` return defensive copies;
`Store.EntityOf(ns)` reports ownership. `Bitmap`, `Time`, `Geo`, `Hash`, `Sim`,
and `Vector` return typed engine pointers and `ok=false` when a namespace is
missing or unavailable. `BitmapStore()` exposes the owning bitmap store for
ACL compilation; `Liveness(entity)` exposes the internal live predicate.

### Write API and lifecycle

`index.Entry` groups associations for **one** nonzero `ValueID`:

| Field | Slice element and key fields | Notes |
|---|---|---|
| `Times` | `index.TimeValue{Namespace, Timestamp uint64}` | Time is unsigned nanoseconds since 1900-01-01 UTC (`timeidx` encoding). |
| `Geos` | `index.GeoValue{Namespace, Key geoidx.S2Key}` | Obtain a fixed-level key with `geoidx.KeyFromLatLng`. An update to another key in the same namespace replaces the location. |
| `Sims` | `index.SimValue{Namespace, Record simhash.Record}` | Perceptual IDs must be dense and sequential starting at 1; incompatible IDs return `ErrNamespaceIDModel`. |
| `Hashes` | `index.HashValue{Namespace, Hash radixhash.Hash}` | Raw 32-byte digest; keep SHA-256/BLAKE3 in separate namespaces and hash outside the index. |
| `Vectors` | `index.VectorValue{Namespace, Element, Float32/Int8/Uint8/Int16/Uint16}` | Set only the slice corresponding to `Element`; `ValueID` must fit the vector `uint32` node range. |
| `Bitmaps` | `index.BitmapValue{Namespace, Value bitmapidx.Value}` | Values are type-checked against the namespace; see the bitmap section below. |

`db.Add(entry)` upserts associations and marks the relevant entity live.
`db.Update(index.Change{Before: old, After: next})` requires the same
`ValueID`; **only** namespaces present in either side are reconciled. Supply
`Before` for removals, especially stale exact-hash associations; absent from
both means unchanged. `db.Delete(entity, id)` clears entity liveness first,
then removes ID-authoritative engine records where supported. Old simhash and
radixhash records may remain physically present; use an entity-scoped plan or
the matcher to exclude deleted IDs.

```go
batch := db.NewBatch(64) // private, reusable; not safe for concurrent staging
if err := batch.Add(entry); err != nil { return err }
if err := batch.Update(index.Change{Before: old, After: next}); err != nil { return err }
if err := batch.Delete(schema.EntityImage, 7); err != nil { return err }
if err := db.Apply(batch); err != nil { return err }
if err := batch.Reset(); err != nil { return err }
```

`Batch.Len()` counts staged operations. `ApplyWithGeneration(batch, generation)`
also records canonical-source completeness after a durability barrier;
`SourceGeneration()` returns the minimum confirmed generation across declared
namespaces and `CheckSourceGeneration(required)` returns `ErrIndexStale` if
behind. Ordinary `Apply` does not advance this watermark.

`Store.Sync`, `Checkpoint`, `Compact`, `Verify`, and `Close` serve different
purposes: Sync flushes engines; Checkpoint writes their durable snapshots;
Compact reduces supported engine deltas; Verify validates exposed files;
Close drains owned engines. Check return values. `Dir`, `Generation`, `Health`,
`HealthOf`, `DegradedNamespaces`, and `RecoveryNamespaces` aid diagnostics.
After rebuilding affected namespaces from canonical data, `ClearRecovery`
clears an interrupted-batch recovery marker—do not use it to declare damaged
indexes healthy without rebuilding them.

**Consistency:** the unified store serializes writes, but a multi-engine
batch is not a database-wide atomic transaction and searches are not a
cross-engine snapshot. Mutations are prevalidated; on a partial failure the
intended namespaces become stale/unavailable. Additions publish values before
liveness and deletions remove liveness first. A crash during a risky batch can
leave a recovery marker that requires rebuilding the affected namespaces.
See [`index/README.md`](index/README.md) for recovery and persistence details.

## Search API

`search.New(db)` binds a unified store. `engine.Compile(entity, clauses...)`
checks namespace ownership, rejects foreign or mismatched indexes, and adds the
entity's liveness predicate automatically. The plan keeps index pointers, so
keep the store open. All clauses are conjunctive (AND); only ACL principals are
unioned internally. Standalone `search.Compile(clauses...)` omits liveness;
`search.CompileWithStore(bitmapStore, clauses...)` additionally resolves ACL
postings; `search.SearchOnce(ctx, dst, clauses...)` is the single-use shortcut.

### Clause constructors

| Unified `Engine` helper | Standalone clause | Meaning |
|---|---|---|
| `Values(ns, values...)` | `AllValues(idx, values...)` | Every typed bitmap value must be present; empty lists are invalid. |
| Obtain `engine.Bitmap(ns)` then call constructor | `NamespaceText(idx, text)` | Require all analyzed tokens (string namespace). |
| Obtain `engine.Bitmap(ns)` then call constructor | `NamespaceRange(idx, bitmapidx.Between(lo, hi))` | Signed continuous-grid range, `[lo,hi)`; `AtLeast`, `AtMost`, and explicit `NumericRange` select open/inclusive bounds. |
| `BSIRange(ns, bitmapidx.ValuesBetween(lo, hi))` | `NamespaceBSIRange(idx, range)` | Unsigned BSI range, **inclusive** for `ValuesBetween`; `ValuesAtLeast`, `ValuesAtMost` or explicit `ValueRange` support open/exclusive bounds. Width mismatches error at compile. |
| `During(ns, start, end, search.ExactTimestamp(lookup))` | `During(idx, start, end, options...)` | `[start,end)` timestamps; without an exact lookup coarse buckets can admit false positives. Empty ranges match nothing. |
| `InCells(ns, cellIDs...)` | `InCells(idx, cellIDs...)` | Union S2 cells (level 19 or coarser; validated at compile). |
| — | `WithACL(userID, authenticatedGroupIDs...)` | One user OR authenticated groups, intersected with other clauses. `Engine.Compile` accepts ACL for `EntityImage` only. Configure `NamespaceUserACL`/`NamespaceGroupACL` as enum namespaces and authenticate group membership externally. |

Exact-content hash checks use `index.Matcher` during import to find existing
live images for a digest; they are not Search clauses or candidate sources.
Perceptual matching uses the same matcher path. Vector ANN is a separate ranked
source, not a plan clause.

### Plan execution and buffers

| Operation | Result / behavior |
|---|---|
| `AppendIDs(ctx, dst, ws)` / `Search(...)` | Append sorted, unique IDs; preserve any existing destination prefix. `SearchWithMetrics(..., *SearchMetrics)` instruments the same operation. |
| `AppendPage(ctx, dst, ws, search.Page{AfterID, Limit})` | At most positive `Limit` IDs strictly after `AfterID`; cursor is by ID, not score. Updates between pages are visible. |
| `FilterIDs(ctx, dst, candidates, ws)` | Keep input order and duplicates; `dst=candidates[:0]` is supported. `FilterIDsWithMetrics` reports stages. |
| `Count(ctx, ws)`, `Exists(ctx, ws)` | Verified count or early existence; bitmap cardinality paths avoid an ID result slice where possible. |
| `Visit(ctx, ws, func(id) bool)` | Stream sorted matches; return `false` to stop. Do not re-enter with the same workspace. |
| `RankTime(ctx, dst, ws, lookup, target, k)` | Top-k by exact unsigned timestamp gap; `Result{ImageID, Score uint64}`. |
| `RankGeo(ctx, dst, ws, lat, lng, lookup, k)` | Top-k by canonical-coordinate haversine meters; `GeoResult{ImageID, Score float64}`. |
| `FilterRanked(ctx, dst, ws, ranked, imageID, score)` | Filter supplied ranked entries (e.g. vector ANN) while retaining order and score; over-fetch when selective filters might underfill. |
| `Refresh(ctx)`, `Stages()`, `Estimate()`, `Len()`, `IsEmpty()` | Refresh ordering hints without recompiling predicates; inspect stage capabilities/estimates. `IsEmpty` is structural only. |

`RankTimeWithMetrics` and `RankGeoWithMetrics` add rank-phase measurements.
`k == 0` returns an unchanged destination; `k < 0` ranks all matches. `FilterTime`
and `FilterGeo` filter supplied candidate slices without a plan. Nil or zero
value `Workspace` is valid; it is private to one execution and retains scratch
between calls. Use `NewWorkspace(capacity)`, `EnsureCapacity`, `Reset`, `Cap`,
`RetainedBytes`, and `Trim` to manage reuse. Plans can be shared across readers
while the store remains open; there is no global snapshot.

```go
executor, err := search.NewExecutor(search.ExecutorConfig{
    WorkspaceCapacity: 4096, MaxRetainedBytes: 8 << 20,
})
if err != nil { return err }
executor.Prewarm(4)
ids, err := executor.AppendIDs(ctx, nil, plan)
count, err := executor.Count(ctx, plan)
```

`Executor` also offers `Search`, `SearchWithMetrics`, `AppendPage`,
`FilterIDs`, `FilterIDsWithMetrics`, and `Exists`. `MaxRetainedBytes` is a
**per-idle-workspace** cap, not a process cap. Metrics are opt-in and
caller-owned (`search.NewSearchMetrics(plan)`); the uninstrumented path avoids
timing overhead. See [`search/README.md`](search/README.md) for ranking fusion
and [`search/PERFORMANCE.md`](search/PERFORMANCE.md) for benchmarks.

### ACL grants and principals

`search.NewUserACL(schema.UserID(id))` and `NewGroupACL(schema.GroupID(id))`
validate nonzero IDs and encode them as enum values for the dedicated bitmap
namespaces. `WithACL` uses the user's posting plus postings for the supplied
authenticated groups; it does not establish group membership. For grants,
bind `search.NewUserGrants(userBitmap)` and
`search.NewGroupGrants(groupBitmap)` to the dedicated namespaces, then call
each handle's `Add(principalID, imageIDs...)` or `Remove(...)`. Grant helpers
wait for the bitmap write barrier; granting an unknown image ID can create a
posting, so the unified entity liveness filter remains essential. Compile
against the unified store for automatic image liveness, or with
`CompileWithStore` when using a standalone bitmap store. Do not apply image
grants to non-image entities.

## Import matching: exact and perceptual hashes

`db.Matcher(index.MatcherSpec{SHA256: shaNS, BLAKE3: blakeNS, Perceptual: simNS})`
binds image namespaces. Zero namespace numbers skip an unneeded family.
`MatchItem` has optional `SHA256` and `BLAKE3` pointers to
`radixhash.Hash`, `PHash *uint64`, and `PDQ *simhash.PDQHash`; nil fields are
skipped. `MatchParams{PHashDistance, PDQDistance, DetectIntraBatch}` uses native
Hamming radii, clamps PDQ distance to `simhash.MaxPDQSearchDistance`, and can
report duplicates among incoming items. `Match`/`MatchOne` append live image
hits; `MatchWithWorkspace` accepts reusable `MatchWorkspace{Exact, Similar}`
scratch. `Match{Item, Kind, ImageID, Distance, Intra, Peer}` distinguishes
stored hits from intra-batch peers. This import workflow does not create
application stack/asset records.

## Standalone engine API reference

The unified store owns its engines and their lifecycle: do **not** close a
pointer returned by `db.Bitmap/Time/Geo/Hash/Sim/Vector` separately. Standalone
constructors below are for independent stores and experiments. They do not
automatically share unified liveness or crash coordination.

### `index/bitmapidx`

`NewMemory(spec, NamespaceConfig{})` creates a single in-memory namespace;
`NewStore(dir, []NamespaceSpec{...}, config, durability)` and
`OpenStore(dir, config, durability)` manage a standalone persistent bitmap
store. `NamespaceConfig` has `ActiveTarget` (rotate delta), `HardDeltaLimit`
and `MaxDeltaValues` (bound visible deltas), and `CompactDelay` (delay
background compaction); zero selects defaults.

| Namespace shape | Values / writes | Reads |
|---|---|---|
| Enum | `EnumStringValue`, `EnumBytesValue`; `Batch.Add/Remove` | `MatchValues`, `ValueIDs`, `CountValues`, `FilterValues`, `Compile` |
| Bool | `BoolValue`; `Batch.Add/Remove` | Same value-query methods; false differs from absent |
| String | `NamespaceSpec.Analyzer` (`CodecSpec`); `Batch.ReplaceText` | `MatchText`, `CompileText` (all analyzed tokens) |
| Continuous | `IntValue` on the stored signed grid; `Batch.Move` or Add/Remove | `Range`, `EstimateRange`, `CountRange`, `FilterRange`; `Between` is half-open |
| BSI | `Uint16Value` / `Uint32Value` with configured `BSIWidth16/32`; `Set`, `Delete`, `Batch.BSIMove` | `Get`, `Equal`, `NotEqual`, `Less`, `LessOrEqual`, `Greater`, `GreaterOrEqual`, `BSIRangeQuery`, `BSICountRange`, `BSIFilterRange` |

`BSIRange(min,max)` and `ValuesBetween(min,max)` include both endpoints;
BSI zero is a stored value, distinct from absence. Width overflow errors,
not truncates. Numeric `Quantizer{Code,Version,Scale,Offset}` documents how
applications encode continuous values; the API takes the already-converted
grid value. Text has no reverse document lookup: replacements need previous
and new tokens. A namespace `Batch` supports `Add`, `Remove`, `Move`,
`ReplaceText`, `BSIMove`, `Apply`, `Reset`, and `Close`; `Apply` queues work and
`Index.Barrier()` waits for committed visibility. Standalone maintenance:
`Compact`, `Vacuum`, `WaitIdle`, `Close`/store `Checkpoint`.

### `index/timeidx`

`NewMemory(spec, config)`, `NewStore(dir, spec, config, durability)`,
`OpenStore(dir, config, durability)`, and `OpenNamespace(dir, ns, config,
durability)` create/open timestamp indexes. Use `timeidx.NewBatch(capacity)`
with `Insert(id, timestamp)`, `Move(id, before, after)`, `Delete(id,
timestamp)` and `Index.Apply(batch)`; reset the batch for reuse. `Search(dst,
start,end)` is a half-open candidate scan; `SearchExact(..., lookup)` verifies
against canonical timestamps. `EstimateRange`, `Len`, `Sync`, `Checkpoint`,
`Compact`, `Vacuum`, and generation checks are available. `Config.MaxShardBytes`
sets the shard payload cap, `MaxFileBytes` the initial sparse arena;
`BlockSize`, `DeltaCapacity`, `WriteQueueCapacity`, and `CompactionQueue` are
legacy/deprecated tuning fields, not a preferred path.

### `index/geoidx`

Use `NewMemory(config)`, `New(dir, config)`, `Open(dir, config)`, or the
single-file `NewFile`/`OpenFile` forms. `Config.Namespace` labels standalone
indexes; `MaxCells` sets the default cover budget; `TargetDelta`,
`CompactionTrigger`, `MaxDelta`, `TargetShardBytes`, `MinShardBytes`,
`MaxShardBytes`, and `MaxFileBytes` control memory/write/storage bounds (zero
selects defaults). Convert coordinates via `KeyFromLatLng` or
`CellIDFromLatLng`. `Insert`, `Delete`, `InsertBatch`, `DeleteBatch`, and
`Apply(inserts,deletes)` mutate keys. **Insertion is ID-authoritative**: an
insert at a new key moves the ID without requiring a separate delete of its
old key. A query uses `SearchCell`, `SearchRanges`, `SearchRect`,
`SearchPolygon`, `SearchCover`, `SearchRadius`, `SearchRadiusResults`, or
`Nearest`. Geography is resolved to S2 level 19 (~20 m); `Result.Distance`
measures to the cell center, not an independently stored exact coordinate.
`CoverOptions{MaxCells}` bounds region covers. For bitmap input/output, use
`FilterCell/Ranges/Rect/Polygon/Cover/Radius` or the analogous `Collect*`
methods with `roar.MutableBitmap`. `Sync`, `Checkpoint`, and `Compact` are
separate; close is not a substitute for an explicit checkpoint.

### `index/radixhash` and `index/simhash`

Exact hashes: `radixhash.New(arenaBytes, config)`, `NewWithArena`,
`NewMmap(path, arenaBytes, config)`, and `OpenMmap(path, config)`; use
`Apply([]Entry)`, `Remove([]Entry)`, `Lookup(dst, hash)`, `LookupFiltered`,
`Contains`, and `Count`. `Config.PromotionBytes`, `MaxLeafSlots`, and
`AutoReclaimBytes` tune tree promotion/reclamation; zero selects defaults.
`Sync`, `Checkpoint`, `Compact`, `Reclaim`, `Close` manage storage/lifetime.
`radixhash` does not hash content or record an algorithm identifier.

Perceptual hashes: `simhash.Open(dir, config)` opens or creates a store;
`OpenFile(prefix, config)` names files within a shared directory. `Config` has
`MaxShardBytes` (default 4 GiB), `SeqBlockRecords` (power of two dividing
65,536; default 1024), and `Durability`. `Append`/`AppendBatch` assign dense
IDs; `Upsert`/`UpsertBatch` update existing records; `Get`, `Quality`,
`SearchPHash`, `SearchPDQ`, and `SearchPDQBulk` expose reads. Results carry
exact native Hamming distance in ascending ID order. `Sync`, `Checkpoint`,
`Close` are distinct; explicitly checkpoint as required by your durability
policy.

### `index/vectoridx`

`NewMemory(config)`, `NewStore(path, config, durability, opts...)`, and
`OpenStore(path, durability, opts...)` create/open a vector space. The
`Config` requires a nonzero namespace, supported dimensions (`Dim512`,
`Dim768`, `Dim1024`), and an explicit element type (`ElementInt8`,
`ElementUint8`, `ElementInt16`, `ElementUint16`, `ElementFloat32`). `Metric`
is `MetricL2` (default), `MetricCosine`, or `MetricDot`. Mutate with the
type-matched `AddFloat32/Int8/Uint8/Int16/Uint16` or `Delete`; `AddAsync`
and `DeleteAsync` queue background work. `SearchFloat32/Int8/Uint8/Int16/Uint16`
and the corresponding `SearchInto*` methods return ranked `Result`s;
`ExactSearchFloat32` is a full-scan reference. `CandidateFilter` (or nil for
all live nodes) gates returned IDs, not graph traversal. `SearchWithFilterHint`
allows an estimated filter cardinality. `Sync`, `Checkpoint`, `Compact`,
`Vacuum`, `Verify`, `WaitIdle`, `Stats`, and `Metadata` support maintenance.

| `vectoridx.Config` option | Meaning |
|---|---|
| `M`, `M0`, `EfConstruction`, `EfSearch` | Graph degree/build/search budget; zero selects defaults. |
| `DynamicEfFactor`, `DynamicEfMin`, `DynamicEfMax` | Dynamic search budget; zero factor disables. |
| `FlatSearchCutoff`, `FlatThreshold` | Exact-scan thresholds; zero defaults, negative disables/forces ANN as defined by each field. |
| `OversampleFactor`, `FilterAware`, `DisableFilterAware` | Filtered search oversampling/filter-aware behavior. |
| `UseHeuristic`, `DisableHeuristic` | Diversity-based neighbor selection; enabled by default unless disabled. |
| `AutoCompactRatio`, `CompactMinNodes` | Async tombstone compaction; ratio zero disables. |
| `AutoVacuumDeltas`, `AutoVacuumFragmentation` | Async delta vacuum triggers; zero disables. |

Persistent vector constructor options include `vectoridx.WithSyncInterval`
and `vectoridx.WithWALSegmentBytes`. The unified store routes vector values
using `index.VectorValue` and applies its runtime `index.Options.Vector` policy.
IDs must fit `uint32`; results identify `ValueID`/`NodeID` rather than an
arbitrary application object. ANN results can be filtered through
`search.Plan.FilterRanked` (provide an ID and integer-score projection), or
ranked/fused with `search/ranking`.

### `store`: separate key-value collection

`store.Open(dir, specs ...store.Spec)` opens or creates a Pebble-backed store;
on creation define each `Spec{Namespace, Name}` and keep them stable on reopen.
This is **not** added to `index.Store` or searched by `search.Plan`.

```go
kv, err := store.Open("./data/kv", store.Spec{Namespace: 200, Name: "metadata"})
if err != nil { return err }
defer kv.Close()
key := store.Key{Namespace: 200, ID: 100}
if err := kv.Set(key, []byte("camera: X100V")); err != nil { return err }
value, err := kv.Get(key)
err = kv.Scan(200, func(key store.Key, value []byte) error { return nil })
err = kv.Delete(key)
```

Check each error before continuing in real code. See the package APIs for
zero-copy/ownership details of returned values.

## Ranking, performance and further references

`Plan.RankTime` and `Plan.RankGeo` retain full-precision scores. For presentation,
`search/ranking` encodes independent lanes to `uint16` ranks (`NoRank=0`,
`MaxRank=65535`), with `EncodeDistance`, `EncodeGeoDistance`,
`EncodeTimeDistance`, `QuantizeCosine`, and bulk helpers. `Signals` holds up to
`MaxSignals` (4) positional lanes; `RRF.Fuse` performs reciprocal-rank fusion
(`K` defaults to `DefaultRRFK=60`). `Sort` or `TopK` orders final IDs/ranks;
callers own buffers and must not share an `RRF` instance concurrently. The
`...WithMetrics` forms add opt-in rank-phase measurements.

```sh
go run ./cmd/imageidx
go run ./cmd/imageidx/examples/basic
go run ./cmd/imageidx/examples/service
go run ./cmd/imageidx/examples/bitmaponly
go build ./...
go test ./...
go test -race ./...
```

See [`index/README.md`](index/README.md) for crash/recovery details,
[`search/README.md`](search/README.md) for planning/ranking examples, and
[`AGENTS/README.md`](AGENTS/README.md) for current agent guidance versus
historical design notes. The `schema.dat` persistent-format contract lives in
[`schema/FORMAT.md`](schema/FORMAT.md).

## Continuous integration

`.github/workflows/ci.yml` runs on Linux AMD64 and ARM64: `go build ./...`,
`go vet ./...`, `go test ./...`, a full `go test -race ./...` job, a schema
recovery/golden-fixture job, a database migration crash job, and an
informational allocator benchmark job. The allocator fast path must stay at
`0 B/op, 0 allocs/op`.

CI-tested platforms are Linux AMD64 and ARM64 (both schema backends use
`flock`-based locking). Windows is **not supported**: the crash-recovery and
file-locking suites assume POSIX semantics, so `schema`/`database` mutating
storage is documented as unavailable there.

### Known unrelated CI failures

On GitHub's AMD64 Linux runners, two pre-existing microkernel packages fail
their own equivalence tests and are excluded from the AMD64 test and race jobs:

- `index/radixhash/internal/tailkernel` — `TestMatchMask*` backend vs scalar.
- `index/simhash/internal/hashkernel` — `TestAVX2PDQBlockMaskAgainstScalar`.

Neither package is touched by the schema work, and both pass on Linux ARM
(where the AMD64 assembly is not built). They are tracked separately and must
be fixed before Linux AMD64 can be declared fully supported for those kernels.
