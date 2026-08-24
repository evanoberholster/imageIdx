package roar

import (
	"encoding/binary"
	"slices"
	"unsafe"
)

// Builder accumulates result containers in key order for materializing
// operations such as AndInto. It is not safe for concurrent use.
//
// Scratch storage belongs to the Builder: an 8 KiB word buffer for dense
// operations and a value buffer for sparse merging. Operations reuse these
// buffers, so a caller performing one operation at a time sees no
// per-operation scratch allocation after warmup.
type Builder struct {
	keys    []uint64
	offsets []uint64
	data    []byte

	scratchWords  []uint64
	scratchValues []uint16
}

// NewBuilder returns a Builder with room for capacity containers.
func NewBuilder(capacity int) *Builder {
	if capacity < 0 {
		capacity = 0
	}
	return &Builder{
		keys:          make([]uint64, 0, capacity),
		offsets:       make([]uint64, 0, capacity),
		scratchWords:  make([]uint64, BitmapWords),
		scratchValues: make([]uint16, 0, 2*ArrayToBitmapThreshold),
	}
}

// Reset clears accumulated containers and scratch state while retaining
// allocated capacity.
func (b *Builder) Reset() {
	b.keys = b.keys[:0]
	b.offsets = b.offsets[:0]
	b.data = b.data[:0]
	clear(b.scratchWords)
	b.scratchValues = b.scratchValues[:0]
}

// KeyCount returns the number of containers accumulated so far.
func (b *Builder) KeyCount() int { return len(b.keys) }

// extend grows the container region by size zeroed bytes and returns the
// new region.
func (b *Builder) extend(size int) []byte {
	start := len(b.data)
	b.data = slices.Grow(b.data, size)[:start+size]
	clear(b.data[start:])
	return b.data[start:]
}

func (b *Builder) appendArray(key uint64, values []uint16) {
	if len(values) == 0 {
		return
	}
	blocks := uint16((ContainerHeaderSize + 2*len(values) + 7) / 8)
	start := len(b.data)
	region := b.extend(int(blocks) * 8)
	writeContainerHeader(region, blocks, ContainerArray, uint32(len(values)))
	for index, value := range values {
		binary.LittleEndian.PutUint16(region[ContainerHeaderSize+2*index:], value)
	}
	b.keys = append(b.keys, key)
	b.offsets = append(b.offsets, uint64(start))
}

func (b *Builder) appendBitmap(key uint64, words []uint64, card uint32) {
	if card == 0 {
		return
	}
	blocks := uint16(1 + BitmapWords)
	start := len(b.data)
	region := b.extend(int(blocks) * 8)
	writeContainerHeader(region, blocks, ContainerBitmap, card)
	dst := unsafe.Slice((*uint64)(unsafe.Pointer(unsafe.SliceData(region[ContainerHeaderSize:]))), BitmapWords)
	copy(dst, words[:BitmapWords])
	b.keys = append(b.keys, key)
	b.offsets = append(b.offsets, uint64(start))
}

// emitWords selects an array or bitmap container for a dense result with
// the given cardinality and appends it. Dense results at or below the
// array-to-bitmap threshold are persisted sparsely.
func (b *Builder) emitWords(key uint64, card uint32) {
	if card == 0 {
		return
	}
	if card <= ArrayToBitmapThreshold {
		values := extractWords(b.scratchValues[:0], b.scratchWords)
		b.appendArray(key, values)
		return
	}
	b.appendBitmap(key, b.scratchWords, card)
}

// emitValues selects an array or bitmap container for a sparse union result
// and appends it.
func (b *Builder) emitValues(key uint64, values []uint16) {
	if len(values) == 0 {
		return
	}
	if len(values) <= ArrayToBitmapThreshold {
		b.appendArray(key, values)
		return
	}
	clear(b.scratchWords)
	card := uint32(len(values))
	for _, value := range values {
		b.scratchWords[value>>6] |= uint64(1) << (value & 63)
	}
	b.appendBitmap(key, b.scratchWords, card)
}

func writeContainerHeader(dst []byte, blocks uint16, containerType ContainerType, card uint32) {
	binary.LittleEndian.PutUint16(dst[0:2], blocks)
	binary.LittleEndian.PutUint16(dst[2:4], uint16(containerType))
	binary.LittleEndian.PutUint32(dst[4:8], card)
}
