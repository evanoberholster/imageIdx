package simhash

import (
	"encoding/binary"
	"math/bits"
	"unsafe"

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
	// tuned for.
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
	hashPageStride   uint64
}

func newGeometry(seqBlockRecords int) geometry {
	blocks := slotsPerHashPage / seqBlockRecords
	return geometry{
		seqBlockRecords:  seqBlockRecords,
		seqBlockShift:    uint(bits.TrailingZeros(uint(seqBlockRecords))),
		seqBlocksPerPage: blocks,
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

const (
	shardMagic        uint32 = 0x53485344 // "SHSD"
	shardVersion      uint16 = 1
	shardPayloadBytes        = 5 * 8
)

// shardMeta is the persisted per-shard metadata. committed is the number of
// valid records in the shard and generation is the last committed generation.
type shardMeta struct {
	index         uint64
	firstPage     uint64
	hashPageCount uint64
	committed     uint64
	generation    uint64
}

// shard is one mmap-backed file holding a fixed range of hash pages. Its
// columns are fixed addresses; readers address them arithmetically from the
// slot. Sequence counters live in the same file but are runtime-only state.
type shard struct {
	index         uint64
	mapped        *storage.MappedFile
	arena         []byte
	firstPage     uint64
	hashPageCount uint64
	stride        uint64
	slot          int
	committed     uint64
	generation    uint64
}

// base returns the byte offset of a hash page within the shard. Page starts are
// 64-byte aligned so every column and every atomic uint64 is aligned.
func (s *shard) base(hashPage uint64) uint64 {
	return uint64(headerBytes) + hashPage*s.stride
}

// phash returns the pHash column word for a slot (8-byte aligned).
func (s *shard) phash(hashPage uint64, slot int) *uint64 {
	off := s.base(hashPage) + uint64(slot)*8
	return (*uint64)(unsafe.Pointer(&s.arena[off]))
}

// pdqWord returns word w (0..3) of the PDQ hash for a slot (8-byte aligned).
func (s *shard) pdqWord(hashPage uint64, slot, word int) *uint64 {
	off := s.base(hashPage) + pdqColumnOffset + uint64(slot)*32 + uint64(word)*8
	return (*uint64)(unsafe.Pointer(&s.arena[off]))
}

// qualityWord returns the 8-byte aligned word containing the quality byte for a
// slot, for atomic read-modify-write without expanding the column to uint16.
func (s *shard) qualityWord(hashPage uint64, slot int) *uint64 {
	off := s.base(hashPage) + qualityColumnOffset + uint64(slot&^7)
	return (*uint64)(unsafe.Pointer(&s.arena[off]))
}

// qualityShift is the bit shift of the quality byte within its aligned word.
func qualityShift(slot int) uint { return uint(slot&7) * 8 }

// seqPtr returns the sequence counter for a block within a hash page.
func (s *shard) seqPtr(hashPage uint64, block int) *uint64 {
	off := s.base(hashPage) + seqOffset + uint64(block)*8
	return (*uint64)(unsafe.Pointer(&s.arena[off]))
}

// resetSequences zeroes all sequence counters. Sequence counters are ephemeral
// runtime synchronization; they must be even after open.
func (s *shard) resetSequences(blocksPerPage int) {
	n := uint64(blocksPerPage) * 8
	for pi := uint64(0); pi < s.hashPageCount; pi++ {
		off := s.base(pi) + seqOffset
		clear(s.arena[off : off+n])
	}
}

func encodeShardMeta(dst []byte, m shardMeta) error {
	var payload [shardPayloadBytes]byte
	binary.LittleEndian.PutUint64(payload[0:8], m.index)
	binary.LittleEndian.PutUint64(payload[8:16], m.firstPage)
	binary.LittleEndian.PutUint64(payload[16:24], m.hashPageCount)
	binary.LittleEndian.PutUint64(payload[24:32], m.committed)
	binary.LittleEndian.PutUint64(payload[32:40], m.generation)
	return storage.EncodeSuperblock(dst, storage.SuperblockHeader{
		Magic:      shardMagic,
		Version:    shardVersion,
		Generation: m.generation,
	}, payload[:])
}

func decodeShardMeta(src []byte) (shardMeta, bool) {
	header, payload, ok := storage.DecodeSuperblock(src, shardMagic, shardVersion)
	if !ok || len(payload) < shardPayloadBytes {
		return shardMeta{}, false
	}
	return shardMeta{
		index:         binary.LittleEndian.Uint64(payload[0:8]),
		firstPage:     binary.LittleEndian.Uint64(payload[8:16]),
		hashPageCount: binary.LittleEndian.Uint64(payload[16:24]),
		committed:     binary.LittleEndian.Uint64(payload[24:32]),
		generation:    header.Generation,
	}, true
}

// committedInShard returns the number of valid records in a shard covering
// slots [firstID, firstID+capacity) up to and including lastID. ImageID 0 is
// reserved and never counted. It uses subtraction-based comparisons so it never
// overflows for lastID up to MaxUint64.
func committedInShard(firstID, capacity, lastID uint64) uint64 {
	offset := uint64(0)
	if firstID == 0 {
		offset = 1 // ImageID 0 is reserved.
	}
	if capacity <= offset {
		return 0
	}
	avail := capacity - offset
	low := firstID + offset
	if lastID < low {
		return 0
	}
	rel := lastID - low
	if rel >= avail {
		return avail
	}
	return rel + 1
}

// writeMeta publishes the shard metadata into the inactive superblock slot and
// switches slots. The caller owns durability (Sync).
func (s *shard) writeMeta() error {
	next := 1 - s.slot
	off := next * storage.SuperblockSlotBytes
	if err := encodeShardMeta(s.arena[off:off+storage.SuperblockSlotBytes], shardMeta{
		index:         s.index,
		firstPage:     s.firstPage,
		hashPageCount: s.hashPageCount,
		committed:     s.committed,
		generation:    s.generation,
	}); err != nil {
		return err
	}
	s.slot = next
	return nil
}
