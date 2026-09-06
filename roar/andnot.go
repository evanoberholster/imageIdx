package roar

import "github.com/evanoberholster/imageIdx/internal/bitmapkernel"

// AndNotInto appends a minus b to dst.
func AndNotInto(dst *Builder, a, b Bitmap) {
	aEntries, bEntries := a.keyEntries(), b.keyEntries()
	i, j := 0, 0
	for i < len(aEntries) {
		if j >= len(bEntries) || aEntries[i].Key < bEntries[j].Key {
			copyContainerInto(dst, aEntries[i].Key, a.data[aEntries[i].Offset:])
			i++
			continue
		}
		if aEntries[i].Key > bEntries[j].Key {
			j++
			continue
		}
		andNotContainerInto(dst, aEntries[i].Key, a.data[aEntries[i].Offset:], b.data[bEntries[j].Offset:])
		i++
		j++
	}
}

func andNotContainerInto(dst *Builder, key uint64, a, b []byte) {
	ha, hb := containerHeaderAt(a), containerHeaderAt(b)
	switch {
	case ha.Type == uint16(ContainerArray) && hb.Type == uint16(ContainerArray):
		differenceArrays(&dst.scratchValues, arrayValuesAt(a), arrayValuesAt(b))
		dst.appendArray(key, dst.scratchValues)
	case ha.Type == uint16(ContainerArray):
		filterArrayNotInBitmap(&dst.scratchValues, arrayValuesAt(a), bitmapWordsAt(b))
		dst.appendArray(key, dst.scratchValues)
	case hb.Type == uint16(ContainerArray):
		card := andNotWordsArray(dst.wordsScratch(), bitmapWordsAt(a), arrayValuesAt(b))
		dst.emitWords(key, card)
	default:
		card := andNotWords(dst.wordsScratch(), bitmapWordsAt(a), bitmapWordsAt(b))
		dst.emitWords(key, card)
	}
}

// andNotWordsArray writes a bitmap container minus a sparse array into dst
// and returns its cardinality.
func andNotWordsArray(dst, words []uint64, values []uint16) uint32 {
	copy(dst, words)
	for _, value := range values {
		dst[value>>6] &^= uint64(1) << (value & 63)
	}
	return uint32(wordsCardinality(dst))
}

// andNotWords writes the wordwise difference into dst and returns its
// cardinality.
func andNotWords(dst, a, b []uint64) uint32 {
	return bitmapkernel.AndNot(dst, a, b)
}
