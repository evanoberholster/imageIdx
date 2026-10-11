// Package pagealloc implements a persistent, 512-byte-granular allocator for
// mmap-backed index files.
//
// The allocator knows nothing about hashes, radix trees, or any other index
// payload. It hands out contiguous runs of 512-byte blocks inside a
// caller-owned arena and tracks free space with one compact free bitmap. All
// persistent references are block addresses (Ref values), never Go pointers,
// so an allocation stays valid when the arena is unmapped and mapped again.
//
// # Exact-fit allocation
//
// MinBlockSize is 512 bytes. An allocation of size bytes occupies exactly
// ceil(size/MinBlockSize) consecutive blocks and reports its actual byte
// capacity, so a 5,808-byte leaf reserves 6,144 bytes rather than the next
// power of two (8,192). This avoids the large rounding waste of a buddy system
// while keeping the same 512-byte persistence granularity. Radix nodes request
// exactly 1024 bytes (two blocks) and therefore stay naturally aligned by
// construction; the allocator itself only guarantees MinBlockSize alignment.
//
// # Block zero
//
// Block address zero is reserved as the null Ref so a zero child in a radix
// node can mean "never allocated". The allocator never returns block zero;
// block one is the first allocatable block.
//
// # Copy-on-write retirement
//
// Page lifetime is owned by the caller, not the allocator. Retire records a
// replaced extent; it is not reused until Reclaim frees every retired extent.
// The caller must complete its own reader grace period before Reclaim (for
// example storage.ReaderTracker.WaitForPriorReaders), because the allocator has
// no knowledge of readers. Reclaim returns extents to the free bitmap, where
// they coalesce implicitly with adjacent free blocks. Retired extents are
// runtime state and are never persisted: after a reopen they are all free.
//
// # Persistence
//
// Encode serializes the arena size, free count, allocation cursor, and free
// bitmap with a magic, version, and CRC32; Decode validates and restores it.
// Allocation state is implicit: a block is either free (a set bit) or live
// (referenced by the caller's persisted root). Retired extents are deliberately
// not persisted.
package pagealloc

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"math/bits"
)

const (
	// MinBlockSize is the allocation granularity in bytes.
	MinBlockSize uint32 = 512

	// metadataMagic identifies a serialized allocator metadata blob.
	metadataMagic uint32 = 0x50414c42 // "PALB"
	// metadataVersion is the current metadata format version.
	metadataVersion uint16 = 2
	// metadataHeaderSize is magic (4) + version (2) + reserved (2) +
	// total bytes (8) + free count (8) + cursor (8) + word count (8).
	metadataHeaderSize = 40
)

// Sentinel errors. They are stable so callers can map allocation failures onto
// their own durability and capacity errors.
var (
	// ErrTooLarge reports an allocation request larger than the arena.
	ErrTooLarge = errors.New("pagealloc: allocation exceeds arena")
	// ErrOutOfSpace reports a request that no free run can satisfy.
	ErrOutOfSpace = errors.New("pagealloc: out of space")
	// ErrInvalidRef reports a reference outside the arena or a size that does
	// not match the extent.
	ErrInvalidRef = errors.New("pagealloc: invalid reference")
	// ErrCorrupt reports metadata that fails magic, version, length, or CRC
	// validation, or bitmap state that is internally inconsistent.
	ErrCorrupt = errors.New("pagealloc: corrupt metadata")
)

// Ref identifies one allocated extent by its first 512-byte block address. Ref
// zero is the null reference; valid references start at one.
type Ref uint32

// retiredExtent is one replaced extent awaiting reclamation.
type retiredExtent struct {
	ref    Ref
	blocks uint32
}

// Allocator is a first-fit, 512-byte-granular allocator over a fixed-size byte
// arena. It is not safe for concurrent use; the owning index serializes all
// writes and performs reclamation after readers have drained.
type Allocator struct {
	totalBytes  uint64
	totalBlocks uint64
	free        []uint64
	freeCount   uint64
	cursor      uint64
	retired     []retiredExtent
	// allocated accumulates the capacity of every successful Alloc, so callers
	// can measure copy-on-write allocation volume independently of reclamation.
	allocated uint64
}

// New returns an allocator that manages arenaBytes of address space. The size
// is rounded down to a whole number of MinBlockSize blocks. arenaBytes smaller
// than two blocks yields ErrTooLarge.
func New(arenaBytes uint64) (*Allocator, error) {
	totalBlocks := arenaBytes / uint64(MinBlockSize)
	if totalBlocks < 2 {
		return nil, fmt.Errorf("%w: arena of %d bytes", ErrTooLarge, arenaBytes)
	}
	allocator := &Allocator{
		totalBytes:  totalBlocks * uint64(MinBlockSize),
		totalBlocks: totalBlocks,
		free:        make([]uint64, (totalBlocks+63)/64),
	}
	// Block zero is reserved; every other block starts free.
	for block := uint64(1); block < totalBlocks; block++ {
		allocator.free[block>>6] |= uint64(1) << (block & 63)
	}
	allocator.freeCount = totalBlocks - 1
	allocator.cursor = 1
	return allocator, nil
}

// TotalBytes returns the usable arena size after rounding to whole blocks.
func (a *Allocator) TotalBytes() uint64 { return a.totalBytes }

// TotalBlocks returns the number of 512-byte blocks in the arena.
func (a *Allocator) TotalBlocks() uint64 { return a.totalBlocks }

// blocksFor rounds size bytes up to a whole number of blocks, minimum one.
func blocksFor(size uint32) uint32 {
	if size == 0 {
		return 1
	}
	return (size + MinBlockSize - 1) / MinBlockSize
}

// Alloc returns the lowest-address free run that holds size bytes. It reports
// the extent reference and its actual byte capacity (a multiple of
// MinBlockSize, not necessarily a power of two).
func (a *Allocator) Alloc(size uint32) (Ref, uint32, error) {
	if a == nil {
		return 0, 0, ErrOutOfSpace
	}
	blocks := blocksFor(size)
	if uint64(blocks) > a.totalBlocks {
		return 0, 0, ErrTooLarge
	}
	start, ok := a.findRun(blocks)
	if !ok {
		return 0, 0, ErrOutOfSpace
	}
	a.clearRun(start, uint64(blocks))
	a.freeCount -= uint64(blocks)
	a.cursor = start + uint64(blocks)
	if a.cursor >= a.totalBlocks {
		a.cursor = 1
	}
	capacity := blocks * MinBlockSize
	a.allocated += uint64(capacity)
	return Ref(start), capacity, nil
}

// AllocatedBytes returns the cumulative capacity handed out by Alloc since the
// allocator was created or decoded. It measures copy-on-write allocation volume,
// not physical disk writes, and is not persisted.
func (a *Allocator) AllocatedBytes() uint64 {
	if a == nil {
		return 0
	}
	return a.allocated
}

// Free immediately returns an extent to the free bitmap without retirement.
// It is intended for rollback of an extent that was allocated but never
// published. Published extents must go through Retire instead.
func (a *Allocator) Free(ref Ref, size uint32) error {
	blocks, err := a.validate(ref, size)
	if err != nil {
		return err
	}
	if a.anyFree(uint64(ref), uint64(blocks)) {
		return fmt.Errorf("%w: extent %d is already free", ErrInvalidRef, ref)
	}
	a.setRun(uint64(ref), uint64(blocks))
	a.freeCount += uint64(blocks)
	if uint64(ref) < a.cursor {
		a.cursor = uint64(ref)
	}
	return nil
}

// Reserve marks an already-allocated extent as used without going through
// Alloc. It is used on reopen to reconstruct allocation state by walking the
// persisted tree, and by index headers that pin fixed arena regions. It fails
// if any block in the run is already used, which also detects overlapping
// references in corrupt metadata.
func (a *Allocator) Reserve(ref Ref, size uint32) error {
	blocks, err := a.validate(ref, size)
	if err != nil {
		return err
	}
	if !a.allFree(uint64(ref), uint64(blocks)) {
		return fmt.Errorf("%w: extent %d overlaps a used block", ErrInvalidRef, ref)
	}
	a.clearRun(uint64(ref), uint64(blocks))
	a.freeCount -= uint64(blocks)
	return nil
}

// Retire records the extent at ref as replaced. size must be the capacity
// returned by Alloc. The extent is not reused until Reclaim runs, and the
// caller must complete its reader grace period before calling Reclaim.
func (a *Allocator) Retire(ref Ref, size uint32) error {
	blocks, err := a.validate(ref, size)
	if err != nil {
		return err
	}
	a.retired = append(a.retired, retiredExtent{ref: ref, blocks: blocks})
	return nil
}

// Reclaim frees every retired extent. The allocator has no knowledge of
// readers, so the caller must have completed its reader grace period (for
// example storage.ReaderTracker.WaitForPriorReaders) before calling Reclaim.
func (a *Allocator) Reclaim() error {
	if a == nil {
		return nil
	}
	for _, extent := range a.retired {
		ref := uint64(extent.ref)
		a.setRun(ref, uint64(extent.blocks))
		a.freeCount += uint64(extent.blocks)
		if ref < a.cursor {
			a.cursor = ref
		}
	}
	a.retired = a.retired[:0]
	return nil
}

// Retired returns the number of extents awaiting reclamation.
func (a *Allocator) Retired() int { return len(a.retired) }

// RetiredBytes returns the total retired capacity in bytes.
func (a *Allocator) RetiredBytes() uint64 {
	var total uint64
	for _, extent := range a.retired {
		total += uint64(extent.blocks) * uint64(MinBlockSize)
	}
	return total
}

// LargestFreeBytes returns the size of the largest contiguous free run. It is a
// fragmentation metric for callers, not a hot-path operation.
func (a *Allocator) LargestFreeBytes() uint64 {
	if a == nil {
		return 0
	}
	var best, run uint64
	for block := uint64(1); block < a.totalBlocks; block++ {
		if a.isFree(block) {
			run++
			if run > best {
				best = run
			}
		} else {
			run = 0
		}
	}
	return best * uint64(MinBlockSize)
}

// FreeRunCount returns the number of contiguous free runs, the denominator of a
// fragmentation ratio. It is a diagnostics helper, not a hot path.
func (a *Allocator) FreeRunCount() uint64 {
	if a == nil {
		return 0
	}
	var runs uint64
	prevFree := false
	for block := uint64(1); block < a.totalBlocks; block++ {
		free := a.isFree(block)
		if free && !prevFree {
			runs++
		}
		prevFree = free
	}
	return runs
}

// FreeBytes returns the total free capacity in bytes.
func (a *Allocator) FreeBytes() uint64 {
	if a == nil {
		return 0
	}
	return a.freeCount * uint64(MinBlockSize)
}

// UsedBytes returns the allocated (free minus retired) capacity in bytes.
func (a *Allocator) UsedBytes() uint64 {
	return a.totalBytes - a.FreeBytes()
}

// validate checks a reference and its block run, returning the block count.
func (a *Allocator) validate(ref Ref, size uint32) (uint32, error) {
	address := uint64(ref)
	if address == 0 || address >= a.totalBlocks {
		return 0, fmt.Errorf("%w: block %d", ErrInvalidRef, address)
	}
	blocks := blocksFor(size)
	if address+uint64(blocks) > a.totalBlocks {
		return 0, fmt.Errorf("%w: block %d size %d", ErrInvalidRef, address, size)
	}
	return blocks, nil
}

// isFree reports whether the block is free.
func (a *Allocator) isFree(block uint64) bool {
	if block >= a.totalBlocks {
		return false
	}
	return a.free[block>>6]&(uint64(1)<<(block&63)) != 0
}

// anyFree reports whether any block in [start, start+count) is free.
func (a *Allocator) anyFree(start, count uint64) bool {
	for i := uint64(0); i < count; i++ {
		if a.isFree(start + i) {
			return true
		}
	}
	return false
}

// allFree reports whether every block in [start, start+count) is free.
func (a *Allocator) allFree(start, count uint64) bool {
	for i := uint64(0); i < count; i++ {
		if !a.isFree(start + i) {
			return false
		}
	}
	return true
}

// clearRun marks [start, start+count) allocated.
func (a *Allocator) clearRun(start, count uint64) {
	for i := uint64(0); i < count; i++ {
		block := start + i
		a.free[block>>6] &^= uint64(1) << (block & 63)
	}
}

// setRun marks [start, start+count) free.
func (a *Allocator) setRun(start, count uint64) {
	for i := uint64(0); i < count; i++ {
		block := start + i
		a.free[block>>6] |= uint64(1) << (block & 63)
	}
}

// nextFree returns the first free block at or after from, or totalBlocks.
func (a *Allocator) nextFree(from uint64) uint64 {
	if from >= a.totalBlocks {
		return a.totalBlocks
	}
	wordIndex := from >> 6
	mask := ^uint64(0) << (from & 63)
	for {
		word := a.free[wordIndex] & mask
		if word != 0 {
			return wordIndex*64 + uint64(bits.TrailingZeros64(word))
		}
		wordIndex++
		if wordIndex >= uint64(len(a.free)) {
			return a.totalBlocks
		}
		mask = ^uint64(0)
	}
}

// findRun returns the start of the first free run of blocks blocks, searching
// forward from the cursor and then wrapping once.
func (a *Allocator) findRun(blocks uint32) (uint64, bool) {
	if uint64(blocks) > a.freeCount {
		return 0, false
	}
	if start, ok := a.scanRun(a.cursor, a.totalBlocks, blocks); ok {
		return start, true
	}
	if a.cursor > 1 {
		return a.scanRun(1, a.cursor, blocks)
	}
	return 0, false
}

// scanRun searches [from, to) for a free run of the requested length.
func (a *Allocator) scanRun(from, to uint64, blocks uint32) (uint64, bool) {
	block := a.nextFree(from)
	for block < to {
		if block+uint64(blocks) > to {
			return 0, false
		}
		run := uint64(0)
		for run < uint64(blocks) && a.isFree(block+run) {
			run++
		}
		if run == uint64(blocks) {
			return block, true
		}
		block = a.nextFree(block + run)
	}
	return 0, false
}

// Validate checks that the free bitmap is internally consistent. It is
// intended for tests and corruption diagnostics, not the hot path.
func (a *Allocator) Validate() error {
	if a == nil {
		return ErrCorrupt
	}
	if a.totalBlocks == 0 || int((a.totalBlocks+63)/64) != len(a.free) {
		return fmt.Errorf("%w: bitmap size", ErrCorrupt)
	}
	var count uint64
	for i, word := range a.free {
		if i == len(a.free)-1 && a.totalBlocks&63 != 0 {
			if word>>(a.totalBlocks&63) != 0 {
				return fmt.Errorf("%w: free bits past the arena", ErrCorrupt)
			}
		}
		count += uint64(bits.OnesCount64(word))
	}
	if count != a.freeCount {
		return fmt.Errorf("%w: free count %d, bitmap counts %d", ErrCorrupt, a.freeCount, count)
	}
	if a.isFree(0) {
		return fmt.Errorf("%w: reserved block zero is free", ErrCorrupt)
	}
	return nil
}

// Encode serializes the arena size, free count, cursor, and free bitmap. The
// result carries magic, version, and a trailing CRC32/IEEE. Retired extents are
// not serialized.
func (a *Allocator) Encode() []byte {
	if a == nil {
		return nil
	}
	words := uint64(len(a.free))
	size := metadataHeaderSize + int(words)*8 + 4
	out := make([]byte, size)
	binary.LittleEndian.PutUint32(out[0:4], metadataMagic)
	binary.LittleEndian.PutUint16(out[4:6], metadataVersion)
	binary.LittleEndian.PutUint64(out[8:16], a.totalBytes)
	binary.LittleEndian.PutUint64(out[16:24], a.freeCount)
	binary.LittleEndian.PutUint64(out[24:32], a.cursor)
	binary.LittleEndian.PutUint64(out[32:40], words)
	offset := metadataHeaderSize
	for _, word := range a.free {
		binary.LittleEndian.PutUint64(out[offset:offset+8], word)
		offset += 8
	}
	binary.LittleEndian.PutUint32(out[offset:offset+4], crc32IEEE(out[:offset]))
	return out
}

// Decode restores an allocator from Encode output. A recognized magic with an
// unknown version, a length that does not match the arena, a free count that
// disagrees with the bitmap, or a failed CRC all report ErrCorrupt.
func Decode(data []byte) (*Allocator, error) {
	if len(data) < metadataHeaderSize+4 {
		return nil, fmt.Errorf("%w: short metadata", ErrCorrupt)
	}
	if binary.LittleEndian.Uint32(data[0:4]) != metadataMagic {
		return nil, fmt.Errorf("%w: bad magic", ErrCorrupt)
	}
	if version := binary.LittleEndian.Uint16(data[4:6]); version != metadataVersion {
		return nil, fmt.Errorf("%w: version %d", ErrCorrupt, version)
	}
	if got := crc32IEEE(data[:len(data)-4]); got != binary.LittleEndian.Uint32(data[len(data)-4:]) {
		return nil, fmt.Errorf("%w: checksum mismatch", ErrCorrupt)
	}
	totalBytes := binary.LittleEndian.Uint64(data[8:16])
	freeCount := binary.LittleEndian.Uint64(data[16:24])
	cursor := binary.LittleEndian.Uint64(data[24:32])
	words := binary.LittleEndian.Uint64(data[32:40])
	if totalBytes == 0 || totalBytes%uint64(MinBlockSize) != 0 {
		return nil, fmt.Errorf("%w: arena bytes %d", ErrCorrupt, totalBytes)
	}
	totalBlocks := totalBytes / uint64(MinBlockSize)
	if words != (totalBlocks+63)/64 {
		return nil, fmt.Errorf("%w: word count %d", ErrCorrupt, words)
	}
	if want := metadataHeaderSize + int(words)*8 + 4; len(data) != want {
		return nil, fmt.Errorf("%w: metadata is %d bytes, want %d", ErrCorrupt, len(data), want)
	}
	allocator := &Allocator{
		totalBytes:  totalBytes,
		totalBlocks: totalBlocks,
		free:        make([]uint64, words),
		freeCount:   freeCount,
		cursor:      cursor,
	}
	offset := metadataHeaderSize
	for i := range allocator.free {
		allocator.free[i] = binary.LittleEndian.Uint64(data[offset : offset+8])
		offset += 8
	}
	if allocator.cursor == 0 || allocator.cursor >= totalBlocks {
		allocator.cursor = 1
	}
	if err := allocator.Validate(); err != nil {
		return nil, err
	}
	return allocator, nil
}

// crc32IEEE computes the checksum shared by every persisted index format.
func crc32IEEE(data []byte) uint32 { return crc32.ChecksumIEEE(data) }
