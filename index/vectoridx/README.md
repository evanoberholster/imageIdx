# vectoridx

Persistent HNSW approximate-nearest-neighbor vector index implementing
`AGENTS/VectorIdx.MD` phases 1–2 (scalar core, in-memory HNSW correctness),
4 (WAL and tombstones), 5 (pooled search state), 6 (post-ANN filtering), and
8 (explicit compact/rebuild). SIMD kernels (phase 7) plug into
`resolveDistance`; the scalar kernels here are their correctness reference.

## Concepts

- **Direct identity.** `NodeID` (`uint32`) is the application identifier
  defined by the index `TypeID` (ImageID, FaceID, …). VectorIdx never
  translates it. HNSW edges store NodeIDs, never pointers or ordinals.
- **Dense physical data.** Vectors pack contiguously in insertion-ordinal
  order even when NodeIDs are sparse. The sorted `NodeID → ordinal`
  locator (binary search) resolves edges to vectors; scratch visited tags
  are indexed by ordinal, so search memory scales with node count, never
  `max(NodeID)`.
- **One writer, shared readers.** `Add`/`Delete`/`Checkpoint`/`Compact`
  serialize through a writer mutex; searches hold a read lock plus a
  `ReaderTracker` token. File generations are immutable once published.
- **Tombstone deletion.** `Delete` only records a tombstone. Deleted nodes
  stay traversable but are never returned; `Compact` rebuilds the graph
  from live vectors and never renumbers NodeIDs.
- **Post-ANN filtering.** A `CandidateFilter` applies after graph traversal
  and never constrains it. Filtered search oversamples (`4×k`) and falls
  back to the exact scan when ANN coverage is thin, giving
  k-or-available semantics.

## Usage

```go
idx, err := vectoridx.Create(path, vectoridx.Config{
    TypeID:      1, // your image-embedding space
    Dimensions:  vectoridx.Dim512,
    ElementType: vectoridx.ElementFloat32,
})
if err != nil {
    log.Fatal(err)
}
defer idx.Close()

if err := idx.AddFloat32(7, vec); err != nil {
    log.Fatal(err)
}
results, err := idx.SearchFloat32(query, nil, 10)
if err != nil {
    log.Fatal(err)
}
// results are sorted by ascending Score (monotonic with squared L2).
```

Reuse caller storage on hot paths:

```go
dst, err := idx.SearchIntoFloat32(dst[:0], query, filter, 10)
```

Filter with any bitmap representation:

```go
filter := vectoridx.CandidateFilter(func(id vectoridx.NodeID) bool {
    return bitmap.Contains(imageidx.ImageID(id))
})
```

Typed `Add`/`Search`/`SearchInto` variants exist for all five element
types (`Int8`, `Uint8`, `Int16`, `Uint16`, `Float32`). There are no
`any`-typed hot paths: dispatch happens once per call, distance dispatch
once per index open.

In-memory indexes (tests, caller-managed durability):

```go
idx, err := vectoridx.New(vectoridx.Config{...})
```

## Persistence

One logical file per vector space plus a `<path>.wal` sidecar:

- 256-byte little-endian superblock (magic `VCTRIDX1`, version, config,
  entry node, counts, checkpoint LSN, CRC32).
- 4-entry region directory (node metadata, vectors, graph, tombstones)
  with offsets, lengths, and CRC32 checksums.
- Framed WAL (`AddVector`/`DeleteNode` with LSN and CRC32). Recovery
  replays records after `CheckpointLSN`, stops at the first torn tail,
  truncates it, and is idempotent across restarts.
- `Checkpoint` writes the file atomically (temp + rename + dir sync) and
  truncates the covered WAL tail. `Compact` rebuilds the graph from live
  vectors in NodeID order, purges tombstones, and checkpoints.
- `Verify` deep-checks in-memory structure; `VerifyFile` validates a file
  offline (header, bounds, checksums).

Crash model: acknowledged mutations are WAL-synced before return;
checkpoint publication is atomic; readers never see a partial generation.

## API rules worth knowing

- Dimensions must be 512, 768, or 1024; element type is fixed for the
  index lifetime. Mismatched vectors/queries return `ErrInvalidVector`.
- Duplicate `Add` of any known NodeID (live **or** tombstoned) returns
  `ErrDuplicateNodeID`. Re-adding after `Delete` requires `Compact` first,
  keeping replacement semantics explicit in V1.
- `Delete` of an unknown ID returns `ErrNodeNotFound`; re-deleting a
  tombstoned ID is a no-op.
- `Search` with `k <= 0` returns `ErrInvalidQuery`. Fewer than `k` live
  (or filter-passing) nodes returns what's available, not an error.
- `Score` is `uint32` saturating: monotonic with squared L2, ties at
  `math.MaxUint32` only for extremely distant pairs / NaN.

## Concurrency and allocation

- One writer; concurrent readers share a read lock. `Close` waits for
  prior readers. Run with `-race` in CI (covered by tests).
- Steady state after pool warm-up: `SearchInto` performs **zero heap
  allocations** (`TestSearchSteadyStateAllocs`); `Search` allocates once
  for the caller-owned copy; distance kernels allocate nothing.
- `PrewarmSearchContexts(n)` primes the pool before serving.

## Benchmarks

```sh
go test ./vectoridx -run '^$' -bench 'BenchmarkDistance' -benchmem
go test ./vectoridx -run '^$' -bench 'Benchmark(Search|Exact)' -benchmem
go test ./vectoridx -run TestHNSWRecall -v -count=1
```

Distance microbenchmarks cover every element type × dimension with
`ns/op`, `bytes/s`, and `allocs/op`. `BenchmarkSearchFloat32` /
`BenchmarkSearchIntoFloat32Reuse` measure end-to-end ANN latency and
allocations against `BenchmarkExactSearchFloat32`; recall tests pin
ANN quality (`recall@5 ≥ 0.70` on the 300-vector fixture) so speedups
that degrade the graph fail loudly.

## File map

| File | Contents |
|---|---|
| `doc.go` | Package overview, V1 scope and simplifications |
| `types.go` | NodeID/TypeID/ElementType/Config/Result/CandidateFilter/errors |
| `distance.go` | Scalar squared-L2 kernels, dispatch, `EncodeScore` |
| `searchctx.go` | Pooled scratch: heaps, generation-tag visited set, buffers |
| `hnsw.go` | Locator, greedy descent, layer search, assembly, exact oracle |
| `index.go` | VectorIdx: typed Add/Search, Delete, Checkpoint/Compact/Verify |
| `format.go` | Single-file superblock + region encode/decode, `VerifyFile` |
| `wal.go` | Framed WAL append and prefix recovery |

## Deliberate V1 simplifications (future work)

- Readers take a shared mutex rather than fully lock-free mmap epochs;
  generations on disk are already immutable, so the swap is API-safe.
- Heap-backed (copied) reads instead of retained mmaps; the format is
  offset/length-addressed for a compatible mmap upgrade.
- Scalar distance only; `resolveDistance` is the AVX2/NEON plug point.
- Closest-only neighbor selection, no diversity heuristic.
- Sorted-locator binary search; dense/sparse locator alternatives await
  end-to-end benchmarks per the design doc.
