package roar

import "math/bits"

// AndInto appends the intersection of a and b to dst. The two key tables
// are merge-walked in order; only matching containers are combined.
func AndInto(dst *Builder, a, b Bitmap) {
	aEntries, bEntries := a.keyEntries(), b.keyEntries()
	i, j := 0, 0
	for i < len(aEntries) && j < len(bEntries) {
		switch {
		case aEntries[i].Key < bEntries[j].Key:
			i++
		case aEntries[i].Key > bEntries[j].Key:
			j++
		default:
			andContainerInto(dst, aEntries[i].Key, a.data[aEntries[i].Offset:], b.data[bEntries[j].Offset:])
			i++
			j++
		}
	}
}

func andContainerInto(dst *Builder, key uint64, a, b []byte) {
	ha, hb := containerHeaderAt(a), containerHeaderAt(b)
	switch {
	case ha.Type == uint16(ContainerArray) && hb.Type == uint16(ContainerArray):
		values := intersectArrays(dst.scratchValues[:0], arrayValuesAt(a), arrayValuesAt(b))
		dst.appendArray(key, values)
	case ha.Type == uint16(ContainerArray):
		values := filterArrayByBitmap(dst.scratchValues[:0], arrayValuesAt(a), bitmapWordsAt(b))
		dst.appendArray(key, values)
	case hb.Type == uint16(ContainerArray):
		values := filterArrayByBitmap(dst.scratchValues[:0], arrayValuesAt(b), bitmapWordsAt(a))
		dst.appendArray(key, values)
	default:
		card := andWords(dst.scratchWords, bitmapWordsAt(a), bitmapWordsAt(b))
		dst.emitWords(key, card)
	}
}

// filterArrayByBitmap appends the values present in words to dst.
func filterArrayByBitmap(dst []uint16, values []uint16, words []uint64) []uint16 {
	for _, value := range values {
		if bitmapContains(words, value) {
			dst = append(dst, value)
		}
	}
	return dst
}

// filterArrayNotInBitmap appends the values absent from words to dst.
func filterArrayNotInBitmap(dst []uint16, values []uint16, words []uint64) []uint16 {
	for _, value := range values {
		if !bitmapContains(words, value) {
			dst = append(dst, value)
		}
	}
	return dst
}

// andWords writes the wordwise intersection into dst, which must hold
// BitmapWords, and returns its cardinality.
func andWords(dst, a, b []uint64) uint32 {
	var card uint32
	for index := 0; index < BitmapWords; index++ {
		word := a[index] & b[index]
		dst[index] = word
		card += uint32(bits.OnesCount64(word))
	}
	return card
}

// copyContainerInto appends an exact copy of a container to dst.
func copyContainerInto(dst *Builder, key uint64, container []byte) {
	h := containerHeaderAt(container)
	switch ContainerType(h.Type) {
	case ContainerArray:
		values := append(dst.scratchValues[:0], arrayValuesAt(container)...)
		dst.appendArray(key, values)
	case ContainerBitmap:
		words := bitmapWordsAt(container)
		copy(dst.scratchWords, words)
		dst.appendBitmap(key, dst.scratchWords, h.Card)
	}
}

// copyAllInto appends a complete copy of src to dst.
func copyAllInto(dst *Builder, src Bitmap) {
	for _, entry := range src.keyEntries() {
		copyContainerInto(dst, entry.Key, src.data[entry.Offset:])
	}
}
