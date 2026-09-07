# imageIdx

Image indexing tools written in Go 1.27. The root `imageidx` package defines
the shared `ImageID` and durability types. The `idset` package owns immutable
roaring representation, while logical indexes live in the
`bitmapidx`, `timeidx`, and `geoidx` subpackages. The `roar` package is a
zero-copy immutable bitmap index documented in `AGENTS/RoarFormat.md`.

Pre-v1 note: `timestamp` and `geoindex` were renamed to `timeidx` and
`geoidx`. Import paths changed; persisted formats are unchanged.

## Roar bitmap index

The `roar` package stores the serialized bitmap as its read representation:
sorted high-48-bit keys with byte offsets, sorted `uint16` array containers
for sparse ranges, and 1,024-`uint64` bitmap containers for dense ranges.
Reads operate directly on mmap-friendly bytes without decoding objects.

```go
bitmap, err := roar.FromSortedList([]uint64{1<<48 + 1, 1<<48 + 2, 2<<48 + 9})
if err != nil {
	log.Fatal(err)
}

bitmap.Contains(1<<48 + 2)                // true, zero allocation
n := roar.AndCardinality(bitmap, other)   // fused count, no result bitmap

results := make([]uint64, 0, 256)
results = bitmap.AppendTo(results)        // fast ascending materialization
```

`AndInto`/`OrInto`/`AndNotInto` write into a reusable `Builder`, and
`AndManyCardinality` counts N-way intersections without intermediate
bitmaps. `FromSorted` returns the encoded representation directly when a
queryable `Bitmap` is not needed. `Open` validates stored or mmap-backed bytes;
`OpenTrusted` skips validation for already-verified bytes. The serialized
format is 8-byte aligned throughout. Benchmarks in
`roar/benchmark_test.go` compare against `weaviate/sroar` using
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

## Development

```sh
go run ./cmd/imageidx
go run ./cmd/imageidx -mode server -addr :8080
go test ./...
go test -race ./...
go test ./timeidx -run '^$' -bench 'Benchmark(SearchParallel|UpdateParallel)$' -benchmem
go test ./search -run '^$' -bench BenchmarkCombinedSearch -benchmem
```

Set `TIMESTAMP_BENCH_LARGE=1` to enable the synthetic 10M and 100M-entry
benchmark builds.

## Bitmap index

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

## Geographic index

The `geoidx` package stores one coordinate per image in fixed S2 level-16
cells. Cells with up to four images use a fixed-width sparse SoA layout; denser
cells reuse immutable `idset.Ref` values. The read representation is split into
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
manifest, and a global batch WAL through `geoidx.NewPersistent` and
`geoidx.OpenPersistent`.

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
