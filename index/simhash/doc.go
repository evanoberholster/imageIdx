// Package simhash implements a persistent, mmap-oriented similarity store for
// the canonical image-hash tuple: a 64-bit pHash, a 256-bit PDQ hash, and a PDQ
// quality byte.
//
// # Addressing
//
// There is no stored ImageID and no per-record offset table. ImageIDs are dense,
// stable, monotonically assigned, and never reused. The low 16 bits select the
// slot within a logical image page and the high bits select the page:
//
//	slot := uint16(imageID & 0xffff)
//	logicalPage := imageID >> 16
//
// A logical page therefore covers exactly 65,536 ImageIDs and the ImageID is
// recovered by page<<16|slot.
//
// # Columnar layout
//
// Each logical page stores the three values as independent contiguous columns:
//
//	pHash64[65536]    512 KiB
//	PDQ256[65536]       2 MiB
//	PDQQuality[65536]  64 KiB
//
// so a pHash-only scan reads only pHash, a PDQ-only scan reads only PDQ, and
// quality is fetched on demand. Columns are 64-byte aligned and addressed by
// arithmetic (slot<<3, slot<<5, slot).
//
// # Concurrency
//
// Many lock-free readers plus one serialized writer. Searches never take a
// mutex. Point updates are performed in place and protected by a per-block
// sequence counter (a seqlock): the writer publishes an odd sequence, updates
// the columns, then publishes the next even sequence; a reader that observes a
// changed or odd sequence retries only the affected block. A 32-byte PDQ write
// is not atomic, so the sequence validates the tuple rather than relying on a
// wide atomic store.
//
// # Durability
//
// Runtime atomicity and crash durability are separate. Sequence counters protect
// readers from torn in-place updates; they do not make an update crash-atomic.
// The store reuses internal/storage for the mmap arena, the dual-slot CRC32C
// superblock envelope, atomic file publication, and the ReaderTracker epoch.
// Appends become visible to readers when a new view is published; the manifest
// and shard superblocks are flushed according to the configured Durability.
//
// # Deletion
//
// The store owns no deletion state. Tombstoning and liveness are authoritative
// in BitmapIdx, which filters the results of a scan.
package simhash
