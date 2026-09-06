package roar

import (
	"github.com/evanoberholster/imageIdx/internal/bitmapkernel"
)

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
		intersectArrays(&dst.scratchValues, arrayValuesAt(a), arrayValuesAt(b))
		dst.appendArray(key, dst.scratchValues)
	case ha.Type == uint16(ContainerArray):
		filterArrayByBitmap(&dst.scratchValues, arrayValuesAt(a), bitmapWordsAt(b))
		dst.appendArray(key, dst.scratchValues)
	case hb.Type == uint16(ContainerArray):
		filterArrayByBitmap(&dst.scratchValues, arrayValuesAt(b), bitmapWordsAt(a))
		dst.appendArray(key, dst.scratchValues)
	default:
		card := andWords(dst.wordsScratch(), bitmapWordsAt(a), bitmapWordsAt(b))
		dst.emitWords(key, card)
	}
}

// filterArrayByBitmap writes the values present in words to *dst.
func filterArrayByBitmap(dst *[]uint16, values []uint16, words []uint64) {
	out := (*dst)[:0]
	for _, value := range values {
		if bitmapContains(words, value) {
			out = append(out, value)
		}
	}
	*dst = out
}

// filterArrayNotInBitmap writes the values absent from words to *dst.
func filterArrayNotInBitmap(dst *[]uint16, values []uint16, words []uint64) {
	out := (*dst)[:0]
	for _, value := range values {
		if !bitmapContains(words, value) {
			out = append(out, value)
		}
	}
	*dst = out
}

// andWords writes the wordwise intersection into dst, which must hold
// BitmapWords, and returns its cardinality.
func andWords(dst, a, b []uint64) uint32 {
	return bitmapkernel.And(dst, a, b)
}

// copyContainerInto appends an exact copy of a container to dst.
func copyContainerInto(dst *Builder, key uint64, container []byte) {
	h := containerHeaderAt(container)
	switch ContainerType(h.Type) {
	case ContainerArray:
		dst.scratchValues = append(dst.scratchValues[:0], arrayValuesAt(container)...)
		dst.appendArray(key, dst.scratchValues)
	case ContainerBitmap:
		words := bitmapWordsAt(container)
		scratch := dst.wordsScratch()
		copy(scratch, words)
		dst.appendBitmap(key, scratch, h.Card)
	}
}

// copyAllInto appends a complete copy of src to dst.
func copyAllInto(dst *Builder, src Bitmap) {
	for _, entry := range src.keyEntries() {
		copyContainerInto(dst, entry.Key, src.data[entry.Offset:])
	}
}
