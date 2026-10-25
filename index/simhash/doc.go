// Package simhash implements a persistent, mmap-oriented similarity store for
// the canonical image-hash tuple: a 64-bit pHash, a 256-bit PDQ hash, and a PDQ
// quality byte.
//
// Current architecture is summarized in AGENTS/ARCHITECTURE.md; usage and
// measured results are in index/simhash/README.md.
//
// # Addressing
//
// The dense path has no stored ImageID and no per-record offset table. Dense
// ImageIDs are contiguous, stable, and assigned by the append API. The low 16
// bits select the slot within a hash page and the high bits select the hash
// page:
//
//	slot := uint16(imageID & 0xffff)
//	hashPage := imageID >> 16
//
// A hash page covers exactly 65,536 ImageIDs; the ImageID is recovered by
// hashPage<<16|slot.
//
// Arbitrary nonzero external IDs use the durable sparse overlay described in
// index/simhash/README.md. Sparse records are stored in immutable CRC-checked
// segments with an atomically replaced manifest; the overlay filters dense
// records it replaces and results remain in ascending ImageID order. Once a
// sparse overlay is used, dense append APIs are rejected so there is one source
// of truth.
//
// # Columnar layout
//
// Each hash page stores the three values as independent contiguous columns:
//
//	pHash64[65536]    512 KiB
//	PDQ256[65536]       2 MiB
//	PDQQuality[65536]  64 KiB
//
// so a pHash-only scan reads only pHash, a PDQ-only scan reads only PDQ, and
// quality is fetched on demand. Columns are 64-byte aligned and addressed by
// arithmetic (slot<<3, slot<<5, slot). Total payload is 41 bytes per image.
//
// # Concurrency
//
// Many lock-free readers plus one serialized writer. Searches never take a
// mutex. Point updates are performed in place (no COW) and protected by a
// per-block seqlock: the writer publishes an odd sequence, updates the columns,
// then publishes the next even sequence; a reader that observes a changed or odd
// sequence retries only the affected block. All column accesses use sync/atomic,
// so the tuple is race-free under the race detector while the sequence detects a
// torn 32-byte PDQ.
//
// A batch groups consecutive updates that share a sequence block, so the block's
// seqlock toggles once per run rather than once per record. The batch is not
// sorted: benchmarks show sorting helps only already-local batches and regresses
// random ones where every record is in a different block.
//
// Search consistency is per sequence block, not whole-query snapshot isolation:
// a long scan may observe an already-updated block and a not-yet-updated block,
// but every individual block it reads is internally consistent.
//
// # Sequence counters are ephemeral
//
// Sequence counters live in the mmap file but are runtime synchronization state,
// not persistent state. Open zeroes every counter after recovery, so a crash
// that left one odd can never make a reader spin forever.
//
// # Appends versus updates
//
// AppendBatch writes new slots that are not yet visible (the committed
// high-water mark has not advanced), so a crash cannot expose a torn record and
// appends do not use the update WAL. Upsert and UpsertBatch overwrite already
// visible records, so they use a redo WAL because a 32-byte PDQ store is not
// atomic and a crash mid-update could otherwise leave a torn tuple.
//
// # Update WAL and generations
//
// The update WAL is batch-oriented: one frame carries one logical transaction
// with a base generation, a new generation, fixed-width updates, and a CRC.
// Recovery never exposes half a batch. Protocol:
//
//	validate the batch
//	encode one complete WAL frame
//	append it (fsync under DurabilitySync)
//	apply all records, grouping sequence blocks
//	advance and publish the generation once
//
// For 64 updates that is one WAL transaction and one generation step, not 64.
// Replay is idempotent and validates a single-step generation chain. A forward
// gap from uncheckpointed appends is tolerated, and an update whose ImageID is
// beyond the manifest high-water is dropped (its append was lost) rather than
// treated as corruption. Duplicate ImageIDs within a batch are allowed and the
// last update wins.
//
// # Durability
//
// Runtime atomicity and crash durability are separate. Under the default OS
// durability, Append and Upsert publish in memory and are visible to readers
// immediately; Checkpoint/Sync makes them durable. Under DurabilitySync a commit
// is made durable before it is published to readers.
//
// The durable commit sequence is split at the manifest write, which is the
// commit marker (it fsyncs its temporary file, renames, and fsyncs the
// directory):
//
//	PRE-COMMIT
//	  canonical mmap Sync
//	  -> write new shard superblocks
//	  -> shard mmap Sync
//	  -> atomically publish the manifest
//	COMMITTED
//	POST-COMMIT
//	  -> reset the WAL
//
// A failure before or at the manifest write returns an error and, under
// DurabilitySync, rolls back the unpublished AppendBatch writer state; readers
// never observe the new IDs. A failure after the manifest write (WAL reset)
// leaves the commit in place and returns the error; the stale WAL is skipped
// (already-committed batches) or repaired on the next Open. The manifest can
// never claim a generation whose canonical data or shard metadata is not
// durable.
//
// This matches index/radixhash's explicit checkpoint model: Close does not
// checkpoint, and pending writes become durable only after Sync/Checkpoint (or
// per-operation under DurabilitySync). A validated WAL chain replayed on Open
// may advance the generation beyond the manifest until the next checkpoint.
//
// # Deletion
//
// The store owns no deletion state. Tombstoning and liveness are authoritative
// in BitmapIdx, which filters the results of a scan.
//
// # Similarity search
//
// Two exact radius queries are supported: SearchPHash (64-bit pHash Hamming
// distance) and SearchPDQ (256-bit PDQ distance). Both append matches to a
// caller-owned []Result and return them in ascending ImageID order. Result
// carries only the ImageID and the exact Hamming distance as a uint8; the
// caller knows which search produced it. No score, normalized distance, hash
// type, quality, Top-K, sorting, or count-only variant is provided.
//
// Search thresholds are uint8. pHash distance is exactly 0..64. PDQ is 256 bits,
// so its mathematical distance is 0..256, but the public search radius is
// bounded by MaxPDQSearchDistance (128): a larger threshold is clamped rather
// than rejected. The internal distance primitive stays uint16 and the running
// distance is never narrowed to uint8 before the threshold test, so every
// emitted Result.Distance is exact. SearchPDQBulk is the quiescent SIMD variant
// and must return exactly the same results as SearchPDQ, including for clamped
// thresholds.
//
// The distance split is:
//
//	pHash: mathematical 0..64,  search threshold 0..64,  Result.Distance uint8
//	PDQ:   mathematical 0..256, search threshold 0..128, Result.Distance uint8
//
// # Kernels and SIMD
//
// The Hamming distance kernels live in index/simhash/internal/hashkernel behind
// one dispatch point: a portable scalar reference (math/bits; POPCNT on amd64)
// and a NEON block kernel on arm64. The scalar reference is the correctness
// oracle.
//
// The concurrent-safe scans (SearchPHash, SearchPDQ) use atomic loads and the
// scalar kernel, because a 32-byte PDQ store is not atomic and a wide SIMD load
// cannot provide the same guarantee against an in-place writer.
// SearchPDQBulk uses the NEON block kernel on plain memory and is therefore only
// safe on a quiescent store (no concurrent writer); it is intended for scans
// after ingest or on a read-only replica. It is 1.5-2.2x faster than the scalar
// scan at realistic thresholds. Never replace the concurrent scalar path with
// wide loads merely for benchmark numbers.
//
// There is no SIMD pHash kernel: one XOR plus hardware POPCNT per record is
// already ~0.8 ns/hash. An AVX2 PDQ kernel exists for amd64 with runtime CPU
// detection but is not enabled in the default dispatch until an amd64 benchmark
// shows it beats four scalar XOR+POPCNT operations end to end. AVX-512 is out
// of scope.
package simhash
