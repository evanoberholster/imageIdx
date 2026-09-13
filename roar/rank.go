package roar

import (
	"math/bits"

	"github.com/evanoberholster/imageIdx/roar/internal/bitmapkernel"
)

// Rank returns the number of IDs less than or equal to x. It allocates
// nothing and is O(containers <= highKey(x)).
func (b Bitmap) Rank(x uint64) uint64 {
	if x == InvalidImageID {
		return 0
	}
	key := highKey(x)
	low := lowValue(x)
	var total uint64
	for _, entry := range b.keyEntries() {
		if entry.Key > key {
			break
		}
		container := b.data[entry.Offset:]
		header := containerHeaderAt(container)
		if entry.Key < key {
			total += uint64(header.Card)
			continue
		}
		if ContainerType(header.Type) == ContainerArray {
			total += uint64(arrayUpperBound(arrayValuesAt(container), low))
		} else {
			total += bitmapRank(bitmapWordsAt(container), low)
		}
	}
	return total
}

// Select returns the x-th smallest ID (zero-indexed). It walks the key
// table linearly, so it is O(containers). It returns
// ErrSelectOutOfRange when x is not less than the cardinality.
func (b Bitmap) Select(x uint64) (uint64, error) {
	entries := b.keyEntries()
	if x >= b.Cardinality() {
		return 0, ErrSelectOutOfRange
	}
	for _, entry := range entries {
		container := b.data[entry.Offset:]
		header := containerHeaderAt(container)
		if x >= uint64(header.Card) {
			x -= uint64(header.Card)
			continue
		}
		if ContainerType(header.Type) == ContainerArray {
			return entry.Key | uint64(arrayValuesAt(container)[x]), nil
		}
		return entry.Key | uint64(bitmapSelect(bitmapWordsAt(container), x)), nil
	}
	return 0, ErrSelectOutOfRange
}

// Minimum returns the smallest ID, or ok=false when the bitmap is empty.
func (b Bitmap) Minimum() (uint64, bool) {
	entries := b.keyEntries()
	if len(entries) == 0 {
		return 0, false
	}
	entry := entries[0]
	return entry.Key | uint64(containerMin(b.data[entry.Offset:])), true
}

// Maximum returns the largest ID, or ok=false when the bitmap is empty.
func (b Bitmap) Maximum() (uint64, bool) {
	entries := b.keyEntries()
	if len(entries) == 0 {
		return 0, false
	}
	entry := entries[len(entries)-1]
	return entry.Key | uint64(containerMax(b.data[entry.Offset:])), true
}

// arrayUpperBound returns the count of values <= x in a sorted array. Like
// arrayContains it is a manual loop to stay inlinable.
func arrayUpperBound(values []uint16, x uint16) int {
	lo, hi := 0, len(values)
	for lo < hi {
		mid := int(uint(lo+hi) >> 1)
		if values[mid] <= x {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	return lo
}

// arrayLowerBound returns the index of the first value >= x.
func arrayLowerBound(values []uint16, x uint16) int {
	lo, hi := 0, len(values)
	for lo < hi {
		mid := int(uint(lo+hi) >> 1)
		if values[mid] < x {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	return lo
}

// bitmapRank counts the set bits in words with value <= x.
func bitmapRank(words []uint64, x uint16) uint64 {
	fullWords := int(x >> 6)
	count := bitmapkernel.CardinalityPrefix(words, fullWords)
	mask := ^uint64(0) >> (63 - (x & 63))
	count += uint64(bits.OnesCount64(words[fullWords] & mask))
	return count
}

// bitmapSelect returns the x-th (zero-indexed) set value in words.
func bitmapSelect(words []uint64, x uint64) uint16 {
	for wordIndex, word := range words {
		c := uint64(bits.OnesCount64(word))
		if x >= c {
			x -= c
			continue
		}
		for x > 0 {
			word &= word - 1
			x--
		}
		return uint16(wordIndex<<6) | uint16(bits.TrailingZeros64(word))
	}
	return 0
}

// containerMin returns the smallest value stored in a container.
func containerMin(container []byte) uint16 {
	header := containerHeaderAt(container)
	if ContainerType(header.Type) == ContainerArray {
		return arrayValuesAt(container)[0]
	}
	for wordIndex, word := range bitmapWordsAt(container) {
		if word != 0 {
			return uint16(wordIndex<<6) | uint16(bits.TrailingZeros64(word))
		}
	}
	return 0
}

// containerMax returns the largest value stored in a container.
func containerMax(container []byte) uint16 {
	header := containerHeaderAt(container)
	if ContainerType(header.Type) == ContainerArray {
		values := arrayValuesAt(container)
		return values[len(values)-1]
	}
	words := bitmapWordsAt(container)
	for wordIndex := len(words) - 1; wordIndex >= 0; wordIndex-- {
		if word := words[wordIndex]; word != 0 {
			return uint16(wordIndex<<6) | uint16(63-bits.LeadingZeros64(word))
		}
	}
	return 0
}
