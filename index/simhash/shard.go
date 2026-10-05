package simhash

import (
	"encoding/binary"
	"unsafe"

	"github.com/evanoberholster/imageIdx/internal/storage"
)

const (
	shardMagic        uint32 = 0x53485344 // "SHSD"
	shardVersion      uint16 = 1
	shardPayloadBytes        = 5 * 8
)

// shardMeta is the persisted per-shard metadata. committed is the number of
// valid records in the shard and generation is the last published generation.
type shardMeta struct {
	index      uint64
	firstPage  uint64
	pageCount  uint64
	committed  uint64
	generation uint64
}

// shard is one mmap-backed file holding a fixed range of logical pages. Its
// columns are fixed addresses; readers address them arithmetically from the
// slot.
type shard struct {
	index      uint64
	mapped     *storage.MappedFile
	arena      []byte
	firstPage  uint64
	pageCount  uint64
	stride     uint64
	slot       int
	committed  uint64
	generation uint64
}

// base returns the byte offset of a logical page within the shard.
func (s *shard) base(pageInShard uint64) uint64 {
	return uint64(headerBytes) + pageInShard*s.stride
}

// phash returns the pHash column word for a slot.
func (s *shard) phash(pageInShard uint64, slot int) *uint64 {
	off := s.base(pageInShard) + uint64(slot)*8
	return (*uint64)(unsafe.Pointer(&s.arena[off]))
}

// pdqWord returns word w (0..3) of the PDQ hash for a slot.
func (s *shard) pdqWord(pageInShard uint64, slot, word int) *uint64 {
	off := s.base(pageInShard) + pdqColumnOffset + uint64(slot)*32 + uint64(word)*8
	return (*uint64)(unsafe.Pointer(&s.arena[off]))
}

// qualityWord returns the 8-byte aligned word containing the quality byte for a
// slot, for atomic read-modify-write without expanding the column to uint16.
func (s *shard) qualityWord(pageInShard uint64, slot int) *uint64 {
	off := s.base(pageInShard) + qualityColumnOffset + uint64(slot&^7)
	return (*uint64)(unsafe.Pointer(&s.arena[off]))
}

// qualityShift is the bit shift of the quality byte within its aligned word.
func qualityShift(slot int) uint { return uint(slot&7) * 8 }

// seqPtr returns the sequence counter for a block within a logical page.
func (s *shard) seqPtr(pageInShard uint64, block int) *uint64 {
	off := s.base(pageInShard) + seqOffset + uint64(block)*8
	return (*uint64)(unsafe.Pointer(&s.arena[off]))
}

func encodeShardMeta(dst []byte, m shardMeta) error {
	var payload [shardPayloadBytes]byte
	binary.LittleEndian.PutUint64(payload[0:8], m.index)
	binary.LittleEndian.PutUint64(payload[8:16], m.firstPage)
	binary.LittleEndian.PutUint64(payload[16:24], m.pageCount)
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
		index:      binary.LittleEndian.Uint64(payload[0:8]),
		firstPage:  binary.LittleEndian.Uint64(payload[8:16]),
		pageCount:  binary.LittleEndian.Uint64(payload[16:24]),
		committed:  binary.LittleEndian.Uint64(payload[24:32]),
		generation: header.Generation,
	}, true
}

// writeMeta publishes the shard metadata into the inactive superblock slot and
// switches slots. The caller owns durability (Sync).
func (s *shard) writeMeta() error {
	next := 1 - s.slot
	off := next * storage.SuperblockSlotBytes
	if err := encodeShardMeta(s.arena[off:off+storage.SuperblockSlotBytes], shardMeta{
		index:      s.index,
		firstPage:  s.firstPage,
		pageCount:  s.pageCount,
		committed:  s.committed,
		generation: s.generation,
	}); err != nil {
		return err
	}
	s.slot = next
	return nil
}
