# vectoridx (experimental)

> **Experimental in 0.5.** Keep existing vector functionality working, but do not
> treat the public API, performance, or on-disk format as a 0.5 contract.
> Applications relying on the documented public surface should treat vector search as an optional experimental
> feature.

Persistent HNSW approximate-nearest-neighbor vector index implementing
experimental phases 1–2 (scalar core, in-memory HNSW correctness),
4 (WAL and tombstones), 5 (pooled search state), 6 (post-ANN filtering), and
8 (explicit compact/rebuild), plus Weaviate-inspired production features:
multi-metric distance (L2/cosine/dot), dynamic `ef`, heuristic neighbor
selection, dense O(1) locator, cost-based flat fallback, async writes, and
lock-free reads. SIMD kernels (phase 7) plug into `resolveDistance`; the
scalar kernels here are their correctness reference.

## Concepts

- **Direct identity.** `Namespace` (`schema.Namespace`, registered as
  `schema.KindVector`) is the shared schema identity of one vector space.
  `NodeID` (`uint32`) is the stored identifier; HNSW edges store NodeIDs,
  never pointers or ordinals. Results widen the stored NodeID to
  `schema.ValueID` (`Result{ValueID, Score}`).
- **Dense physical data.** Vectors pack contiguously in insertion-ordinal
  order even when NodeIDs are sparse. A dense O(1) table serves dense ID
  ranges with atomic fills; the sorted `NodeID → ordinal` locator covers
  sparse ranges. Scratch visited tags are indexed by ordinal, so search
  memory scales with node count, never `max(NodeID)`.
- **One writer, lock-free readers.** `Add`/`Delete`/`Checkpoint`/`Compact`
  serialize through a writer mutex (plus an async queue: `AddAsync`,
  `DeleteAsync`, `CompactAsync`, `WaitIdle`); searches acquire a
  `ReaderTracker` token and load one atomically published snapshot, taking
  no locks. File generations are immutable once published.
- **Metrics.** One fixed metric per index: L2 (squared), cosine (`1-cos`),
  or dot (`querySelfDot-dot`, zero for self). Persisted in the superblock.
  Cosine indexes cache one norm per stored vector and compute the query
  norm once per search, so the hot loop performs a single dot product per
  comparison instead of three passes.
- **Tombstone deletion.** `Delete` records into an immutable roaring
  tombstone bitmap. Deleted nodes stay traversable but are never returned;
  `Compact` rebuilds the graph from live vectors and never renumbers
  NodeIDs. `SetAutoCompact` enables background compaction on a tombstone
  ratio.
- **Search budget.** Static `EfSearch` or dynamic `ef`
  (`clamp(k*DynamicEfFactor, Min, Max)`, Weaviate analogue) with
  `OversampleFactor` scaling under filters and one iterative expansion round
  before the exact-scan fallback.
- **Flat fast path.** Indexes at/below `FlatThreshold` live nodes and filter
  hints at/below `FlatSearchCutoff` run the pooled exact scan directly
  (Weaviate flat/dynamic analogue); `SearchWithFilterHint` takes the hint.
- **Post-ANN filtering.** A `CandidateFilter` applies after graph traversal
  and never constrains it. Filtered search oversamples and falls back to
  the exact scan when ANN coverage is thin, giving k-or-available semantics.

## Usage

```go
idx, err := vectoridx.NewStore(path, vectoridx.Config{
    Namespace:   1, // schema.Namespace registered as schema.KindVector
    Dimensions:  vectoridx.Dim512,
    ElementType: vectoridx.ElementFloat32,
}, vectoridx.DurabilitySync)
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
// results are []Result{ValueID, Score}, sorted by ascending Score
// (monotonic with squared L2); ValueID is the widened NodeID.
```

Reuse caller storage on hot paths:

```go
dst, err := idx.SearchIntoFloat32(dst[:0], query, filter, 10)
```

Filter with any bitmap representation:

```go
filter := vectoridx.CandidateFilter(func(id vectoridx.NodeID) bool {
    imageID, ok := imageForValue(schema.ValueID(id))
    return ok && bitmap.Contains(imageID)
})
```

Typed `Add`/`Search`/`SearchInto` variants exist for all five element
types (`Int8`, `Uint8`, `Int16`, `Uint16`, `Float32`). There are no
`any`-typed hot paths: dispatch happens once per call, distance dispatch
once per index open.

In-memory indexes (tests, caller-managed durability):

```go
idx, err := vectoridx.NewMemory(vectoridx.Config{...})
```

## Persistence

A base generation file plus a `<path>.manifest`, `<path>.delta.<gen>`
generations, and segmented `<path>.wal.<seq>` logs:

- 256-byte little-endian superblock (magic `VCTRIDX1`, version, config,
  entry node, counts, checkpoint LSN, CRC32).
- 4-entry region directory (node metadata, vectors, graph, tombstones)
  with offsets, lengths, and CRC32 checksums.
- `Checkpoint` publishes the mutations since the last checkpoint as one
  immutable delta generation (puts carry level and vector, deletes carry
  the NodeID; superseded mutations are promoted away) and updates the
  manifest atomically. The base file is not rewritten, so checkpoint cost
  scales with changes, not index size.
- `Vacuum` merges the base and every delta into a fresh base, drops
  superseded and tombstone records, removes the delta files, and truncates
  the covered WAL. `Compact` rebuilds the graph from live vectors in NodeID
  order first and then vacuums; it returns a `CompactionResult` (nodes
  before/after, tombstones purged, deltas merged, records reclaimed, bytes
  reclaimed, duration) and a concurrent call returns
  `ErrCompactionInProgress`.
- `SetAutoVacuum(maxDeltas, minFragmentation)` enqueues background vacuum
  when the manifest holds too many generations or tombstone fragmentation
  crosses the threshold; `SetAutoCompact(ratio, minNodes)` keeps the
  tombstone-ratio rebuild policy.
- WAL files rotate at `WithWALSegmentBytes` (64 MiB default); recovery
  replays segments oldest to newest, stops at the newest torn tail and
  truncates it, and a checkpoint removes every non-active segment.
- The index path is exclusively locked (`<path>.lock`) from `NewStore`/`OpenStore`
  until `Close`; a second handle returns `ErrStoreBusy`.
- `Stats` reports generation, counts, checkpoint LSN, WAL counters, delta
  generations/fragmentation/promotion counters, file sizes, pending async
  ops, and the last compaction; `FileSize` returns base plus WAL bytes.
- `Verify` deep-checks in-memory structure; `VerifyFile` validates the base
  file offline (header, bounds, checksums), and a corrupt manifest or delta
  fails OpenStore with `ErrCorruptStorage`.

Crash model: `NewStore`/`OpenStore` select a durability policy; with
`DurabilitySync` acknowledged mutations are WAL-synced before return, with
`DurabilityOS` they are in the OS page cache. `WithSyncInterval(d)` adds
periodic group commit for `DurabilityOS`: fsync happens when the async queue
drains and at least every `d`, so a write batch shares one fsync. `Sync`
forces a durable boundary at any time; `Close` always syncs. A delta file is
durable before the manifest names it and the WAL is truncated only after
both, so an orphan delta from a crash is ignored and reclaimed by the next
vacuum. Checkpoint publication is atomic; readers never see a partial
generation.

## API rules worth knowing

- `Dimensions` is closed {512, 768, 1024} and element type is fixed for the
  index lifetime; an invalid (zero) `Namespace` or `Dimensions` returns
  `ErrInvalidConfig`. Mismatched vectors/queries return `ErrInvalidVector`.
- Duplicate `Add` of a live NodeID returns `ErrDuplicateNodeID`. Adding a
  tombstoned NodeID replaces its vector and reconnects it, so `Delete`
  followed by `Add` is an update; `Compact` purges tombstones and rebuilds.
- `NewStore`/`OpenStore` take a durability policy: `DurabilitySync` fsyncs every
  acknowledged mutation, `DurabilityOS` leaves records to the OS page cache
  (add `vectoridx.WithSyncInterval(d)` for periodic group commit). Invalid
  values return `ErrInvalidConfig`.
- `Delete` of an unknown ID returns `ErrNodeNotFound`; re-deleting a
  tombstoned ID is a no-op.
- `Search` with `k <= 0` returns `ErrInvalidQuery`. Fewer than `k` live
  (or filter-passing) nodes returns what's available, not an error.
- `Score` is `uint32` saturating: monotonic with the index metric distance
  (L2, cosine, or dot), ties at `math.MaxUint32` only for extremely distant
  pairs / NaN.

## Concurrency and allocation

- One writer (sync or async queue); lock-free concurrent readers.
  `Close` drains the queue and waits for prior readers. Run with `-race`
  in CI (covered by tests).
- Steady state after pool warm-up: `SearchInto` performs **zero heap
  allocations** (`TestSearchSteadyStateAllocs`); `Search` allocates once
  for the caller-owned copy; distance kernels allocate nothing.
- Writer-owned scratch buffers (`encodeBuf`, `walBuf`, `pruneScored`,
  `pruneCands`) remove per-add and per-prune allocations; the WAL record and
  the encoded vector are rewritten in place under the writer lock.
- Base publication streams the vector region from the published snapshot
  instead of copying it into a second payload buffer and then into the output
  file. `Compact` likewise copies each live vector once, directly into the new
  snapshot.
- `PrewarmSearchContexts(n)` primes the pool before serving.

## Benchmarks

```sh
go test ./vectoridx -run '^$' -bench 'BenchmarkDistance' -benchmem
go test ./vectoridx -run '^$' -bench 'Benchmark(Search|Exact)' -benchmem
go test ./vectoridx -run TestHNSWRecall -v -count=1
```

Distance microbenchmarks cover every element type × dimension with
`ns/op`, `bytes/s`, and `allocs/op`. `BenchmarkSearchFloat32_512ANN` /
`BenchmarkSearchFloat32_768ANN` and their cosine variants set
`FlatThreshold: -1` so they exercise the graph path; `BenchmarkAddFloat32_512`
/ `BenchmarkAddFloat32_768` cover the writer path. `BenchmarkSearchFloat32` /
`BenchmarkSearchIntoFloat32Reuse` measure the default flat path against
`BenchmarkExactSearchFloat32`; recall tests pin ANN quality
(`recall@5 ≥ 0.70` on the 300-vector fixture) so speedups that degrade the
graph fail loudly.

## File map

| File | Contents |
|---|---|
| `doc.go` | Package overview, experimental scope and current limitations |
| `types.go` | Namespace/Dimensions/NodeID/ElementType/Config/Result/CandidateFilter/errors |
| `distance.go` | Scalar L2/cosine/dot kernels, dispatch, `EncodeScore` |
| `searchctx.go` | Pooled scratch: heaps, generation-tag visited set, buffers |
| `hnsw.go` | Locator, greedy descent, layer search, assembly, exact oracle |
| `index.go` | VectorIdx: typed Add/Search, Delete, Checkpoint/Compact/Verify, async queue, policies |
| `options.go` | Runtime `Option`s (WithSyncInterval) |
| `stats.go` | `Stats`/`FileSize`/`CompactionResult` |
| `format.go` | Base superblock + region encode/decode, `VerifyFile` |
| `manifest.go` | Manifest and delta generations, Checkpoint/Vacuum, promotion |
| `wal.go` | Segmented framed WAL: rotation, append, prefix recovery |

## Current experimental limitations (future work)

- Readers are lock-free over heap snapshots (tracker + atomic view), not
  yet mmap epochs; generations on disk are already immutable, so the swap
  is API-safe.
- Heap-backed (copied) reads instead of retained mmaps; the format is
  offset/length-addressed for a compatible mmap upgrade.
- Scalar distance only; `resolveDistance` is the v2 AVX2/NEON plug point.
- Stored vectors are never quantized (quantization deferred to v2).
