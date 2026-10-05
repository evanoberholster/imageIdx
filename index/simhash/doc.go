// Package simhash implements a persistent, mmap-oriented similarity store for
// the canonical image-hash tuple: a 64-bit pHash, a 256-bit PDQ hash, and a PDQ
// quality byte.
//
// # Addressing
//
// There is no stored ImageID and no per-record offset table. ImageIDs are dense,
// stable, monotonically assigned, and never reused. The low 16 bits select the
// slot within a hash page and the high bits select the hash page:
//
//	slot := uint16(imageID & 0xffff)
//	hashPage := imageID >> 16
//
// A hash page (the brief calls it a logical page) covers exactly 65,536
// ImageIDs; the ImageID is recovered by hashPage<<16|slot.
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
// Replay is idempotent and validates the generation chain from the manifest
// generation, rejecting a gap. Duplicate ImageIDs within a batch are allowed and
// the last update wins.
//
// # Durability
//
// Runtime atomicity and crash durability are separate. Append and Upsert publish
// in memory and are visible to readers immediately. Checkpoint/Sync commit in
// this order:
//
//	canonical columns (already written in place)
//	-> flush canonical mmap
//	-> write new shard superblocks
//	-> flush shard metadata
//	-> publish the manifest LAST
//	-> fsync the directory
//	-> reset the WAL
//
// The manifest is the final durable commit marker: it can never claim a
// generation whose canonical data or shard metadata is not durable, and the WAL
// is discarded only after both are durable. This matches index/radixhash's
// explicit checkpoint model: Close does not checkpoint, and pending writes
// become durable only after Sync/Checkpoint (or per-operation under
// DurabilitySync). A validated WAL chain replayed on Open may advance the
// generation beyond the manifest until the next checkpoint.
//
// # Deletion
//
// The store owns no deletion state. Tombstoning and liveness are authoritative
// in BitmapIdx, which filters the results of a scan.
//
// # Kernels and SIMD
//
// The Hamming distance kernels live in index/simhash/internal/hashkernel behind
// one dispatch point: a portable scalar reference (math/bits; POPCNT on amd64)
// and a NEON block kernel on arm64. The scalar reference is the correctness
// oracle.
//
// The concurrent-safe scans (SearchPHash, SearchPDQ, SearchCombined) use atomic
// loads and the scalar kernel, because a 32-byte PDQ store is not atomic and a
// wide SIMD load cannot provide the same guarantee against an in-place writer.
// SearchPDQBulk uses the NEON block kernel on plain memory and is therefore only
// safe on a quiescent store (no concurrent writer); it is intended for scans
// after ingest or on a read-only replica. It is 1.6-2.5x faster than the scalar
// scan. Never replace the concurrent scalar path with wide loads merely for
// benchmark numbers.
package simhash
