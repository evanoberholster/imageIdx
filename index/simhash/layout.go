package simhash

import "github.com/evanoberholster/imageIdx/internal/storage"

const (
	// slotsPerPage is 65,536 ImageIDs per logical page.
	slotsPerPage = 1 << 16
	slotMask     = slotsPerPage - 1

	// Column sizes within one logical page.
	pHashColumnBytes   = 512 << 10 // 512 KiB
	pdqColumnBytes     = 2 << 20   // 2 MiB
	qualityColumnBytes = 1 << 16   // 64 KiB
	pagePayloadBytes   = pHashColumnBytes + pdqColumnBytes + qualityColumnBytes

	// Column offsets within one logical page. pHash is slot<<3, PDQ slot<<5,
	// quality slot.
	pHashColumnOffset   = 0
	pdqColumnOffset     = pHashColumnBytes
	qualityColumnOffset = pHashColumnBytes + pdqColumnBytes
	seqOffset           = pagePayloadBytes

	// headerBytes is the dual-slot superblock prefix.
	headerBytes = 2 * storage.SuperblockSlotBytes

	defaultMaxShardBytes   uint64 = 4 << 30
	defaultSeqBlockRecords        = 1024
	minimumShardBytes      uint64 = 1 << 20

	minSeqBlockRecords = 256
	maxSeqBlockRecords = 4096
)

// validSeqBlockRecords reports whether n is a supported sequence block size: a
// power of two in [256,4096] that divides 65,536.
func validSeqBlockRecords(n int) bool {
	return n >= minSeqBlockRecords && n <= maxSeqBlockRecords && n&(n-1) == 0 && slotsPerPage%n == 0
}

// geometry is the byte geometry derived from the sequence block size. Column
// offsets are fixed; only the trailing sequence region and therefore the page
// stride depend on the block size.
type geometry struct {
	seqBlockRecords  int
	seqBlocksPerPage int
	seqBytesPerPage  uint64
	pageStride       uint64
}

func newGeometry(seqBlockRecords int) geometry {
	blocks := slotsPerPage / seqBlockRecords
	return geometry{
		seqBlockRecords:  seqBlockRecords,
		seqBlocksPerPage: blocks,
		seqBytesPerPage:  uint64(blocks) * 8,
		pageStride:       pagePayloadBytes + uint64(blocks)*8,
	}
}

// slotOf returns the slot within the logical page for id.
func slotOf(id uint64) int { return int(id & slotMask) }

// pageOf returns the logical page for id.
func pageOf(id uint64) uint64 { return id >> 16 }

// idOf reconstructs the ImageID from a logical page and slot.
func idOf(page uint64, slot int) uint64 { return page<<16 | uint64(slot) }
