# imageIdx

Image indexing tools written in Go 1.27. The `common` package defines the
shared `ImageID`, principal (`UserID`/`GroupID`), and durability types, plus
the namespace and schema vocabulary that identifies every indexed collection.
The `roar` package owns the immutable zero-copy bitmap representation, used
directly by the logical indexes in `index/bitmapidx`, `index/timeidx`, and
`index/vectoridx`. The `search` package compiles and executes queries across
them.

- `bitmapidx` provides persistent typed namespaces for enum, bool, text,
  continuous and geo values.
- `timeidx` indexes timestamps for range search.
- `vectoridx` indexes fixed-dimension embeddings with persistent HNSW ANN search.
- `search` combines namespace, timestamp and geographic clauses with AND
  semantics.

Every collection is registered in a `common.Schema` under a unique namespace
number and name; see `common/namespace.go` and `common/schema.go`.

Pre-v1 note: `timestamp` and `geoindex` were renamed to `timeidx` and
`geoidx`; geo is now part of `bitmapidx`. Persisted geo formats are unchanged.

## Roar bitmap index

The `roar` package stores the serialized bitmap as its read representation:
sorted high-48-bit keys with byte offsets, sorted `uint16` array containers
for sparse ranges, and 1,024-`uint64` bitmap containers for dense ranges.
Reads operate directly on mmap-friendly bytes without decoding objects.

```go
bitmap, err := roar.FromSorted([]uint64{1<<48 + 1, 1<<48 + 2, 2<<48 + 9})
if err != nil {
	log.Fatal(err)
}

bitmap.Contains(1<<48 + 2)                // true, zero allocation
n := roar.AndCardinality(bitmap, other)   // fused count, no result bitmap

results := make([]uint64, 0, 256)
results = bitmap.AppendTo(results)        // fast ascending materialization
```

`AndInto`/`OrInto`/`AndNotInto` write into a reusable `Builder`, and
`AndManyCardinality` counts N-way intersections without intermediate bitmaps.
`FromSorted` builds a bitmap from sorted IDs; `FromUnsorted` sorts first.
`Open` validates stored or mmap-backed bytes; `OpenTrusted` skips validation
for already-verified bytes. The serialized format is 8-byte aligned throughout.
Benchmarks in `roar/benchmark_test.go` compare against `weaviate/sroar` using
photo-database-shaped distributions.

## Timestamp index

The `timeidx` package provides a scalar, block-local timestamp index designed
for large image collections. Its current implementation includes:

- immutable, sorted structure-of-arrays base blocks;
- fixed-capacity active and frozen deltas with a hard three-layer read bound;
- lock-free, zero-allocation range searches with caller-owned result buffers;
- a single-owner batched writer and background copy-on-write compaction;
- internal read-side lifetime tracking for safe delta reuse;
- a checksummed write-ahead log with optional sync-on-write durability;
- page-aligned sequential snapshots, atomic manifests, mmap reopen, and vacuum;
- scalar search kernels only; SIMD is reserved for a later implementation.

```go
idx, err := timeidx.New(timeidx.Spec{
	Namespace: common.Namespace(90), // register in common.Schema
	Name:      "timeline",
	Offset:    offset,
	Entries: []timeidx.Entry{
		{ImageID: 100, Timestamp: createdAt},
	},
}, timeidx.Config{})
if err != nil {
	log.Fatal(err)
}
defer idx.Close()

results := make([]timeidx.ImageID, 0, 256)
results = idx.Search(results, start, end)
```

`Search` returns compressed-index candidates. Use `SearchExact` with the
canonical timestamp column when exact low-bit boundary semantics are required.
Reuse a `Batch` to keep normal updates allocation-free. Mutations are created
with `Insert`, `Delete`, and `Move`:

```go
batch := timeidx.NewBatch(1)
batch.Append(timeidx.Move(100, oldTimestamp, newTimestamp))
if err := idx.Apply(batch); err != nil {
	log.Fatal(err)
}
batch.Reset()
```

For persistence, `NewPersistent` creates and checkpoints a store directory, and
`OpenPersistent` maps the published generation and replays the WAL tail:

```go
persistent, err := timeidx.NewPersistent(
	"./data/time",
	spec,
	timeidx.Config{},
	common.DurabilityOS,
)
if err != nil {
	log.Fatal(err)
}
defer persistent.Close()

if err := persistent.Checkpoint(); err != nil {
	log.Fatal(err)
}
if err := persistent.Vacuum(); err != nil {
	log.Fatal(err)
}
```

## Namespace index (enum, bool, text, continuous, geo)

`bitmapidx` provides a unified typed namespace index. One `NamespaceIndex`
holds one namespace and supports five value shapes:

| Kind | Values | Query |
|---|---|---|
| `KindEnum` | discrete strings/bytes (camera model, keyword) | `MatchValues` |
| `KindBool` | explicit false/true; missing is neither | `MatchValues` |
| `KindText` | tokenized documents (title, caption) | `MatchText` |
| `KindContinuous` | `int64` grid values (ISO, focal length) | `Range` |
| `KindGeo` | level-16 S2 cell keys | `GeoRanges` |

Values are identified by a 64-bit xxh3 hash. Rare hash collisions claim a
dictionary slot, so distinct values never share membership. The built-in Latin
analyzer lowercases, accent-folds and tokenizes text.

Declare namespaces as values, register them in a `common.Schema`, and persist
the definitions with `NewIndex`:

```go
const (
	NamespaceTitle       common.Namespace = 100 // callers may allocate 100..65535
	NamespaceFocalLength common.Namespace = 101
)

tag := bitmapidx.NamespaceSpec{
	Namespace:  common.NamespaceTag,
	Name:       "tag",
	Kind:       bitmapidx.KindEnum,
	Storage:    bitmapidx.StoragePerType,
	Normalizer: bitmapidx.CodecSpec{Code: bitmapidx.NormalizerIdentity},
}
focal := bitmapidx.NamespaceSpec{
	Namespace: NamespaceFocalLength,
	Name:      "focal-length",
	Kind:      bitmapidx.KindContinuous,
	Storage:   bitmapidx.StoragePerType,
	Quant:     bitmapidx.Quantizer{Code: bitmapidx.QuantIdentity, Version: 1, Scale: 1},
}
title := bitmapidx.NamespaceSpec{
	Namespace: NamespaceTitle,
	Name:      "title",
	Kind:      bitmapidx.KindText,
	Storage:   bitmapidx.StoragePerType,
	Analyzer:  bitmapidx.CodecSpec{Code: bitmapidx.AnalyzerLatin, Version: bitmapidx.AnalyzerVersion},
}

// NewNamespaceSchema builds the shared namespace vocabulary; numbers and
// names must be unique within it.
schema, err := bitmapidx.NewNamespaceSchema(tag, focal, title)
if err != nil {
	log.Fatal(err)
}
```

Create or reopen the persistent namespace store with `NewIndex` and
`OpenIndex`. The store owns the namespace handles and their WAL; callers
must close the store rather than individual namespaces:

```go
store, err := bitmapidx.NewIndex(
	"./data/namespaces", []bitmapidx.NamespaceSpec{tag, focal, title},
	bitmapidx.NamespaceConfig{}, common.DurabilityOS,
)
if err != nil {
	log.Fatal(err)
}
defer store.Close()

tags, _ := store.Lookup(common.NamespaceTag)
batch := tags.NewBatch()
if err := batch.Add(bitmapidx.Entry{
	ImageID: 100, Value: bitmapidx.StringValue("Trekking"),
}); err != nil {
	log.Fatal(err)
}
if err := batch.Apply(); err != nil {
	log.Fatal(err)
}
batch.Close()
if err := tags.Barrier(); err != nil {
	log.Fatal(err)
}
if err := store.Checkpoint(); err != nil {
	log.Fatal(err)
}
```

Use `StoragePerValue` for low-cardinality namespaces and `StoragePerType` for
high-cardinality namespaces; both mutate and query the same way. Enum and bool
membership:

```go
tags, err := bitmapidx.NewNamespaceIndex(tag, bitmapidx.NamespaceConfig{})
if err != nil {
	log.Fatal(err)
}
defer tags.Close()

batch := tags.NewBatch()
if err = batch.Add(
	bitmapidx.Entry{ImageID: 100, Value: bitmapidx.StringValue("Trekking")},
	bitmapidx.Entry{ImageID: 200, Value: bitmapidx.StringValue("Trekking")},
	bitmapidx.Entry{ImageID: 300, Value: bitmapidx.StringValue("Travel")},
); err != nil {
	log.Fatal(err)
}
if err = batch.Apply(); err != nil {
	log.Fatal(err)
}
batch.Close()
if err = tags.Barrier(); err != nil {
	log.Fatal(err)
}

// MatchValues requires every supplied value (AND); one value is a set lookup.
match, err := tags.MatchValues(bitmapidx.StringValue("Trekking"))
if err != nil {
	log.Fatal(err)
}
ids := match.AppendTo(nil) // [100 200]

count, err := tags.CountValues(bitmapidx.StringValue("Trekking"))
ids, err = tags.ValueIDs(nil, bitmapidx.StringValue("Travel"))
filtered, err := tags.FilterValues(nil, candidates, bitmapidx.StringValue("Trekking"))
present, err := tags.Contains(100, bitmapidx.StringValue("Trekking"))

// Remove wins when an ID appears in both sets of one batch.
batch = tags.NewBatch()
if err = batch.Add(bitmapidx.Entry{ImageID: 400, Value: bitmapidx.StringValue("Trekking")}); err != nil {
	log.Fatal(err)
}
if err = batch.Remove(bitmapidx.Entry{ImageID: 400, Value: bitmapidx.StringValue("Trekking")}); err != nil {
	log.Fatal(err)
}
if err = batch.Apply(); err != nil {
	log.Fatal(err)
}
batch.Close()
if err = tags.Barrier(); err != nil {
	log.Fatal(err)
}

distinctValues := tags.Cardinality() // 2: "Trekking" and "Travel"
```

`CountValues` counts matching ImageIDs; `Cardinality` counts distinct values
with at least one live ImageID. Removing a value's final membership drops it
from cardinality without requiring a checkpoint or vacuum. On a
multi-namespace store, query one registered namespace or get counts for every
registered namespace (including empty ones):

```go
tagValues, ok := store.Cardinality(common.NamespaceTag)
byNamespace := store.Cardinalities() // map[common.Namespace]uint64
```

The per-namespace counts are sampled independently, not as one cross-namespace
snapshot when writes are concurrent.

Continuous values use explicit transitions, which resolve the zero-value
ambiguity (`To: 0` alone would otherwise be indistinguishable from "absent").
One batch may stage only one transition per image, so apply each step
separately:

```go
focals, err := bitmapidx.NewNamespaceIndex(focal, bitmapidx.NamespaceConfig{})
if err != nil {
	log.Fatal(err)
}
defer focals.Close()

batch := focals.NewBatch()
if err = batch.Move(bitmapidx.Move{ImageID: 100, To: 35, HasTo: true}); err != nil { // insert
	log.Fatal(err)
}
if err = batch.Apply(); err != nil {
	log.Fatal(err)
}
batch.Close()

batch = focals.NewBatch()
if err = batch.Move(bitmapidx.Move{ImageID: 100, From: 35, To: 50, HasFrom: true, HasTo: true}); err != nil { // move
	log.Fatal(err)
}
if err = batch.Apply(); err != nil {
	log.Fatal(err)
}
batch.Close()

batch = focals.NewBatch()
if err = batch.Move(bitmapidx.Move{ImageID: 100, From: 50, HasFrom: true}); err != nil { // remove
	log.Fatal(err)
}
if err = batch.Apply(); err != nil {
	log.Fatal(err)
}
batch.Close()
if err = focals.Barrier(); err != nil {
	log.Fatal(err)
}

// Between is [lo, hi); AtLeast and AtMost are open-ended. Bounds are int64 in
// the namespace's stored grid domain.
match, err = focals.Range(bitmapidx.Between(24, 70))
match, err = focals.Range(bitmapidx.AtLeast(200))
estimate, err := focals.EstimateRange(bitmapidx.Between(24, 70)) // planning hint
```

Geo namespaces store one level-16 S2 cell per image. Keys come from coordinates
or cells, and queries take inclusive key ranges produced by
`RangesFromCellIDs` or `CoverRegion`:

```go
geoSpec := bitmapidx.NamespaceSpec{
	Namespace: common.NamespaceLocation,
	Name:      "location",
	Kind:      bitmapidx.KindGeo,
	Storage:   bitmapidx.StoragePerType,
	Geo:       bitmapidx.GeoCodec{Version: 1},
}
locations, err := bitmapidx.NewNamespaceIndex(geoSpec, bitmapidx.NamespaceConfig{})
if err != nil {
	log.Fatal(err)
}
defer locations.Close()

key, err := bitmapidx.KeyFromLatLng(37.7749, -122.4194)
if err != nil {
	log.Fatal(err)
}
// One cell per image: insert, move and remove with explicit transitions.
batch := locations.NewBatch()
if err = batch.Move(bitmapidx.Move{ImageID: 100, To: int64(key)}); err != nil {
	log.Fatal(err)
}
if err = batch.Apply(); err != nil {
	log.Fatal(err)
}
batch.Close()
if err = locations.Barrier(); err != nil {
	log.Fatal(err)
}

cellIDs := []uint64{
	bitmapidx.CellIDFromLatLng(37.7749, -122.4194),
}
ranges, err := bitmapidx.RangesFromCellIDs(nil, cellIDs)
if err != nil {
	log.Fatal(err)
}
geoMatch, err := locations.GeoRanges(ranges)
if err != nil {
	log.Fatal(err)
}
geoIDs := geoMatch.AppendTo(nil)
geoCount, err := locations.CountGeoRanges(ranges)
geoFiltered, err := locations.FilterGeoRanges(nil, candidates, ranges)
geoEstimate, err := locations.EstimateGeoRanges(ranges) // planning hint

// Cover a region to ranges; Nearest returns exact top K using canonical
// coordinates supplied by the caller.
workspace := bitmapidx.NewGeoCoverWorkspace(64)
ranges, err = bitmapidx.CoverRegion(ranges[:0], region, bitmapidx.GeoCoverOptions{MaxCells: 32, Workspace: workspace})
nearest := locations.Nearest(nil, 37.7749, -122.4194, 10, coordinateLookup)
```

Stale `From` values and duplicate inserts into the same cell are rejected
before publication. No reverse index is kept, so a cross-cell
insert without `From` is caller discipline.

Text namespaces are replaced whole, so callers supply the old and new text
and the index diffs token sets. A token present in both before and after is
retained. One batch may stage at most one replacement per image:

```go
titles, err := bitmapidx.NewNamespaceIndex(title, bitmapidx.NamespaceConfig{})
if err != nil {
	log.Fatal(err)
}
defer titles.Close()

batch := titles.NewBatch()
if err = batch.ReplaceText(
	bitmapidx.TextReplacement{ImageID: 100, After: []string{"Golden Gate Bridge"}},
	bitmapidx.TextReplacement{ImageID: 200, After: []string{"Golden Gate Park"}},
); err != nil {
	log.Fatal(err)
}
if err = batch.Apply(); err != nil {
	log.Fatal(err)
}
batch.Close()

batch = titles.NewBatch()
if err = batch.ReplaceText(
	bitmapidx.TextReplacement{ImageID: 100, Before: []string{"Golden Gate Bridge"}, After: []string{"Golden Gate Park"}},
); err != nil {
	log.Fatal(err)
}
if err = batch.Apply(); err != nil {
	log.Fatal(err)
}
batch.Close()
if err = titles.Barrier(); err != nil {
	log.Fatal(err)
}

match, err = titles.MatchText("golden park") // AND of analyzed tokens
```

Dictionary inspection and maintenance:

```go
key, ok, err := tags.KeyForValue(bitmapidx.StringValue("Trekking"))
value, ok := tags.LookupValue(nil, key) // appends canonical bytes to the buffer

tags.Iterate(func(key bitmapidx.NamespaceKey, value []byte) bool {
	fmt.Println(key, string(value))
	return true // return false to stop
})
tags.Prefix([]byte("Tr"), func(key bitmapidx.NamespaceKey, value []byte) bool {
	return true
})

// Compact folds delta layers into the base. Vacuum also drops values with no
// remaining posting and may reuse their collision slots.
if err := tags.Compact(); err != nil {
	log.Fatal(err)
}
if err := tags.Vacuum(); err != nil {
	log.Fatal(err)
}
```

`OpenIndex` restores the persisted registry, mmap snapshot and WAL
tail. `Checkpoint` publishes a new snapshot; `Vacuum` also removes obsolete
generations and dead dictionary values. Namespace query results are immutable
`roar.Bitmap` values. Mutable bitmaps are reserved for transient `search`
workspace accumulation.

## Geographic index

Geo is a `bitmapidx` namespace kind. Each image holds one level-16 S2 bucket
key; higher-level areas are searched by expanding cells into lexicographic key
ranges, so any S2 level up to 16 maps onto the same bucket table.
`search.InCells` takes the namespace handle directly, and the same queries are
available without the search package.

```go
locations, err := bitmapidx.NewGeoNamespace(common.NamespaceLocation, []bitmapidx.GeoEntry{
	{ImageID: 100, Latitude: 37.7749, Longitude: -122.4194},
}, bitmapidx.NamespaceConfig{})
if err != nil {
	log.Fatal(err)
}
defer locations.Close()

// One cell per image: insert, move and remove with explicit transitions.
losAngeles, err := bitmapidx.KeyFromLatLng(34.0522, -118.2437)
if err != nil {
	log.Fatal(err)
}
batch := locations.NewBatch()
if err := batch.Move(bitmapidx.Move{ImageID: 200, To: int64(losAngeles)}); err != nil {
	log.Fatal(err)
}
if err := batch.Apply(); err != nil {
	log.Fatal(err)
}
batch.Close()
if err := locations.Barrier(); err != nil {
	log.Fatal(err)
}

cellIDs := []uint64{
	bitmapidx.CellIDFromLatLng(37.7749, -122.4194),
}
ranges, err := bitmapidx.RangesFromCellIDs(nil, cellIDs)
if err != nil {
	log.Fatal(err)
}
match, err := locations.GeoRanges(ranges) // inclusive key-range union
if err != nil {
	log.Fatal(err)
}
ids := match.AppendTo(nil)
count, err := locations.CountGeoRanges(ranges)
filtered, err := locations.FilterGeoRanges(nil, candidates, ranges)
```

Moves and removals use explicit transitions. Geo keys are never zero, so the
presence flags may be omitted there (`To: key` inserts, `From: key, To: key`
moves, `From: key` removes); continuous namespaces keep the flags because zero
is a valid grid value. `GeoValue` carries a bucket key for direct batch
`Add`/`Remove`. Stale `From` values and duplicate inserts into the same bucket
are rejected before publication, and no reverse image-to-cell map is retained,
so a cross-cell insert without `From` is caller discipline.

`CoverRegion` converts an `s2.Region` to ranges. Supply both a reusable
`GeoCoverWorkspace` and a destination with sufficient capacity for the
allocation-free path; `Nearest` returns exact top-K using a caller-supplied
coordinate lookup. Allocation behavior inside the caller-supplied region's
`IntersectsCell` and `ContainsCell` methods remains the responsibility of that
region implementation. Geo namespaces use the same persistent `Index` as the
other namespace kinds.

## Vector index

The `vectoridx` package provides persistent HNSW approximate-nearest-neighbor
search over fixed-dimension embeddings (`512`, `768`, or `1024`) in one of
five element types (`int8`, `uint8`, `int16`, `uint16`, `float32`), ranked by
squared L2 distance. Each vector space registers a `common.Namespace` with
kind `common.KindVector`. `NodeID` is the compact identifier stored in the
graph; results widen it to the database-wide `common.ImageID`
(`vectoridx.ImageID`), so an image-embedding index addresses images directly
with no second mapping. See `vectoridx/README.md` and `AGENTS/VectorIdx.MD`.

```go
idx, err := vectoridx.Create(path, vectoridx.Config{
    Namespace:   common.Namespace(100), // registered as common.KindVector
    Dimensions:  vectoridx.Dim512,
    ElementType: vectoridx.ElementFloat32,
}, common.DurabilitySync)
if err != nil {
    log.Fatal(err)
}
defer idx.Close()

if err := idx.AddFloat32(100, vec); err != nil {
    log.Fatal(err)
}
results, err := idx.SearchFloat32(query, nil, 10)
if err != nil {
    log.Fatal(err)
}
```

Deletion is tombstone-only (`Delete` keeps nodes traversable but never
returned); re-adding a deleted NodeID replaces its vector and reconnects it,
and `Compact` rebuilds the graph from live vectors without renumbering
NodeIDs. `CandidateFilter` applies bitmap-style membership after ANN
discovery with oversampling and an exact-scan fallback:

```go
filter := vectoridx.CandidateFilter(func(id vectoridx.NodeID) bool {
    return allowSet[id]
})
results, err := idx.SearchFloat32(query, filter, 10)
```

Persistence is a single file (superblock, region directory, node metadata,
vectors, graph, tombstones) plus a framed WAL with prefix recovery through
`vectoridx.Open`. `SearchInto` variants reuse caller storage and pooled
scratch for zero steady-state allocations.

## Combined search

The `search` package compiles and executes clauses across namespace, timestamp
and geographic indexes. Every clause is combined with AND semantics:

- all namespace clauses must contain the image;
- every timestamp range must contain the image (lossy-only ranges on the same
  index coalesce by intersection; each `ExactTimestamp` range is separate);
- every geographic range set must contain the image.

The compiler orders these stages using live cardinality estimates. This only
changes execution order; it does not change query results. Results are returned
as sorted, unique `search.ImageID` values. Every package aliases the
canonical `common.ImageID` type. Clauses are intersected; `WithACL` is
the built-in exception that unions authenticated user and group grants within
one clause. Ordering and reranking are opt-in (`RankTime`, `RankGeo`, and the
`FilterRanked` bridge for `vectoridx` ANN results); `AppendPage` returns a
bounded sorted prefix. The full API reference lives in
`search/README.md`.

```go
plan, err := search.Compile(
	search.AllValues(tags, bitmapidx.StringValue("Trekking")),
	search.AllValues(favorites, bitmapidx.StringValue("favorite")),
	search.During(
		timeIndex,
		start,
		end,
		search.ExactTimestamp(lookupCanonicalTimestamp),
	),
	search.InCells(geo, cellIDs...),
)
if err != nil {
	log.Fatal(err)
}

// Plans and workspaces can be reused. AppendIDs reads the indexes' current
// published views; the estimates captured by Plan affect ordering only.
workspace := search.NewWorkspace(4_096)
matches := make([]search.ImageID, 0, 256)
matches, err = plan.AppendIDs(ctx, matches, workspace)
```

For a one-off query, pass clauses directly to `search.SearchOnce`. It compiles and
executes them immediately:

```go
matches, err := search.SearchOnce(ctx, nil,
	search.AllValues(tags, bitmapidx.StringValue("Trekking")),
	search.During(timeIndex, start, end),
	search.InCells(geo, cellIDs...),
)
```

Reusing a `Plan`, caller-owned result slice, and `Workspace` avoids rebuilding
planning metadata and retains intermediate buffers. With sufficient capacities,
the combined search path performs zero heap allocations.

A `Plan` exposes several execution shapes over the same compiled stages:

```go
// Count matches without retaining IDs.
count, err := plan.Count(ctx, workspace)

// Stop after N sorted matches (truncation, not ranking).
top, err := plan.AppendPage(ctx, nil, workspace, search.Page{Limit: 100})

// Stream sorted, unique matches without building a result slice.
err = plan.Visit(ctx, workspace, func(id search.ImageID) bool {
	fmt.Println(id)
	return true
})

// Cancellation is checked between stages and batches.
matches, err = plan.AppendIDs(ctx, matches[:0], workspace)
```

For long-lived services, `search.Executor` pools exclusive workspaces across
concurrent queries and plans:

```go
executor, err := search.NewExecutor(search.ExecutorConfig{WorkspaceCapacity: 4_096})
if err != nil { log.Fatal(err) }
executor.Prewarm(4)

matches, err = executor.AppendIDs(ctx, matches[:0], plan)
count, err = executor.Count(ctx, plan)
```

Clauses are conjunctive: `search.AllValues`, `search.FieldText` and
`search.FieldRange` query typed namespaces; `search.During` handles a half-open
timestamp range (optionally with `search.ExactTimestamp`); and
`search.InCells` queries a `KindGeo` namespace. Constructors are validated by
`Compile`. `vectoridx` is not a clause source.

For access control, compile `search.WithACL(userID, authenticatedGroupIDs...)`
with `search.CompileWithStore(store, clauses...)`. The clause unions direct
UserACL and GroupACL postings; callers authenticate group membership.
See Access control (ACL) below.

Timestamp ranges use the timestamp index's compressed candidate semantics.
Add `search.ExactTimestamp` when exact boundary filtering is required; only
exact timestamp stages can filter candidates in place. An empty range
(`start >= end`) compiles to a valid empty plan. Geographic search options
are S2 cell IDs, not latitude/longitude values: `search.InCells` accepts
one or more valid cells and normalizes them to level-16 descendant ranges
(coarser cells expand, finer cells snap to their ancestor). Exact polygon or
coordinate verification belongs in the caller's canonical-data layer before or
after the candidate search.

A combined search does not create a database-wide transactional snapshot. Each
stage reads one independently published immutable view, consistent with the
standalone index APIs. `search.Compile` and `search.SearchOnce` return
`search.ErrEmptyQuery` when no clauses are supplied and `search.ErrInvalidClause`
for a missing index.

Reader lifetime tracking is automatic. Every namespace read registers with its
internal tracker, and `Index.Close` waits for reads that could still
reference retired or mmap-backed state. Callers open one persistent
`Index`, obtain namespaces with `Lookup`, and close the store after use.

## Access control (ACL)

Access control is a `search`-level policy over two dedicated `bitmapidx`
`KindEnum` namespaces. `bitmapidx` only stores opaque values; all principal
semantics live in `search`:

- `common.NamespaceUserACL` holds one posting per user: the `ImageID`s
  granted directly to that user.
- `common.NamespaceGroupACL` holds one posting per group: the `ImageID`s
  granted to that group.
- A principal is its `uint64` ID (`common.UserID` / `common.GroupID`) in
  fixed 8-byte little-endian form. Each principal kind has its own namespace
  number, so equal IDs in the user, group, or any ordinary enum namespace
  never collide. ID zero is reserved and rejected everywhere.

imageIdx stores image grants, not user-to-group membership. Your application
authenticates which groups a user belongs to and passes those IDs to the
query; there is no public-image bypass — images without a grant never match
a `WithACL` clause.

Declare the namespaces alongside the rest of the store. A user-only deployment
needs only the user namespace; group queries additionally require the group
namespace:

```go
namespaces := []bitmapidx.NamespaceSpec{
	{Namespace: common.NamespaceUserACL, Name: "user-acl", Kind: bitmapidx.KindEnum, Storage: bitmapidx.StoragePerType},
	{Namespace: common.NamespaceGroupACL, Name: "group-acl", Kind: bitmapidx.KindEnum, Storage: bitmapidx.StoragePerType},
	{Namespace: common.NamespaceTag, Name: "tag", Kind: bitmapidx.KindEnum, Storage: bitmapidx.StoragePerType},
}
store, err := bitmapidx.NewIndex("./data/namespaces", namespaces, bitmapidx.NamespaceConfig{}, common.DurabilityOS)
if err != nil {
	log.Fatal(err)
}
defer store.Close()
```

Grant and revoke images with `search.UserGrants` and `search.GroupGrants`,
bound once to the looked-up namespaces. `Add`, `Contains`, and `Remove` take
typed IDs (`common.UserID` / `common.GroupID` / `search.ImageID`);
zero IDs report `search.ErrInvalidPrincipal`. Batches of up to 8 images
stay on the stack and share one encoded key, so the helpers add no
allocations of their own:

```go
userNamespace, _ := store.Lookup(common.NamespaceUserACL)
groupNamespace, _ := store.Lookup(common.NamespaceGroupACL)
users, err := search.NewUserGrants(userNamespace)
if err != nil {
	log.Fatal(err)
}
groups, err := search.NewGroupGrants(groupNamespace)
if err != nil {
	log.Fatal(err)
}

// Direct grant: image 100 is visible to user 42.
if err := users.Add(42, 100); err != nil {
	log.Fatal(err)
}
// Group grant: image 100 is visible to every member of group 7.
if err := groups.Add(7, 100); err != nil {
	log.Fatal(err)
}
if ok, err := users.Contains(42, 100); err != nil || !ok {
	log.Fatal(ok, err)
}

// Revoking the direct grant leaves the group grant intact.
if err := users.Remove(42, 100); err != nil {
	log.Fatal(err)
}
```

The single-principal `search.NewUserACL` / `search.NewGroupACL` constructors
and their `Value()` / `Parse*` helpers remain for audit paths: resolving a
principal back from dictionary bytes returned by `LookupValue` or `Iterate`.

Query with `search.WithACL`, compiled through `search.CompileWithStore` so
the clause can bind the dedicated namespaces. The clause unions the user's
posting with one posting per authenticated group, then intersects that union
with the rest of the query. Repeated group IDs are deduplicated:

```go
plan, err := search.CompileWithStore(store,
	search.WithACL(42, authenticatedGroupIDs...),
	search.AllValues(tags, bitmapidx.StringValue("visible")),
)
if err != nil {
	log.Fatal(err)
}
matches, err := plan.AppendIDs(ctx, nil, search.NewWorkspace(4_096))
```

Misuse is a compile error (`search.ErrInvalidClause`): `WithACL` without a
store, user or group ID zero, or a missing required ACL namespace. Plans keep
compiled namespace queries, so the store must stay open while they execute;
grants added after compilation are picked up by subsequent executions, and
`plan.Refresh` re-samples planning estimates.

Audit group exposure without touching your membership store: each group
posting lists exactly the images granted through it. Resolve a principal
back from dictionary bytes with `search.ParseUserACL` /
`search.ParseGroupACL`:

```go
editors, err := search.NewGroupACL(7)
if err != nil {
	log.Fatal(err)
}
imageIDs, err := groupNamespace.ValueIDs(nil, editors.Value()) // images granted to group 7
```

## Runnable command

`cmd/imageidx` constructs a small compatible namespace, timestamp, and
geographic index set. Its default one-shot mode executes `search.SearchOnce` once and prints a
JSON result:

```sh
go run ./cmd/imageidx
# {"image_ids":[100,200]}
```

Focused, copy-pasteable programs live under `cmd/imageidx/examples`:

```sh
go run ./cmd/imageidx/examples/basic      # bitmap + timestamp + geo plan
go run ./cmd/imageidx/examples/bitmaponly # AppendIDs, Count, AppendPage, Visit
go run ./cmd/imageidx/examples/service    # shared Plan with pooled Executor
```

Server mode demonstrates a long-lived HTTP function:

```sh
go run ./cmd/imageidx -mode server -addr :8080
```

The command logs a ready-to-use `/search` URL. The endpoint accepts decimal
`start` and `end` timestamp values plus one or more decimal or `0x`-prefixed
`cell` query parameters. Geographic search input remains S2 cell IDs, matching
the library API.

Persistent files retain their native SoA element widths and bitmap encodings.
Fixed records, region offsets, and file boundaries are eight-byte aligned;
large mmap payloads remain page aligned.

## Development

```sh
go run ./cmd/imageidx
go run ./cmd/imageidx -mode server -addr :8080
go test ./...
go test -race ./...
go test ./index/timeidx -run '^$' -bench 'Benchmark(SearchParallel|UpdateParallel)$' -benchmem
go test ./index/bitmapidx -run '^$' -bench BenchmarkNamespaceIndex -benchmem
go test ./search -run '^$' -bench BenchmarkCombinedSearch -benchmem
```

Set `IMAGEIDX_BENCH_LARGE=1` to enable the synthetic 10M and 100M-entry
benchmark builds.
