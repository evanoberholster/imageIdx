package roar

import (
	"math/bits"
)

// Cursor accelerates membership probes and seeks against a fixed Bitmap when
// successive probes are non-decreasing (or at least have locality). It
// returns the same results as Contains for any order, and is cheapest when
// probes repeat or advance past the current container.
//
// A Cursor is not safe for concurrent use, and its Bitmap must not be mutated
// or freed while the cursor is live.
type Cursor struct {
	data    []byte
	entries []keyEntry

	keyPos int    // index of the loaded container, or -1
	base   uint64 // loaded key (low 16 bits zero)
	values []uint16
	words  []uint64
}

// NewCursor returns a cursor bound to b. It allocates nothing.
func (b Bitmap) NewCursor() Cursor {
	return Cursor{data: b.data, entries: b.keyEntries(), keyPos: -1}
}

// load populates the cursor's container cache for entries[index].
func (c *Cursor) load(index int) {
	entry := c.entries[index]
	c.keyPos = index
	c.base = entry.Key
	container := c.data[entry.Offset:]
	header := containerHeaderAt(container)
	switch ContainerType(header.Type) {
	case ContainerArray:
		c.values = arrayValuesAt(container)
		c.words = nil
	case ContainerBitmap:
		c.values = nil
		c.words = bitmapWordsAt(container)
	}
}

// gallop finds the index of the first entry whose key is >= key, starting
// from c.keyPos for forward probes. It falls back to binary search on
// backward moves.
func (c *Cursor) gallop(key uint64) int {
	if c.keyPos >= 0 && key >= c.entries[c.keyPos].Key {
		// Forward: exponential then binary within [keyPos, n).
		index := c.keyPos
		if c.entries[index].Key >= key {
			return index
		}
		step := 1
		for index+step < len(c.entries) && c.entries[index+step].Key < key {
			index += step
			step <<= 1
		}
		lo, hi := index, min(index+step, len(c.entries)-1)
		for lo < hi {
			mid := (lo + hi) / 2
			if c.entries[mid].Key < key {
				lo = mid + 1
			} else {
				hi = mid
			}
		}
		return lo
	}
	// Backward or fresh: binary search the whole table.
	index, _ := findKey(c.entries, key)
	return index
}

// Contains reports whether x is present. It is correct for any probe order.
func (c *Cursor) Contains(x uint64) bool {
	if x == InvalidImageID {
		return false
	}
	key := highKey(x)
	low := lowValue(x)
	if c.keyPos >= 0 && c.base == key {
		if c.values != nil {
			return arrayContains(c.values, low)
		}
		return bitmapContains(c.words, low)
	}
	index := c.gallop(key)
	if index >= len(c.entries) || c.entries[index].Key != key {
		return false
	}
	c.load(index)
	if c.values != nil {
		return arrayContains(c.values, low)
	}
	return bitmapContains(c.words, low)
}

// NextGeq returns the smallest ID greater than or equal to x. It is intended
// for non-decreasing probe sequences, where it advances in amortized O(1)
// work per element.
func (c *Cursor) NextGeq(x uint64) (uint64, bool) {
	key := highKey(x)
	low := lowValue(x)

	index := c.gallop(key)
	if index >= len(c.entries) {
		return 0, false
	}
	entry := c.entries[index]
	if entry.Key > key {
		c.load(index)
		return entry.Key | uint64(containerMin(c.data[entry.Offset:])), true
	}
	// entry.Key == key: seek within the container.
	c.load(index)
	container := c.data[entry.Offset:]
	if value, ok := containerNextGeq(container, low); ok {
		return key | uint64(value), true
	}
	// No element >= low in this container: fall through to the next one.
	if index+1 >= len(c.entries) {
		return 0, false
	}
	next := c.entries[index+1]
	c.load(index + 1)
	return next.Key | uint64(containerMin(c.data[next.Offset:])), true
}

// containerNextGeq returns the smallest stored value >= x in a container.
func containerNextGeq(container []byte, x uint16) (uint16, bool) {
	header := containerHeaderAt(container)
	if ContainerType(header.Type) == ContainerArray {
		values := arrayValuesAt(container)
		index := arrayLowerBound(values, x)
		if index < len(values) {
			return values[index], true
		}
		return 0, false
	}
	return bitmapNextSet(bitmapWordsAt(container), x)
}

// bitmapNextSet returns the smallest set bit >= x in words.
func bitmapNextSet(words []uint64, x uint16) (uint16, bool) {
	wordIndex := int(x >> 6)
	mask := ^uint64(0) << (x & 63)
	word := words[wordIndex] & mask
	for {
		if word != 0 {
			return uint16(wordIndex<<6) | uint16(bits.TrailingZeros64(word)), true
		}
		wordIndex++
		if wordIndex >= len(words) {
			return 0, false
		}
		word = words[wordIndex]
	}
}
