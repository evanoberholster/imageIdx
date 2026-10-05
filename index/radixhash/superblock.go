package radixhash

import (
	"encoding/binary"

	"github.com/evanoberholster/imageIdx/internal/pagealloc"
	"github.com/evanoberholster/imageIdx/internal/storage"
)

// On-disk arena header layout (per slot, so a crash mid-checkpoint leaves the
// other slot and its root intact):
//
//	block 0      superblock slot A (512 B)
//	block 1      superblock slot B (512 B)
//	blocks 2-3   root radix node A (256 * uint32 = 1024 B)
//	blocks 4-5   root radix node B
//	blocks 6+    allocator-managed leaves and interior nodes
//
// Checkpoint writes the inactive slot's root and superblock, flushes, then
// switches the active slot. Reopen reads both slots, keeps the valid one with
// the highest generation, and reconstructs allocation state by walking the
// tree, so no free-space bitmap is persisted.
//
// The superblock envelope, CRCs, version check, and slot selection live in
// internal/storage; radixhash owns only its payload and its per-slot root.
const (
	superblockMagic   uint32 = 0x52485342 // RHSB
	superblockVersion uint16 = 2
	superblockBytes          = storage.SuperblockSlotBytes
	superblockSlots          = storage.SuperblockSlots
	rootBlocks               = radixNodeBytes / pagealloc.MinBlockSize
	headerBlocks             = superblockSlots + superblockSlots*rootBlocks

	// radixPayloadBytes is the radix checkpoint payload: entries(8) +
	// rootBuckets(8) + depth(1) + root CRC32C(4).
	radixPayloadBytes = 21
)

// superblock is the persisted root metadata for one arena generation.
type superblock struct {
	generation  uint64
	entries     uint64
	rootBuckets uint64
	depth       uint8
}

// superblockOffset is the byte offset of a superblock slot.
func superblockOffset(slot int) uint64 {
	return uint64(slot) * uint64(superblockBytes)
}

// rootOffset is the byte offset of a slot's persisted root node.
func rootOffset(slot int) uint64 {
	return uint64(superblockSlots+slot*int(rootBlocks)) * uint64(pagealloc.MinBlockSize)
}

// encodeRadixSuperblock writes the radix payload and common envelope into one
// slot. The payload binds the root bytes with a CRC, so a torn root is rejected
// together with its slot.
func encodeRadixSuperblock(dst []byte, sb superblock, root []byte) error {
	var payload [radixPayloadBytes]byte
	binary.LittleEndian.PutUint64(payload[0:8], sb.entries)
	binary.LittleEndian.PutUint64(payload[8:16], sb.rootBuckets)
	payload[16] = sb.depth
	binary.LittleEndian.PutUint32(payload[17:21], storage.Checksum(root))
	return storage.EncodeSuperblock(dst, storage.SuperblockHeader{
		Magic:      superblockMagic,
		Version:    superblockVersion,
		Generation: sb.generation,
	}, payload[:])
}

// decodeRadixSuperblock validates the envelope, the radix payload, and the root
// it describes. ok is false for an empty, wrong-version, or corrupt slot.
func decodeRadixSuperblock(src, root []byte) (superblock, bool) {
	header, payload, ok := storage.DecodeSuperblock(src, superblockMagic, superblockVersion)
	if !ok || len(payload) < radixPayloadBytes {
		return superblock{}, false
	}
	if binary.LittleEndian.Uint32(payload[17:21]) != storage.Checksum(root) {
		return superblock{}, false
	}
	return superblock{
		generation:  header.Generation,
		entries:     binary.LittleEndian.Uint64(payload[0:8]),
		rootBuckets: binary.LittleEndian.Uint64(payload[8:16]),
		depth:       payload[16],
	}, true
}

// reserveHeader pins the fixed superblock and root blocks. pagealloc.New
// already reserves block zero, so only blocks one through headerBlocks-1 need
// reserving.
func reserveHeader(allocator *pagealloc.Allocator) error {
	blocks := uint32(headerBlocks - 1)
	return allocator.Reserve(pagealloc.Ref(1), blocks*uint32(pagealloc.MinBlockSize))
}

// encodeRoot serializes the root node into dst.
func encodeRoot(dst []byte, root *[radixFanout]uint32) {
	for i, child := range root {
		binary.LittleEndian.PutUint32(dst[i*4:], child)
	}
}

// decodeRoot deserializes the root node from src.
func decodeRoot(src []byte, root *[radixFanout]uint32) error {
	if uint64(len(src)) < uint64(radixNodeBytes) {
		return ErrCorrupt
	}
	for i := range root {
		root[i] = binary.LittleEndian.Uint32(src[i*4:])
	}
	return nil
}
