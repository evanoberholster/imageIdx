// Package hashidx implements persistent, mmap-backed hash indexes for image
// similarity and exact-content duplicate detection.
//
// Index is the perceptual-hash index for similarity search. It stores three
// fixed-width columns in one shared Structure of Arrays (SoA) ordinal space:
//
//	ImageID uint64
//	pHash64 uint64
//	PDQ256  [4]uint64
//
// A physical record is 48 bytes; one ordinal names the same image across all
// six planes (ImageIDs, pHash64, PDQ0..PDQ3). Records are grouped into fixed
// logical pages of Config.RecordsPerPage records (8192 by default, i.e.
// 64 KiB per uint64 plane and 384 KiB of planes per page).
//
// # Store
//
// Store is the single entry point. NewStore creates a directory containing one
// Index and one ExactIndex and checkpoints both; OpenStore reopens it. Both
// indexes are mmap-backed, share one reader lifetime tracker and one exclusive
// directory lock, and reads never take a lock. Store.Checkpoint, Store.Vacuum,
// and Store.Close drive them together. Indexes are reached through
// Store.Perceptual and Store.Exact and are never constructed directly.
//
// # Canonical append-only storage
//
// Pages are append-only. Sealed pages are immutable and never edited in
// place; the writer keeps one active page with a published prefix. Readers
// load one atomically published indexView and scan immutable memory without
// taking a mutex. Publication is a single atomic pointer store per Apply, so
// a reader observes either the old prefix or the new prefix, never a torn
// batch.
//
// # Liveness is external
//
// hashidx never deletes a physical record during normal operation. Logical
// deletion belongs to the caller, typically in a bitmapidx "live images"
// bitmap intersected with other candidate filters. Search accepts a
// CandidateSet and returns only records admitted by it. Physical removal
// happens only through Vacuum, which rewrites live records densely into a new
// generation. Ordinals are internal and are not stable across Vacuum.
//
// Apply does not enforce ImageID uniqueness across appends. The canonical
// "at most one live record per ImageID" invariant belongs to the authoritative
// ImageID registry or live bitmap; if a caller appends a replacement record
// without removing the old one, both physical records are scanned and the
// candidate/liveness filter decides which one is visible.
//
// # Search modes
//
// pHash64 and PDQ256 are stored together but searched independently, so a
// query only pays for the hash it needs. Radius search computes an exact
// Hamming distance and appends matches in physical ordinal order:
//
//	SearchPHashRadius(dst, query, maxDistance, candidates)
//	SearchPDQRadius(dst, query, maxDistance, candidates)
//
// Top-K search returns up to k results ordered by ascending distance using a
// bounded max-heap, with deterministic ImageID tie-breaking; caller-supplied
// Workspace memory is reused:
//
//	SearchPHashTopK(dst, query, k, candidates, ws)
//	SearchPDQTopK(dst, query, k, candidates, ws)
//
// Radius search is distance-first: records failing the Hamming bound never
// pay a candidate lookup, so dense live sets cost one popcount per record
// and nothing else. Top-K search is candidates-first instead, so sparse
// candidate sets skip distance work per rejected record while dense sets
// pay a lookup per record. When candidates is nil both perform a full
// sequential scan. CountPHashRadius and CountPDQRadius run the radius
// order without materializing matches for planners. A future accelerator
// may pick the order from measured candidate density; V1 keeps the two
// scalar, benchmarkable paths with the tradeoff documented here.
//
// # Standalone scope
//
// hashIdx is usable standalone through Store (see ExampleStore): combined
// search integration stays a post-V1 co-design item. The seam is ready:
// CandidateSet admits any live/candidate bitmap (a roar.Bitmap satisfies it
// directly) and imageidx.ImageID is one type everywhere, so a future clause
// can filter hashIdx results by, and feed candidates into, other indexes
// without representation changes.
//
// # Exact content-hash index
//
// ExactIndex maps 256-bit content digests (SHA-256 or BLAKE3-256) to image IDs
// for exact-content identity and deduplication. It shares no storage with the
// perceptual index: records are fixed 40-byte [Hash32][ImageID8] pairs ordered
// by hash then image ID, grouped into immutable sorted copy-on-write blocks
// addressed by a small sorted directory. A mutation rewrites only the affected
// blocks and publishes one new immutable view, so a 1,000-image batch
// publishes once. Hash and Algorithm are independent of the perceptual
// columns:
//
//	store, err := NewStore(dir, StoreSpec{
//		Perceptual: PerceptualSpec{Namespace: 1, Name: "perceptual"},
//		Exact:      ExactSpec{Namespace: 2, Name: "exact", Algorithm: AlgorithmBLAKE3},
//	}, DurabilityOS)
//	ids := store.Exact().Lookup(dst, hash)
//	found := store.Exact().Contains(hash)
//
// One ExactIndex holds exactly one algorithm; it is persisted in the manifest
// and generation header and is authoritative on open. Remove wins over Add for
// the same (Hash, ImageID) pair in one Mutation, and multiple ImageIDs may
// share one hash. Hashing stays outside the index: SumSHA256, SumBLAKE3,
// HashReaderSHA256, and HashReaderBLAKE3 compute digests before lookup.
//
// Small publishes stage into an immutable overlay instead of rewriting
// blocks: the new view shares the block slice and carries the merged
// staged sets, so a single-pair publish never copies the block directory.
// Reads consult staged adds, then staged removes, then base blocks.
// Past OverlayMaxEntries staged entries the batch folds into the blocks
// and clears the overlay, amortizing the O(#blocks) directory copy.
// Checkpoint and Vacuum fold the overlay into what they persist, so files
// never contain staged state and the WAL tail replays onto folded
// generations without duplication.
//
// Repeated point lookups compile once with CompileLookup: the hash
// resolves against the current view and replays the cached IDs while the
// view generation is stable, re-resolving exactly once per generation
// change.
//
// Overlay reads cost up to two small binary probes before the base walk
// (measured +50-70ns on a 40-60ns base with a full overlay, 0 allocs).
// Workloads that burst-write then read hot should Checkpoint once after
// the burst — the checkpoint folds for the persisted file — or serve hot
// hashes through CompileLookup, which replays at ~6ns regardless of
// overlay depth.
//
// # Persistence
//
// A persistent Store owns one directory:
//
//	.hashidx.lock  exclusive single-writer lock
//	manifest       joint generation anchor (advisory; see below)
//	perceptual/    perceptual Index artifacts
//	exact/         ExactIndex artifacts
//
// The single-writer lock is flock on Unix and LockFileEx on Windows (see
// the filelock files in internal/storage); other platforms fail fast
// instead of running unlocked. Mmap-backed reads are Unix-only until
// internal/storage gains a MapViewOfFile backend, so Windows support
// also waits on that.
//
// The perceptual index uses three artifact kinds under perceptual/:
//
//	hashidx.<generation>.data  generation header + page directory + pages
//	hashidx.<epoch>.wal        append-batch write-ahead log
//	manifest                   atomically published root (namespace, generation, WAL epoch, WAL offset)
//
// The exact index uses its own artifacts under exact/:
//
//	exact-<generation>.idx  generation header + block directory + blocks
//	exact-<epoch>.wal       append/remove write-ahead log
//	manifest                atomically published root (namespace, generation, WAL epoch, WAL offset, algorithm)
//
// The generation headers, page/block directories, and WAL frames use explicit
// little-endian encoding with CRC32/IEEE checksums. Every SoA plane is 8-byte
// aligned (and 64-byte aligned for full pages). OpenStore maps both
// generations, validates every page and block, and replays only complete WAL
// batches after each checkpoint offset, stopping cleanly at truncated tails.
// Store.Checkpoint commits the current sealed pages/active prefix and exact
// block directory as new generations. Store.Vacuum rewrites only records
// admitted by the live set (nil keeps every perceptual record), repacks the
// exact blocks, and starts a fresh WAL epoch for both indexes; obsolete
// generations and WAL epochs are removed after prior readers finish.
//
// Metadata reports the persisted namespace, generation, total record count,
// page count, and records per page for the perceptual index, and the
// namespace, algorithm, generation, and block count for the exact index.
// Page size and exact block sizing are persistent properties: OpenStore
// adopts the recorded values rather than silently reinterpreting them.
//
// Store.Checkpoint persists perceptual then exact, then the joint anchor
// naming both generations; a failure between them leaves the indexes
// diverged by design, and the next Checkpoint converges while the stale
// anchor still names the last joint pair. The anchor is advisory only:
// sub-manifests stay authoritative, a missing file (pre-V1 stores) is
// normal, and OpenStore never consults it.
//
// # Format stability
//
// Every artifact carries magic plus version, and every payload carries a
// CRC32/IEEE: recognized magic with an unknown version fails open with
// ErrUnsupported (never misread as same-version rot), corrupt bytes fail
// with ErrCorrupt. The perceptual manifest version 2 adds the index
// namespace; version 1 manifests fail open with ErrUnsupported. The exact
// manifest and generation header keep their versions: the namespace reuses
// the old uint64 identity field zero-extended, and values above uint16 are
// rejected as corrupt. WAL framing violations report ErrCorruptWAL either way:
// WALs are transient epochs, rolled on every Vacuum, so they carry no
// upgrade path — Vacuum before upgrading. Batch limits, overlay bounds,
// and durability are runtime policy, never persisted, so tuning them
// changes no bytes on disk. (Exact block sizing, page sizing, and the
// digest algorithm are persisted and adopted on open.) There is no
// migration framework yet: V1 opens what V1 wrote, and any future version
// bump must add a tested upgrade path before it ships.
//
// # Non-goals
//
// V1 deliberately has no ANN/metric structures (HNSW, VP-tree, BK-tree, MIH,
// IVF, LSH), no mutable mmap pages, no free lists or page reuse, no per-page
// locks, and no SIMD kernels. The SoA layout keeps those options open as
// rebuildable secondary structures over stable ordinals.
package hashidx
