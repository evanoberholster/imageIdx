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
// When candidates is nil the index performs a full sequential scan (distance
// first, then liveness). When a candidate set is supplied it is applied after
// the Hamming distance so dense live sets do not pay a bitmap lookup per
// record. A future accelerator may invert this order based on candidate
// density; V1 keeps the scalar, benchmarkable path.
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
//		Exact: ExactSpec{Algorithm: AlgorithmBLAKE3},
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
// # Persistence
//
// A persistent Store owns one directory:
//
//	.hashidx.lock  exclusive single-writer lock
//	perceptual/    perceptual Index artifacts
//	exact/         ExactIndex artifacts
//
// The perceptual index uses three artifact kinds under perceptual/:
//
//	hashidx.<generation>.data  generation header + page directory + pages
//	hashidx.<epoch>.wal        append-batch write-ahead log
//	manifest                   atomically published root (generation, WAL epoch, WAL offset)
//
// The exact index uses its own artifacts under exact/:
//
//	exact-<generation>.idx  generation header + block directory + blocks
//	exact-<epoch>.wal       append/remove write-ahead log
//	manifest                atomically published root (generation, WAL epoch, WAL offset, algorithm)
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
// Metadata reports the persisted generation, total record count, page count,
// and records per page for the perceptual index, and the stable IndexID,
// algorithm, generation, and block count for the exact index. Page size and
// exact block sizing are persistent properties: OpenStore adopts the recorded
// values rather than silently reinterpreting them.
//
// # Non-goals
//
// V1 deliberately has no ANN/metric structures (HNSW, VP-tree, BK-tree, MIH,
// IVF, LSH), no mutable mmap pages, no free lists or page reuse, no per-page
// locks, and no SIMD kernels. The SoA layout keeps those options open as
// rebuildable secondary structures over stable ordinals.
package hashidx
