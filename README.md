# imageIdx

Image indexing tools written in Go 1.27. The root `imageidx` package defines
the shared `ImageID` and durability types. The `roar` package owns the
immutable zero-copy bitmap representation, used directly by the logical
indexes in `bitmapidx`, `timeidx`, `geoidx`, and `vectoridx`. The `search` package compiles
and executes queries across them.

- `bitmapidx` provides persistent caller-keyed predicate sets and an in-memory
  unified field index for enum, bool, text and continuous values.
- `timeidx` indexes timestamps for range search.
- `geoidx` indexes coordinates through fixed-level S2 cells.
- `vectoridx` indexes fixed-dimension embeddings with persistent HNSW ANN search.
- `search` combines bitmap, timestamp and geographic clauses with AND
  semantics.

Pre-v1 note: `timestamp` and `geoindex` were renamed to `timeidx` and
`geoidx`. Import paths changed; persisted formats are unchanged.

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

## Bitmap index (predicate sets)

The `bitmapidx` package implements the immutable snapshot design documented in
`AGENTS/BitmapIndex.md`:

- database-wide `uint64` image IDs and fixed-width predicate keys;
- lock-free reads through one atomic immutable view per bitmap;
- atomic batch additions/removals with Base + Frozen + Active bounds;
- background copy-on-write compaction and internal reader-safe reclamation;
- Base-cardinality AND planning and delta-correct AND/OR execution;
- lock-free immutable key/ID directories;
- page-aligned packed sroar segments with checksums and mmap reopen;
- a global checksummed batch WAL, atomic manifests, checkpoints, and vacuum.

```go
paris, err := bitmapidx.New(
	bitmapidx.Spec{
		Key: bitmapidx.Key{Type: bitmapidx.IndexTypeLocation, ValueID: 10},
		IndexID: 1,
		IDs: []imageidx.ImageID{100, 200},
	},
	bitmapidx.Config{},
)
if err != nil {
	log.Fatal(err)
}
defer paris.Close()

if err := paris.Apply(bitmapidx.Mutation{
	Add: []imageidx.ImageID{300, 400},
}); err != nil {
	log.Fatal(err)
}
matches := paris.AppendTo(nil)
```

For persistence, one `Store` owns a directory of predicates, a packed
checksummed segment generation, and a global batch WAL. `Apply` is atomic per
batch, `Checkpoint` publishes a new mmap generation, and `Vacuum` reclaims
obsolete generations:

```go
store, err := bitmapidx.NewPersistent(
	"./data/bitmaps",
	[]bitmapidx.Spec{{
		Key:     bitmapidx.Key{Type: bitmapidx.IndexTypeTag, ValueID: 10},
		IndexID: 1,
		IDs:     []imageidx.ImageID{100, 200},
	}},
	bitmapidx.Config{},
	imageidx.DurabilityOS,
)
if err != nil {
	log.Fatal(err)
}
defer store.Close()

key := bitmapidx.Key{Type: bitmapidx.IndexTypeTag, ValueID: 10}
if err := store.Apply(key, bitmapidx.Mutation{Add: []imageidx.ImageID{300}}); err != nil {
	log.Fatal(err)
}
if err := store.Checkpoint(); err != nil {
	log.Fatal(err)
}
if err := store.Vacuum(); err != nil {
	log.Fatal(err)
}
```

Reopen with `bitmapidx.OpenPersistent`, which maps the published generation and
replays the WAL tail.

## Field index (enum, bool, text, continuous)

`bitmapidx` also provides an in-memory unified field index for metadata that
does not fit a caller-assigned numeric predicate ID. One `FieldIndex` holds one
field and supports four value shapes:

| Kind | Values | Query |
|---|---|---|
| `KindEnum` | discrete strings/bytes (camera model, keyword) | `MatchValues` |
| `KindBool` | explicit false/true; missing is neither | `MatchValues` |
| `KindText` | tokenized documents (title, caption) | `MatchText` |
| `KindContinuous` | `int64` grid values (ISO, focal length) | `Range` |

Values are identified by a 64-bit xxh3 hash. Rare hash collisions claim a
dictionary slot, so distinct values never share membership. The built-in Latin
analyzer lowercases, accent-folds and tokenizes text.

Declare fields as values and optionally validate or persist the definitions in
a `Registry`:

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

Field indexes are in-memory only; persistence for the v3 field model is not
implemented yet. They are not yet accepted as `search` clauses.

## Geographic index

The `geoidx` package stores one coordinate per image in fixed S2 level-16
cells. Cells with up to four images use a fixed-width sparse SoA layout; denser
cells reuse immutable `roar.Bitmap` values. The read representation is split into
private immutable shards, so a publication rebuilds only shards affected by a
batch. Searches load one atomic generation and never inspect writer mutations.

```go
geo, err := geoidx.New(geoidx.Spec{
	IndexID: 1,
	Entries: []geoidx.Entry{
		{ImageID: 100, Latitude: 37.7749, Longitude: -122.4194},
	},
}, geoidx.Config{})
if err != nil {
	log.Fatal(err)
}
defer geo.Close()

// Small writes become visible at the configured threshold or timer. Flush is
// available when the caller needs an explicit visibility boundary.
losAngeles, err := geoidx.KeyFromLatLng(34.0522, -118.2437)
if err != nil {
	log.Fatal(err)
}
if err := geo.Apply([]geoidx.Mutation{
	{ImageID: 200, To: losAngeles}, // From zero means the image was absent.
}); err != nil {
	log.Fatal(err)
}
if err := geo.Flush(); err != nil {
	log.Fatal(err)
}

cellIDs := []uint64{
	uint64(s2.CellIDFromLatLng(s2.LatLngFromDegrees(37.7749, -122.4194)).Parent(geoidx.GeoS2Level)),
}
ranges, err := geoidx.RangesFromCellIDs(nil, cellIDs)
if err != nil {
	log.Fatal(err)
}
candidates := geo.SearchRanges(nil, ranges)
```

Moves and removals are explicit: use `{ImageID: id, From: old, To: next}` to
move an image and `{ImageID: id, From: old}` to remove it. `S2Key(0)` is the
absent sentinel. This keeps the index from retaining a database-sized reverse
map from image ID to cell.

`CoverRegion` remains available when a caller needs to convert an `s2.Region`
to ranges. Supply both a reusable `CoverWorkspace` and a destination with
sufficient capacity for the allocation-free path. Allocation behavior inside
the caller-supplied region's `IntersectsCell` and `ContainsCell` methods remains
the responsibility of that region implementation.

Geo persistence uses checksummed, mmap-oriented generation files, an atomic
manifest, and a global batch WAL:

```go
persistent, err := geoidx.NewPersistent(
	"./data/geo",
	geoidx.Spec{
		IndexID: 1,
		Entries: []geoidx.Entry{
			{ImageID: 100, Latitude: 37.7749, Longitude: -122.4194},
		},
	},
	geoidx.Config{},
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

`geoidx.OpenPersistent` maps the published generation and replays the WAL tail.

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

The `search` package compiles and executes clauses across all three index
types. Every clause is combined with AND semantics:

- all bitmap indexes must contain the image;
- every timestamp range must contain the image;
- every geographic range set must contain the image.

The compiler orders these stages using live cardinality estimates. This only
changes execution order; it does not change query results. Results are returned
as sorted, unique `search.ImageID` values. All index packages alias the
canonical root `imageidx.ImageID` type.

```go
geoClause, err := search.WithinCells(geo, cellIDs...)
if err != nil {
	log.Fatal(err)
}

plan, err := search.Compile(
	search.AllBitmaps(paris, favorites),
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
	search.AllBitmaps(paris, favorites),
	search.During(timeIndex, start, end),
	geoClause,
)
```

Reusing a `Plan`, caller-owned result slice, and `Workspace` avoids rebuilding
planning metadata and retains intermediate buffers. With sufficient capacities,
the combined search path performs zero heap allocations.

A `Plan` exposes several execution shapes over the same compiled stages:

```go
// Count matches without materializing IDs.
count := plan.Count(workspace)

// Stop after N sorted matches.
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
// fill batch with timeidx.Insert/Delete/Move mutations, then apply through
// the timestamp index; return it so another agent can reuse its buffers.
service.ReleaseBatch(batch)
```

Clauses are additive: `search.AllBitmaps` takes one or more `bitmapidx`
predicate indexes, `search.During` a timestamp range (optionally with
`search.ExactTimestamp`), and `search.WithinCells` one or more S2 cell IDs. The
`bitmapidx` field index (enum, bool, text, continuous) is not yet a clause
source; query it directly and combine results in the caller.

Timestamp ranges use the timestamp index's compressed candidate semantics.
Add `search.ExactTimestamp` when exact boundary filtering is required.
Geographic search options are S2 cell IDs, not latitude/longitude values:
`search.WithinCells` accepts one or more valid cells at levels 0 through 16 and
normalizes them to level-16 descendant ranges. Exact polygon or coordinate
verification belongs in the caller's canonical-data layer before or after the
candidate search.

A combined search does not create a database-wide transactional snapshot. Each
stage reads one independently published immutable view, consistent with the
standalone index APIs. `search.Compile` and `search.Search` return
`search.ErrEmptyQuery` when no clauses are supplied and `search.ErrInvalidClause`
for a missing index.

Reader lifetime tracking is automatic. Every index read registers with its
own internal tracker, and `Close` waits for reads that could still reference
retired or mmap-backed state. Callers create indexes with `New`, call search
methods normally, and only need to `defer index.Close()`; no per-query guard is
part of the public API. Bitmap indexes opened from one persistent `Store` share
one private tracker, so a multi-bitmap search registers once for that store.

## Runnable command

`cmd/imageidx` constructs a small compatible bitmap, timestamp, and geographic
index set. Its default one-shot mode executes `search.Search` once and prints a
JSON result:

```sh
go run ./cmd/imageidx
# {"image_ids":[100,200]}
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
