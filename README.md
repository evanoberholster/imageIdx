# imageIdx

Image indexing tools written in Go 1.27. The root `imageidx` package defines
the shared `ImageID` and durability types. The `roar` package owns the
immutable zero-copy bitmap representation, used directly by the logical
indexes in `bitmapidx`, `timeidx`, and `vectoridx`. The `search` package
compiles and executes queries across them.

- `bitmapidx` provides persistent typed fields for enum, bool, text, continuous
  and geo values.
- `timeidx` indexes timestamps for range search.
- `vectoridx` indexes fixed-dimension embeddings with persistent HNSW ANN search.
- `search` combines field, timestamp and geographic clauses with AND
  semantics.

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
	IndexID: 1,
	Offset:  offset,
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
	imageidx.DurabilityOS,
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

## Field index (enum, bool, text, continuous, geo)

`bitmapidx` provides a unified typed field index. One `FieldIndex` holds one
field and supports five value shapes:

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

Declare fields as values and persist the registry with `NewPersistentFields`:

```go
const (
	IndexTypeTitle       bitmapidx.IndexType = 100 // callers use 100..65535
	IndexTypeFocalLength bitmapidx.IndexType = 101
)

tag := bitmapidx.Field{
	Type:       bitmapidx.IndexTypeTag,
	Kind:       bitmapidx.KindEnum,
	Storage:    bitmapidx.StoragePerType,
	Normalizer: bitmapidx.CodecSpec{Code: bitmapidx.NormalizerIdentity},
}
focal := bitmapidx.Field{
	Type:    IndexTypeFocalLength,
	Kind:    bitmapidx.KindContinuous,
	Storage: bitmapidx.StoragePerType,
	Quant:   bitmapidx.Quantizer{Code: bitmapidx.QuantIdentity, Version: 1, Scale: 1},
}
title := bitmapidx.Field{
	Type:     IndexTypeTitle,
	Kind:     bitmapidx.KindText,
	Storage:  bitmapidx.StoragePerType,
	Analyzer: bitmapidx.CodecSpec{Code: bitmapidx.AnalyzerLatin, Version: bitmapidx.AnalyzerVersion},
}

registry := bitmapidx.NewRegistry()
if err := registry.Register(tag, focal, title); err != nil {
	log.Fatal(err)
}
```

Create or reopen the persistent field store with `NewPersistentFields` and
`OpenPersistentFields`. The store owns the field handles and their WAL; callers
must close the store rather than individual fields:

```go
store, err := bitmapidx.NewPersistentFields(
	"./data/fields", []bitmapidx.Field{tag, focal, title},
	bitmapidx.FieldConfig{}, imageidx.DurabilityOS,
)
if err != nil {
	log.Fatal(err)
}
defer store.Close()

tags, _ := store.Lookup(bitmapidx.IndexTypeTag)
if err := tags.Add(bitmapidx.Entry{
	ImageID: 100, Value: bitmapidx.StringValue("Trekking"),
}); err != nil {
	log.Fatal(err)
}
if err := store.Checkpoint(); err != nil {
	log.Fatal(err)
}
```

Use `StoragePerValue` for low-cardinality fields and `StoragePerType` for
high-cardinality fields; both mutate and query the same way. Enum and bool
membership:

```go
tags, err := bitmapidx.NewFieldIndex(tag, bitmapidx.FieldConfig{})
if err != nil {
	log.Fatal(err)
}
defer tags.Close()

if err = tags.Add(
	bitmapidx.Entry{ImageID: 100, Value: bitmapidx.StringValue("Trekking")},
	bitmapidx.Entry{ImageID: 200, Value: bitmapidx.StringValue("Trekking")},
	bitmapidx.Entry{ImageID: 300, Value: bitmapidx.StringValue("Travel")},
); err != nil {
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
if err = tags.Apply(bitmapidx.FieldMutation{
	Add:    []bitmapidx.Entry{{ImageID: 400, Value: bitmapidx.StringValue("Trekking")}},
	Remove: []bitmapidx.Entry{{ImageID: 400, Value: bitmapidx.StringValue("Trekking")}},
}); err != nil {
	log.Fatal(err)
}
```

Continuous values use explicit transitions, which resolve the zero-value
ambiguity (`To: 0` alone would otherwise be indistinguishable from "absent"):

```go
focals, err := bitmapidx.NewFieldIndex(focal, bitmapidx.FieldConfig{})
if err != nil {
	log.Fatal(err)
}
defer focals.Close()

if err = focals.Move(
	bitmapidx.Move{ImageID: 100, To: 35, HasTo: true},                                   // insert
	bitmapidx.Move{ImageID: 100, From: 35, To: 50, HasFrom: true, HasTo: true},          // move
); err != nil {
	log.Fatal(err)
}
if err = focals.Move(bitmapidx.Move{ImageID: 100, From: 50, HasFrom: true}); err != nil { // remove
	log.Fatal(err)
}

// Between is [lo, hi); AtLeast and AtMost are open-ended. Bounds are int64 in
// the field's stored grid domain.
match, err = focals.Range(bitmapidx.Between(24, 70))
match, err = focals.Range(bitmapidx.AtLeast(200))
estimate, err := focals.EstimateRange(bitmapidx.Between(24, 70)) // planning hint
```

Geo fields store one level-16 S2 cell per image. Keys come from coordinates or
cells, and queries take inclusive key ranges produced by `RangesFromCellIDs` or
`CoverRegion`:

```go
geoField := bitmapidx.Field{
	Type:    bitmapidx.IndexTypeLocation,
	Kind:    bitmapidx.KindGeo,
	Storage: bitmapidx.StoragePerType,
	Geo:     bitmapidx.GeoCodec{Version: 1},
}
locations, err := bitmapidx.NewFieldIndex(geoField, bitmapidx.FieldConfig{})
if err != nil {
	log.Fatal(err)
}
defer locations.Close()

key, err := bitmapidx.KeyFromLatLng(37.7749, -122.4194)
if err != nil {
	log.Fatal(err)
}
// One cell per image: insert, move and remove with explicit transitions.
if err = locations.Move(bitmapidx.Move{ImageID: 100, To: int64(key)}); err != nil {
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

Text fields are replaced whole, so callers supply the old and new field text
and the index diffs token sets. A token present in both before and after is
retained:

```go
titles, err := bitmapidx.NewFieldIndex(title, bitmapidx.FieldConfig{})
if err != nil {
	log.Fatal(err)
}
defer titles.Close()

if err = titles.ReplaceText(
	bitmapidx.TextReplacement{ImageID: 100, After: []string{"Golden Gate Bridge"}},
	bitmapidx.TextReplacement{ImageID: 200, After: []string{"Golden Gate Park"}},
	bitmapidx.TextReplacement{ImageID: 100, Before: []string{"Golden Gate Bridge"}, After: []string{"Golden Gate Park"}},
); err != nil {
	log.Fatal(err)
}

match, err = titles.MatchText("golden park") // AND of analyzed tokens
```

Dictionary inspection and maintenance:

```go
key, ok, err := tags.KeyForValue(bitmapidx.StringValue("Trekking"))
value, ok := tags.LookupValue(nil, key) // appends canonical bytes to the buffer

tags.Iterate(func(key bitmapidx.FieldKey, value []byte) bool {
	fmt.Println(key, string(value))
	return true // return false to stop
})
tags.Prefix([]byte("Tr"), func(key bitmapidx.FieldKey, value []byte) bool {
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

`OpenPersistentFields` restores the persisted registry, mmap snapshot and WAL
tail. `Checkpoint` publishes a new snapshot; `Vacuum` also removes obsolete
generations and dead dictionary values. Field query results are immutable
`roar.Bitmap` values. Mutable bitmaps are reserved for transient `search`
workspace accumulation.

## Geographic index

Geo is a `bitmapidx` field kind. Each image holds one level-16 S2 bucket key;
higher-level areas are searched by expanding cells into lexicographic key
ranges, so any S2 level up to 16 maps onto the same bucket table.
`search.WithinCells` takes the field directly, and the same queries are
available without the search package.

```go
locations, err := bitmapidx.NewGeoField(bitmapidx.IndexTypeLocation, []bitmapidx.GeoEntry{
	{ImageID: 100, Latitude: 37.7749, Longitude: -122.4194},
}, bitmapidx.FieldConfig{})
if err != nil {
	log.Fatal(err)
}
defer locations.Close()

// One cell per image: insert, move and remove with explicit transitions.
losAngeles, err := bitmapidx.KeyFromLatLng(34.0522, -118.2437)
if err != nil {
	log.Fatal(err)
}
if err := locations.Move(bitmapidx.Move{ImageID: 200, To: int64(losAngeles)}); err != nil {
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
moves, `From: key` removes); continuous fields keep the flags because zero is a
valid grid value. `GeoValue` carries a bucket key for direct `Add`/`Remove`.
Stale `From` values and duplicate inserts into the same bucket are rejected
before publication, and no reverse image-to-cell map is retained, so a
cross-cell insert without `From` is caller discipline.

`CoverRegion` converts an `s2.Region` to ranges. Supply both a reusable
`GeoCoverWorkspace` and a destination with sufficient capacity for the
allocation-free path; `Nearest` returns exact top-K using a caller-supplied
coordinate lookup. Allocation behavior inside the caller-supplied region's
`IntersectsCell` and `ContainsCell` methods remains the responsibility of that
region implementation. Geo fields use the same persistent `FieldStore` as the
other field kinds.

## Vector index

The `vectoridx` package provides persistent HNSW approximate-nearest-neighbor
search over fixed-dimension embeddings (`512`, `768`, or `1024`) in one of
five element types (`int8`, `uint8`, `int16`, `uint16`, `float32`), ranked by
squared L2 distance. `NodeID` is the application identifier defined by the
index `TypeID`, so an image-embedding index addresses `ImageID`s directly
with no second mapping. See `vectoridx/README.md` and `AGENTS/VectorIdx.MD`.

```go
idx, err := vectoridx.Create(path, vectoridx.Config{
    TypeID:      1,
    Dimensions:  vectoridx.Dim512,
    ElementType: vectoridx.ElementFloat32,
})
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
returned) and `Compact` rebuilds the graph from live vectors without
renumbering NodeIDs. `CandidateFilter` applies bitmap-style membership after
ANN discovery with oversampling and an exact-scan fallback:

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

The `search` package compiles and executes clauses across field, timestamp and
geographic indexes. Every clause is combined with AND semantics:

- all field clauses must contain the image;
- every timestamp range must contain the image (lossy-only ranges on the same
  index coalesce by intersection; each `ExactTimestamp` range is separate);
- every geographic range set must contain the image.

The compiler orders these stages using live cardinality estimates. This only
changes execution order; it does not change query results. Results are returned
as sorted, unique `search.ImageID` values. All index packages alias the
canonical root `imageidx.ImageID` type. There is no OR, no result ordering or
reranking, and no `vectoridx` involvement: `SearchLimit` truncates the sorted
prefix.

```go
geoClause, err := search.WithinCells(geo, cellIDs...)
if err != nil {
	log.Fatal(err)
}

plan, err := search.Compile(
	search.FieldValues(tags, bitmapidx.StringValue("Trekking")),
	search.FieldValues(favorites, bitmapidx.StringValue("favorite")),
	search.During(
		timeIndex,
		start,
		end,
		search.ExactTimestamp(lookupCanonicalTimestamp),
	),
	geoClause,
)
if err != nil {
	log.Fatal(err)
}

// Plans and workspaces can be reused. Search reads the indexes' current
// published views; the estimates captured by Plan affect ordering only.
workspace := search.NewWorkspace(4_096)
matches := make([]search.ImageID, 0, 256)
matches = plan.Search(matches, workspace)
```

For a one-off query, pass clauses directly to `search.Search`. It compiles and
executes them immediately:

```go
matches, err := search.Search(nil,
	search.FieldValues(tags, bitmapidx.StringValue("Trekking")),
	search.During(timeIndex, start, end),
	geoClause,
)
```

Reusing a `Plan`, caller-owned result slice, and `Workspace` avoids rebuilding
planning metadata and retains intermediate buffers. With sufficient capacities,
the combined search path performs zero heap allocations.

A `Plan` exposes several execution shapes over the same compiled stages:

```go
// Count matches without retaining IDs.
count := plan.Count(workspace)

// Stop after N sorted matches (truncation, not ranking).
top := plan.SearchLimit(nil, workspace, 100)

// Stream sorted, unique matches without building a result slice.
for id := range plan.Iterate(workspace) {
	fmt.Println(id)
}

// Cancellation is checked between stages.
matches, err = plan.SearchContext(ctx, matches[:0], workspace)
```

For long-lived services, `search.NewService` shares one plan and pools
workspaces plus timestamp write batches across concurrent agents:

```go
service := search.NewService(plan, 4_096, 256)
service.Prewarm(4)

matches := service.Query(nil)
matches, err = service.QueryContext(ctx, matches[:0])
count = service.Count()

batch := service.AcquireBatch() // sized for the plan's timestamp writer
// fill batch with timeidx mutations, then apply through
// the timestamp index; return it so another agent can reuse its buffers.
service.ReleaseBatch(batch)
```

Clauses are conjunctive: `search.FieldValues`, `search.FieldText` and
`search.FieldRange` query typed fields; `search.During` handles a half-open
timestamp range (optionally with `search.ExactTimestamp`); and
`search.WithinCells` queries a `KindGeo` field. `WithinCells` is the only clause
constructor that returns an error. `vectoridx` is not a clause source.

Timestamp ranges use the timestamp index's compressed candidate semantics.
Add `search.ExactTimestamp` when exact boundary filtering is required; only
exact timestamp stages can filter candidates in place. An empty range
(`start >= end`) compiles to a valid empty plan. Geographic search options
are S2 cell IDs, not latitude/longitude values: `search.WithinCells` accepts
one or more valid cells and normalizes them to level-16 descendant ranges
(coarser cells expand, finer cells snap to their ancestor). Exact polygon or
coordinate verification belongs in the caller's canonical-data layer before or
after the candidate search.

A combined search does not create a database-wide transactional snapshot. Each
stage reads one independently published immutable view, consistent with the
standalone index APIs. `search.Compile` and `search.Search` return
`search.ErrEmptyQuery` when no clauses are supplied and `search.ErrInvalidClause`
for a missing index.

Reader lifetime tracking is automatic. Every field read registers with its
internal tracker, and `FieldStore.Close` waits for reads that could still
reference retired or mmap-backed state. Callers open one persistent
`FieldStore`, obtain fields with `Lookup`, and close the store after use.

## Runnable command

`cmd/imageidx` constructs a small compatible field, timestamp, and geographic
index set. Its default one-shot mode executes `search.Search` once and prints a
JSON result:

```sh
go run ./cmd/imageidx
# {"image_ids":[100,200]}
```

Focused, copy-pasteable programs live under `cmd/imageidx/examples`:

```sh
go run ./cmd/imageidx/examples/basic      # bitmap + timestamp + geo plan
go run ./cmd/imageidx/examples/bitmaponly # Search, Count, SearchLimit, Iterate
go run ./cmd/imageidx/examples/service    # shared Service plus batch reuse
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
go test ./timeidx -run '^$' -bench 'Benchmark(SearchParallel|UpdateParallel)$' -benchmem
go test ./bitmapidx -run '^$' -bench BenchmarkField -benchmem
go test ./search -run '^$' -bench BenchmarkCombinedSearch -benchmem
```

Set `TIMESTAMP_BENCH_LARGE=1` to enable the synthetic 10M and 100M-entry
benchmark builds.
