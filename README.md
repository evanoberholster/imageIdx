# imageIdx

Image indexing tools written in Go 1.27.

## Timestamp index

The `timestamp` package provides a scalar, block-local timestamp index designed
for large image collections. Its current implementation includes:

- immutable, sorted structure-of-arrays base blocks;
- fixed-capacity active and frozen deltas with a hard three-layer read bound;
- lock-free, zero-allocation range searches with caller-owned result buffers;
- a single-owner batched writer and background copy-on-write compaction;
- epoch-based reclamation for reader-safe delta reuse;
- a checksummed write-ahead log with optional sync-on-write durability;
- page-aligned sequential snapshots, atomic manifests, mmap reopen, and vacuum;
- scalar search kernels only; SIMD is reserved for a later implementation.

```go
idx, err := timestamp.New(records, offset, timestamp.Config{})
if err != nil {
	log.Fatal(err)
}
defer idx.Close()

results := make([]timestamp.ImageID, 0, 256)
results = idx.Search(results, start, end)
```

`Search` returns compressed-index candidates. Use `SearchExact` with the
canonical timestamp column when exact low-bit boundary semantics are required.
Reuse a `TimestampWriteBatch` to keep normal updates allocation-free.

## Development

```sh
go run .
go test ./...
go test -race ./timestamp
go test ./timestamp -run '^$' -bench 'Benchmark(SearchParallel|UpdateParallel)$' -benchmem
```

Set `TIMESTAMP_BENCH_LARGE=1` to enable the synthetic 10M and 100M-entry
benchmark builds.

## Bitmap index

The `bitmapidx` package implements the immutable snapshot design described in
`AGENTS/BitmapIdxProposal.MD`:

- database-wide `uint64` image IDs and fixed-width predicate keys;
- lock-free reads through one atomic immutable view per bitmap;
- atomic batch additions/removals with Base + Frozen + Active bounds;
- background copy-on-write compaction and query-wide epoch reclamation;
- Base-cardinality AND planning and delta-correct AND/OR execution;
- lock-free immutable key/ID directories;
- page-aligned packed sroar segments with checksums and mmap reopen;
- a global checksummed batch WAL, atomic manifests, checkpoints, and vacuum.

```go
epoch := bitmapidx.NewEpoch()
paris, err := bitmapidx.New(
	bitmapidx.BitmapKey{Type: bitmapidx.IndexTypeLocation, ValueID: 10},
	1,
	[]uint64{100, 200},
	bitmapidx.Config{Epoch: epoch},
)
if err != nil {
	log.Fatal(err)
}
defer paris.Close()

if err := paris.Add([]uint64{300, 400}); err != nil {
	log.Fatal(err)
}
matches := paris.Search(nil)
```

## Geographic index

The `geoindex` package stores one coordinate per image in fixed S2 level-16
cells. Cells with up to four images use a fixed-width sparse SoA layout; denser
cells reuse immutable `bitmapidx.BitmapRef` values. Searches load one atomic
generation and never inspect writer-side mutations.

```go
geo, err := geoindex.New(1, []geoindex.GeoEntry{
	{ImageID: 100, Latitude: 37.7749, Longitude: -122.4194},
}, geoindex.Config{})
if err != nil {
	log.Fatal(err)
}
defer geo.Close()

// Small writes become visible at the configured threshold or timer. Flush is
// available when the caller needs an explicit visibility boundary.
if err := geo.AddBatch([]geoindex.GeoEntry{
	{ImageID: 200, Latitude: 34.0522, Longitude: -118.2437},
}); err != nil {
	log.Fatal(err)
}
if err := geo.Flush(); err != nil {
	log.Fatal(err)
}

candidates := geo.SearchBounds(nil, 37.7, -122.5, 37.9, -122.3, nil)
```

Geo persistence uses checksummed, mmap-oriented generation files, an atomic
manifest, and a global batch WAL through `geoindex.NewPersistent` and
`geoindex.OpenPersistent`.
