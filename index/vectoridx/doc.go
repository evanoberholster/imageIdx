// Package vectoridx implements VectorIdx, a persistent HNSW
// approximate-nearest-neighbor vector index following AGENTS/VectorIdx.MD.
//
// # Scope of V1
//
//   - Fixed dimensions {512, 768, 1024} and one element type per index
//     {int8, uint8, int16, uint16, float32}, stored as received.
//   - Squared-L2 ranking with scalar reference kernels and float64
//     accumulation; public Result Score is a saturating uint32.
//   - Standard HNSW insertion/search with direct uint32 NodeID edges,
//     sorted-metadata locator (binary search), and closest-only neighbor
//     selection.
//   - Tombstone deletion: deleted nodes stay traversable, never returned,
//     purged by explicit Compact which never renumbers NodeIDs.
//   - Post-ANN candidate filtering with oversampling and an exact-scan
//     fallback guaranteeing k-or-available semantics under filters.
//   - Single-file multi-region persistence (superblock, region directory,
//     node metadata, vectors, graph, tombstones) with CRC32 integrity,
//     framed WAL with prefix recovery, explicit Checkpoint/Compact.
//   - Pooled SearchContext scratch (heaps, generation-tag visited set,
//     query buffer) and SearchInto result reuse.
//
// # What V1 simplifies
//
//   - Readers hold a shared RWMutex plus a ReaderTracker token instead of
//     fully lock-free mmap epoch reads. File generations remain immutable;
//     a future Phase-3 change can swap the lock for epoch reclamation
//     without altering the file format or public API.
//   - Distance is scalar only. resolveDistance is the single dispatch point
//     where AVX2/NEON kernels plug in behind the distRaw signature.
//   - Open copies region payloads into heap storage instead of retaining an
//     mmap. The on-disk layout is already offset/length-addressed so a later
//     mmap optimisation keeps format compatibility.
//   - Re-adding a NodeID (even tombstoned) is rejected until Compact purges
//     it, keeping vector replacement semantics explicit.
//
// # Basic use
//
//	idx, err := vectoridx.Create(path, vectoridx.Config{
//	    TypeID:      1,
//	    Dimensions:  vectoridx.Dim512,
//	    ElementType: vectoridx.ElementFloat32,
//	})
//	if err != nil { ... }
//	defer idx.Close()
//
//	if err := idx.AddFloat32(7, queryVec); err != nil { ... }
//	results, err := idx.SearchFloat32(queryVec, nil, 10)
//	if err != nil { ... }
//
// Filter with any bitmap representation through CandidateFilter:
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
