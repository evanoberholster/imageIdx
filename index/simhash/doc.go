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
// A hash page (the brief calls it a logical page) therefore covers exactly
// 65,536 ImageIDs and the ImageID is recovered by hashPage<<16|slot.
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
// # Concurrency and the update journal
//
// Many lock-free readers plus one serialized writer. Searches never take a
// mutex. Point updates are performed in place (no COW) and protected by a
// per-block seqlock: the writer publishes an odd sequence, updates the columns,
// then publishes the next even sequence; a reader that observes a changed or
// odd sequence retries only the affected block. All column accesses use
// sync/atomic, so the tuple is race-free under the race detector while the
// sequence detects a torn 32-byte PDQ.
//
// Sequence counters are ephemeral runtime synchronization, not persistent state:
// they live in the mmap file but are meaningless across processes. Open always
// zeroes them after recovery, so a crash that left a counter odd can never make
// a reader spin forever.
//
// The seqlock only protects runtime readers. Crash consistency for in-place
// updates comes from a redo journal (see journal.go): each point update writes
// an idempotent record (ImageID + full replacement tuple + CRC) before touching
// the canonical columns. Open replays every valid record and repairs an
// interrupted tail. A crash before the record is written leaves the previous
// canonical state; a crash after it is completed by replay. A permanently mixed
// pHash/PDQ/quality tuple is therefore impossible for any update whose journal
// record survived.
//
// # Durability
//
// Runtime atomicity and crash durability are separate. Append and Upsert publish
// in memory and are visible to readers immediately. Checkpoint/Sync write the
// shard superblocks and manifest, flush the mappings, and only then truncate the
// journal. This matches index/radixhash's explicit checkpoint model: Close does
// not checkpoint, and pending writes become durable only after Sync/Checkpoint
// (or per-operation under DurabilitySync). After a crash without a checkpoint the
// store reopens at the last committed generation.
//
// The manifest is the authoritative committed generation. On Open, shard
// superblocks with a generation newer than the manifest are ignored as
// interrupted commits, and a shard that does not cover the committed records is
// rejected as corrupt.
//
// # Deletion
//
// The store owns no deletion state. Tombstoning and liveness are authoritative
// in BitmapIdx, which filters the results of a scan.
//
// # SIMD
//
// The scalar scans use atomic loads so they are correct against concurrent
// in-place updates. A future AVX2/AVX-512/NEON kernel cannot use plain wide
// loads for the same guarantee, because a 32-byte PDQ store is not atomic. SIMD
// may be enabled only after a design proves one of: sequence validation around
// SIMD reads is safe with a concurrent writer, SIMD scans run in a quiescent
// update epoch, or the scanned memory is immutable. The scalar path is the
// reference implementation.
package simhash
