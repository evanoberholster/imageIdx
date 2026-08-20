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
