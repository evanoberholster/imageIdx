// Package vectoridx implements VectorIdx, a persistent HNSW
// approximate-nearest-neighbor vector index following AGENTS/VectorIdx.MD.
//
// # Scope of V1
//
//   - Closed dimensions {512, 768, 1024} and one element type per index
//     {int8, uint8, int16, uint16, float32}, stored as received.
//   - One metric per index {L2, cosine, dot} with scalar kernels and float64
//     accumulation; public Result Score is a saturating uint32, monotonic
//     per metric and zero for self matches.
//   - SpaceID is the packed identity of one vector space: the high byte is the
//     IndexType (IndexTypeImage, IndexTypeFace, or IndexTypeCustom) and the
//     low byte is the space number within that type. IDs are stored and
//     filtered as compact uint32 NodeIDs and widened to imageidx.ImageID in
//     Result values.
//   - Standard HNSW insertion/search with direct uint32 NodeID edges, a dense
//     O(1) locator with sorted sparse fallback, and heuristic neighbor
//     selection (closest-only available for benchmarking).
//   - Tombstone deletion in an immutable roaring bitmap: deleted nodes stay
//     traversable, never returned, purged by explicit or background Compact
//     which never renumbers NodeIDs.
//   - Post-ANN candidate filtering with dynamic ef, oversampling, iterative
//     expansion, cost-based flat fallback, and an exact-scan fallback
//     guaranteeing k-or-available semantics under filters.
//   - Single-file multi-region persistence (superblock, region directory,
//     node metadata, vectors, graph, tombstones) with CRC32 integrity,
//     framed WAL with prefix recovery, explicit Checkpoint/Compact, and a
//     per-path lock that rejects a second writer with ErrStoreBusy.
//   - Durability policy per open (OS page cache, per-record fsync, or
//     interval group commit), explicit Sync, and Stats/FileSize counters.
//   - Lock-free reads over atomically published snapshots plus a
//     ReaderTracker epoch; one serialized writer with an async queue.
//   - Pooled SearchContext scratch (heaps, generation-tag visited set,
//     query buffer) and SearchInto result reuse with zero steady-state allocs
//     on the flat and ANN paths.
//
// # What V1 simplifies
//
//   - Readers use heap snapshots rather than fully lock-free mmap epoch
//     reads. File generations remain immutable; a future change can swap in
//     mmap views without altering the file format or public API.
//   - Distance is scalar only. resolveDistance is the single dispatch point
//     where v2 AVX2/NEON kernels plug in behind the distRaw signature.
//   - Stored vectors are never quantized (v2 work).
//   - Open copies region payloads into heap storage instead of retaining an
//     mmap. The on-disk layout is already offset/length-addressed so a later
//     mmap optimisation keeps format compatibility.
//   - Re-adding a live NodeID is rejected; re-adding a tombstoned NodeID
//     replaces its vector and reconnects it, so Delete followed by Add is an
//     update. Compact remains the way to purge tombstones and rebuild.
//
// # Basic use
//
//	idx, err := vectoridx.Create(path, vectoridx.Config{
//	    SpaceID:      vectoridx.NewSpaceID(vectoridx.IndexTypeImage, 1),
//	    Dimensions:  vectoridx.Dim512,
//	    ElementType: vectoridx.ElementFloat32,
//	}, vectoridx.DurabilitySync)
//	if err != nil { ... }
//	defer idx.Close()
//
//	idx.AddFloat32(7, queryVec)                       // NodeID 7 (uint32)
//	results, err := idx.SearchFloat32(queryVec, nil, 10)
//	// results[i] is a Result{ImageID imageidx.ImageID, Score uint32}, with
//	// ImageID widened from the stored NodeID.
//
// Filter with any bitmap representation through CandidateFilter, which sees
// the stored uint32 NodeID:
//
//	filter := vectoridx.CandidateFilter(func(id vectoridx.NodeID) bool {
//	    return allowSet[id]
//	})
//	results, err := idx.SearchFloat32(queryVec, filter, 10)
//
// In-memory indexes skip persistence:
//
//	idx, err := vectoridx.New(vectoridx.Config{...})
package vectoridx
