package roar

import (
	"encoding/binary"
	"slices"
	"unsafe"
)

// valueScratchCap is the number of uint16 values the Builder stores inline
// for sparse merging before falling back to a heap slice. Most array
// intersections and unions produce small results, so the common case avoids
// allocating.
const valueScratchCap = 64

// Builder accumulates result containers in key order for materializing
// operations such as AndInto. It is not safe for concurrent use.
//
// Scratch storage belongs to the Builder. The dense word buffer is allocated
// lazily on first use, and the sparse value buffer starts as a small inline
// array, so array-only operations do not pay for the 8 KiB word buffer. The
// merge helpers write results into scratchValues in place (via *[]uint16), so
// its growth is retained across containers; a caller performing one operation
// at a time sees no per-operation scratch allocation after warmup.
type Builder struct {
	keys        []uint64
	offsets     []uint64
	data        []byte
	cardinality uint64

	scratchWords  []uint64
	scratchValues []uint16
	valuesBuf     [valueScratchCap]uint16
}

// NewBuilder returns a Builder with room for capacity containers.
func NewBuilder(capacity int) *Builder {
	if capacity < 0 {
		capacity = 0
	}
	b := &Builder{
		keys:    make([]uint64, 0, capacity),
		offsets: make([]uint64, 0, capacity),
	}
	b.scratchValues = b.valuesBuf[:0]
	return b
}

// wordsScratch returns the dense word buffer, allocating it on first use.
func (b *Builder) wordsScratch() []uint64 {
	if b.scratchWords == nil {
		b.scratchWords = make([]uint64, BitmapWords)
	}
	return b.scratchWords
}

// Reset clears accumulated containers while retaining allocated capacity.
// Scratch storage is reinitialized by the operation that next uses it.
func (b *Builder) Reset() {
	b.keys = b.keys[:0]
	b.offsets = b.offsets[:0]
	b.data = b.data[:0]
	b.cardinality = 0
	b.scratchValues = b.scratchValues[:0]
}

// KeyCount returns the number of containers accumulated so far.
func (b *Builder) KeyCount() int { return len(b.keys) }

// reserveData ensures the container region can hold at least n more bytes
// without reallocation. Callers that know an upper bound on the result size
// use it to avoid the geometric reallocation that appending otherwise incurs.
func (b *Builder) reserveData(n int) {
	if n > cap(b.data) {
		b.data = make([]byte, 0, n)
	}
}

// reserveKeys ensures the key and offset slices can hold at least n entries
// without reallocation.
func (b *Builder) reserveKeys(n int) {
	if n > cap(b.keys) {
		b.keys = make([]uint64, 0, n)
		b.offsets = make([]uint64, 0, n)
	}
}

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
	b.cardinality += uint64(len(values))
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
	b.cardinality += uint64(card)
}

// emitWords selects an array or bitmap container for a dense result with
// the given cardinality and appends it. Dense results at or below the
// array-to-bitmap threshold are persisted sparsely.
func (b *Builder) emitWords(key uint64, card uint32) {
	if card == 0 {
		return
	}
	if card <= ArrayToBitmapThreshold {
		extractWords(&b.scratchValues, b.scratchWords)
		b.appendArray(key, b.scratchValues)
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
	words := b.wordsScratch()
	clear(words)
	card := uint32(len(values))
	for _, value := range values {
		words[value>>6] |= uint64(1) << (value & 63)
	}
	b.appendBitmap(key, words, card)
}

func writeContainerHeader(dst []byte, blocks uint16, containerType ContainerType, card uint32) {
	binary.LittleEndian.PutUint16(dst[0:2], blocks)
	binary.LittleEndian.PutUint16(dst[2:4], uint16(containerType))
	binary.LittleEndian.PutUint32(dst[4:8], card)
}
