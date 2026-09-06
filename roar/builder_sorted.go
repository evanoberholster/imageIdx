package roar

import (
	"encoding/binary"
	"slices"
	"unsafe"
)

// SortedBuilder constructs a bitmap from strictly increasing uint64 IDs.
// It is not safe for concurrent use.
type SortedBuilder struct {
	keys    []uint64
	offsets []uint64
	data    []byte

	currentKey    uint64
	currentValues []uint16
	currentBitmap []uint64
	bitmapCard    uint32
	isBitmap      bool
	hasCurrent    bool
	finished      bool
	last          uint64
}

// NewSortedBuilder returns a builder with room for capacity containers.
func NewSortedBuilder(capacity int) *SortedBuilder {
	if capacity < 0 {
		capacity = 0
	}
	return &SortedBuilder{
		keys:    make([]uint64, 0, capacity),
		offsets: make([]uint64, 0, capacity),
	}
}

// Reset clears the builder while retaining allocated capacity. It must be
// called before adding IDs after Finish.
func (b *SortedBuilder) Reset() {
	b.keys = b.keys[:0]
	b.offsets = b.offsets[:0]
	b.data = b.data[:0]
	b.currentValues = b.currentValues[:0]
	if b.isBitmap {
		clear(b.currentBitmap)
	}
	b.bitmapCard = 0
	b.isBitmap = false
	b.hasCurrent = false
	b.finished = false
	b.last = 0
}

// Add appends one ImageID. IDs must be strictly increasing; zero is
// rejected. After Finish, Add returns ErrSortedBuilderFinished until Reset
// is called.
func (b *SortedBuilder) Add(id uint64) error {
	if b.finished {
		return ErrSortedBuilderFinished
	}
	if id == InvalidImageID {
		return ErrInvalidImageID
	}
	if b.hasCurrent && id <= b.last {
		return ErrUnsortedIDs
	}
	b.last = id
	key := highKey(id)
	if !b.hasCurrent || key != b.currentKey {
		b.finishCurrent()
		b.currentKey = key
		b.hasCurrent = true
	}
	b.addValue(lowValue(id))
	return nil
}

func (b *SortedBuilder) addValue(value uint16) {
	if b.isBitmap {
		word, bit := value>>6, value&63
		mask := uint64(1) << bit
		if b.currentBitmap[word]&mask == 0 {
			b.currentBitmap[word] |= mask
			b.bitmapCard++
		}
		return
	}
	b.currentValues = append(b.currentValues, value)
	if len(b.currentValues) > ArrayToBitmapThreshold {
		if len(b.currentBitmap) == 0 {
			b.currentBitmap = make([]uint64, BitmapWords)
		}
		b.bitmapCard = uint32(len(b.currentValues))
		for _, current := range b.currentValues {
			b.currentBitmap[current>>6] |= uint64(1) << (current & 63)
		}
		b.currentValues = b.currentValues[:0]
		b.isBitmap = true
	}
}

func (b *SortedBuilder) finishCurrent() {
	if !b.hasCurrent {
		return
	}
	if b.isBitmap {
		blocks := uint16(1 + BitmapWords)
		start := len(b.data)
		region := b.extend(int(blocks) * 8)
		writeContainerHeader(region, blocks, ContainerBitmap, b.bitmapCard)
		words := unsafe.Slice((*uint64)(unsafe.Pointer(unsafe.SliceData(region[ContainerHeaderSize:]))), BitmapWords)
		copy(words, b.currentBitmap)
		clear(b.currentBitmap)
		b.keys = append(b.keys, b.currentKey)
		b.offsets = append(b.offsets, uint64(start))
		b.bitmapCard = 0
		b.isBitmap = false
	} else {
		values := b.currentValues
		blocks := uint16((ContainerHeaderSize + 2*len(values) + 7) / 8)
		start := len(b.data)
		region := b.extend(int(blocks) * 8)
		writeContainerHeader(region, blocks, ContainerArray, uint32(len(values)))
		for index, value := range values {
			binary.LittleEndian.PutUint16(region[ContainerHeaderSize+2*index:], value)
		}
		b.keys = append(b.keys, b.currentKey)
		b.offsets = append(b.offsets, uint64(start))
		b.currentValues = b.currentValues[:0]
	}
	b.hasCurrent = false
}

// Finish assembles the serialized bitmap. Finish may be called repeatedly,
// but Reset must be called before adding more IDs.
func (b *SortedBuilder) Finish() ([]byte, error) {
	b.finishCurrent()
	b.finished = true
	return assemble(b.keys, b.offsets, b.data), nil
}

func (b *SortedBuilder) extend(size int) []byte {
	start := len(b.data)
	b.data = slices.Grow(b.data, size)[:start+size]
	clear(b.data[start:])
	return b.data[start:]
}

// FromSortedList constructs an immutable Bitmap from strictly increasing
// ImageIDs. ImageID zero and duplicate IDs are rejected.
func FromSortedList(ids []uint64) (Bitmap, error) {
	data, err := FromSorted(ids)
	if err != nil {
		return Bitmap{}, err
	}
	return Bitmap{data: data}, nil
}

// FromSorted constructs the compact serialized representation of strictly
// increasing ImageIDs. ImageID zero and duplicate IDs are rejected. Use
// FromSortedList when an immediately queryable Bitmap is wanted.
//
// Construction is two-pass: the first pass validates the input and computes
// the exact layout, and the second writes into one preallocated buffer. It
// allocates the result once and never relocates containers.
func FromSorted(ids []uint64) ([]byte, error) {
	keyCount, totalBlocks, err := scanLayout(ids)
	if err != nil {
		return nil, err
	}
	containerBase := HeaderSize + keyCount*KeyEntrySize
	out := make([]byte, containerBase+totalBlocks*8)
	requireAligned(out)
	binary.LittleEndian.PutUint32(out[0:4], Magic)
	binary.LittleEndian.PutUint16(out[4:6], Version)
	binary.LittleEndian.PutUint16(out[6:8], FlagCompact)
	binary.LittleEndian.PutUint32(out[8:12], HeaderSize)
	binary.LittleEndian.PutUint32(out[12:16], uint32(keyCount))

	keyIndex := 0
	offset := containerBase
	start := 0
	for start < len(ids) {
		key := highKey(ids[start])
		end := start + 1
		for end < len(ids) && highKey(ids[end]) == key {
			end++
		}
		run := containerRun{key: key, start: start, end: end}
		entry := HeaderSize + keyIndex*KeyEntrySize
		binary.LittleEndian.PutUint64(out[entry:entry+8], key)
		binary.LittleEndian.PutUint64(out[entry+8:entry+16], uint64(offset))
		offset += writeContainer(out[offset:], ids, run) * 8
		keyIndex++
		start = end
	}
	return out, nil
}

// containerRun describes one key's value range within the input slice.
type containerRun struct {
	key   uint64
	start int
	end   int
}

// scanLayout validates ids and returns the key count and total 8-byte
// blocks required for all containers.
func scanLayout(ids []uint64) (keys int, blocks int, err error) {
	if len(ids) == 0 {
		return 0, 0, nil
	}
	var currentKey uint64
	card := 0
	for index, id := range ids {
		if id == InvalidImageID {
			return 0, 0, ErrInvalidImageID
		}
		if index != 0 && id <= ids[index-1] {
			return 0, 0, ErrUnsortedIDs
		}
		key := highKey(id)
		if index == 0 || key != currentKey {
			if index != 0 {
				keys++
				blocks += blocksFor(card)
			}
			currentKey = key
			card = 0
		}
		card++
	}
	keys++
	blocks += blocksFor(card)
	return keys, blocks, nil
}

func blocksFor(card int) int {
	if card > ArrayToBitmapThreshold {
		return int(1 + BitmapWords)
	}
	return (ContainerHeaderSize + 2*card + 7) / 8
}

// writeContainer writes one container into dst and returns its size in
// 8-byte blocks. dst is zero-filled by the caller.
func writeContainer(dst []byte, ids []uint64, run containerRun) int {
	card := run.end - run.start
	if card > ArrayToBitmapThreshold {
		blocks := int(1 + BitmapWords)
		writeContainerHeader(dst, uint16(blocks), ContainerBitmap, uint32(card))
		words := unsafe.Slice((*uint64)(unsafe.Pointer(unsafe.SliceData(dst[ContainerHeaderSize:]))), BitmapWords)
		for index := run.start; index < run.end; index++ {
			value := lowValue(ids[index])
			words[value>>6] |= uint64(1) << (value & 63)
		}
		return blocks
	}
	blocks := (ContainerHeaderSize + 2*card + 7) / 8
	writeContainerHeader(dst, uint16(blocks), ContainerArray, uint32(card))
	position := ContainerHeaderSize
	for index := run.start; index < run.end; index++ {
		binary.LittleEndian.PutUint16(dst[position:], lowValue(ids[index]))
		position += 2
	}
	return blocks
}
