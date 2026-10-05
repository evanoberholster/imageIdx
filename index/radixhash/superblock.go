package radixhash

import (
	"encoding/binary"
	"fmt"
	"hash/crc32"

	"github.com/evanoberholster/imageIdx/internal/pagealloc"
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
const (
	superblockMagic   uint32 = 0x52485342 // RHSB
	superblockVersion uint16 = 1
	superblockBytes          = uint32(pagealloc.MinBlockSize)
	superblockSlots          = 2
	rootBlocks               = radixNodeBytes / pagealloc.MinBlockSize
	headerBlocks             = superblockSlots + superblockSlots*rootBlocks
)

// Superblock field offsets within a 512-byte slot.
const (
	sbMagic       = 0
	sbVersion     = 4
	sbFlags       = 6
	sbGeneration  = 8
	sbEntries     = 16
	sbRootBuckets = 24
	sbDepth       = 32
	sbRootCRC     = 36
	sbHeaderCRC   = 40
	sbHeaderEnd   = 44
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

// encodeSuperblock writes sb and the CRC of root into dst.
func encodeSuperblock(dst []byte, sb superblock, root []byte) {
	clear(dst[:superblockBytes])
	binary.LittleEndian.PutUint32(dst[sbMagic:], superblockMagic)
	binary.LittleEndian.PutUint16(dst[sbVersion:], superblockVersion)
	binary.LittleEndian.PutUint64(dst[sbGeneration:], sb.generation)
	binary.LittleEndian.PutUint64(dst[sbEntries:], sb.entries)
	binary.LittleEndian.PutUint64(dst[sbRootBuckets:], sb.rootBuckets)
	dst[sbDepth] = sb.depth
	binary.LittleEndian.PutUint32(dst[sbRootCRC:], crc32.ChecksumIEEE(root))
	binary.LittleEndian.PutUint32(dst[sbHeaderCRC:], crc32.ChecksumIEEE(dst[:sbHeaderCRC]))
}

// decodeSuperblock validates a slot and its root, returning ok=false for an
// empty or corrupt slot.
func decodeSuperblock(src, root []byte) (superblock, bool) {
	if len(src) < sbHeaderEnd || len(root) < radixNodeBytes {
		return superblock{}, false
	}
	if binary.LittleEndian.Uint32(src[sbMagic:]) != superblockMagic {
		return superblock{}, false
	}
	if binary.LittleEndian.Uint16(src[sbVersion:]) != superblockVersion {
		return superblock{}, false
	}
	if binary.LittleEndian.Uint32(src[sbHeaderCRC:]) != crc32.ChecksumIEEE(src[:sbHeaderCRC]) {
		return superblock{}, false
	}
	if binary.LittleEndian.Uint32(src[sbRootCRC:]) != crc32.ChecksumIEEE(root) {
		return superblock{}, false
	}
	return superblock{
		generation:  binary.LittleEndian.Uint64(src[sbGeneration:]),
		entries:     binary.LittleEndian.Uint64(src[sbEntries:]),
		rootBuckets: binary.LittleEndian.Uint64(src[sbRootBuckets:]),
		depth:       src[sbDepth],
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
		return fmt.Errorf("%w: short root", ErrCorrupt)
	}
	for i := range root {
		root[i] = binary.LittleEndian.Uint32(src[i*4:])
	}
	return nil
}
