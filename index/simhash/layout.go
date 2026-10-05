package simhash

import (
	"math/bits"

	"github.com/evanoberholster/imageIdx/internal/storage"
)

// A hash page (the brief calls it a logical page) holds exactly 65,536
// ImageIDs: the low 16 bits of the ImageID are the slot, the high bits are the
// hash page index.
const (
	slotsPerHashPage = 1 << 16
	slotMask         = slotsPerHashPage - 1

	// Column sizes within one hash page.
	pHashColumnBytes   = 512 << 10 // 512 KiB
	pdqColumnBytes     = 2 << 20   // 2 MiB
	qualityColumnBytes = 1 << 16   // 64 KiB

	// Column offsets within one hash page. pHash is slot<<3, PDQ slot<<5,
	// quality slot, all 64-byte aligned. The sequence region follows the
	// payload.
	pHashColumnOffset   = 0
	pdqColumnOffset     = pHashColumnBytes
	qualityColumnOffset = pHashColumnBytes + pdqColumnBytes
	seqOffset           = pHashColumnBytes + pdqColumnBytes + qualityColumnBytes

	// headerBytes is the dual-slot superblock prefix.
	headerBytes = 2 * storage.SuperblockSlotBytes

	defaultMaxShardBytes   uint64 = 4 << 30
	defaultSeqBlockRecords        = 1024
	minimumShardBytes      uint64 = 1 << 20

	minSeqBlockRecords = 256
	maxSeqBlockRecords = 4096

	// optimizedBatchRecords is the batch size the update and append paths are
	// tuned for. Larger batches are processed as consecutive chunks of this
	// size, so the common path never allocates dynamic working structures.
	optimizedBatchRecords = 64
)

// validSeqBlockRecords reports whether n is a supported sequence block size: a
// power of two in [256,4096] that divides 65,536.
func validSeqBlockRecords(n int) bool {
	return n >= minSeqBlockRecords && n <= maxSeqBlockRecords && n&(n-1) == 0 && slotsPerHashPage%n == 0
}

// geometry is the byte geometry derived from the sequence block size. Column
// offsets are fixed; only the trailing sequence region, and therefore the hash
// page stride, depend on the block size. seqBlockShift lets the hot paths
// compute a sequence block with a shift instead of a division.
type geometry struct {
	seqBlockRecords  int
	seqBlockShift    uint
	seqBlocksPerPage int
	seqBytesPerPage  uint64
	hashPageStride   uint64
}

func newGeometry(seqBlockRecords int) geometry {
	blocks := slotsPerHashPage / seqBlockRecords
	return geometry{
		seqBlockRecords:  seqBlockRecords,
		seqBlockShift:    uint(bits.TrailingZeros(uint(seqBlockRecords))),
		seqBlocksPerPage: blocks,
		seqBytesPerPage:  uint64(blocks) * 8,
		hashPageStride:   seqOffset + uint64(blocks)*8,
	}
}

// blockOf returns the sequence block for a slot.
func (g geometry) blockOf(slot int) int { return slot >> g.seqBlockShift }

// slotOf returns the slot within the hash page for id.
func slotOf(id uint64) int { return int(id & slotMask) }

// hashPageOf returns the hash page index for id.
func hashPageOf(id uint64) uint64 { return id >> 16 }

// imageIDFromHashPage reconstructs the ImageID from a hash page and slot.
func imageIDFromHashPage(hashPage uint64, slot int) uint64 { return hashPage<<16 | uint64(slot) }
