package roar

import (
	"math/bits"
	"unsafe"
)

// Iterator walks a bitmap's IDs in ascending order without materializing
// them. It allocates nothing. A zero Iterator reports false immediately.
//
// The iterator keeps two modes: card > 0 selects the sorted array payload
// and word selects the dense payload. Next emits from the current mode and
// falls through to advance when a mode is exhausted; advance moves to the
// next non-zero word or the next container.
type Iterator struct {
	data    []byte
	entries []keyEntry
	keyPos  int

	base    uint64
	card    int
	values  []uint16
	valueAt int
	words   []uint64
	wordAt  int
	word    uint64

	value uint64
}

// Iterator returns an iterator over the bitmap's ascending IDs. The
// returned iterator must not outlive the View's backing bytes. The first
// container is loaded lazily so the constructor stays trivial.
func (v View) Iterator() Iterator {
	return Iterator{data: v.data, entries: v.keyEntries(), keyPos: -1}
}

// advance moves to the next non-empty position: the next non-zero word of
// the current bitmap container, or the next container.
func (it *Iterator) advance() bool {
	for {
		if it.words != nil {
			for it.wordAt+1 < len(it.words) {
				it.wordAt++
				word := it.words[it.wordAt]
				if word != 0 {
					it.word = word
					return true
				}
			}
		}
		it.keyPos++
		if it.keyPos >= len(it.entries) {
			return false
		}
		entry := it.entries[it.keyPos]
		it.base = entry.Key
		container := it.data[entry.Offset:]
		h := (*containerHeader)(unsafe.Pointer(unsafe.SliceData(container)))
		switch ContainerType(h.Type) {
		case ContainerArray:
			card := int(h.Card)
			it.card = card
			it.values = unsafe.Slice(
				(*uint16)(unsafe.Pointer(unsafe.SliceData(container[ContainerHeaderSize:]))),
				card,
			)
			it.valueAt = 0
			it.words = nil
			it.word = 0
			return true
		case ContainerBitmap:
			it.card = 0
			it.values = nil
			it.words = unsafe.Slice(
				(*uint64)(unsafe.Pointer(unsafe.SliceData(container[ContainerHeaderSize:]))),
				BitmapWords,
			)
			it.wordAt = 0
			it.word = it.words[0]
			if it.word != 0 {
				return true
			}
			// fall through: advance to the first non-zero word
		}
	}
}

// Next advances to the following ID and reports whether one exists.
func (it *Iterator) Next() bool {
	for {
		if it.card != 0 {
			index := it.valueAt
			if index < it.card {
				it.value = it.base | uint64(it.values[index])
				it.valueAt = index + 1
				return true
			}
		} else if it.word != 0 {
			bit := bits.TrailingZeros64(it.word)
			it.word &= it.word - 1
			it.value = it.base | uint64(it.wordAt<<6) | uint64(bit)
			return true
		}
		if !it.advance() {
			return false
		}
	}
}

// Value returns the ID produced by the most recent successful Next call.
func (it *Iterator) Value() uint64 {
	return it.value
}

// AppendTo appends all IDs in ascending order to dst. The walk runs in one
// tight loop without per-value calls and allocates nothing; prefer it over
// Next/Value when materializing results.
func (v View) AppendTo(dst []uint64) []uint64 {
	for _, entry := range v.keyEntries() {
		container := v.data[entry.Offset:]
		h := (*containerHeader)(unsafe.Pointer(unsafe.SliceData(container)))
		switch ContainerType(h.Type) {
		case ContainerArray:
			values := unsafe.Slice(
				(*uint16)(unsafe.Pointer(unsafe.SliceData(container[ContainerHeaderSize:]))),
				int(h.Card),
			)
			for _, value := range values {
				dst = append(dst, entry.Key|uint64(value))
			}
		case ContainerBitmap:
			words := unsafe.Slice(
				(*uint64)(unsafe.Pointer(unsafe.SliceData(container[ContainerHeaderSize:]))),
				BitmapWords,
			)
			for wordIndex, word := range words {
				for word != 0 {
					bit := bits.TrailingZeros64(word)
					dst = append(dst, entry.Key|uint64(wordIndex<<6)|uint64(bit))
					word &= word - 1
				}
			}
		}
	}
	return dst
}
