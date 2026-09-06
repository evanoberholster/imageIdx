package roar

import "math/bits"

// OrInto appends the union of a and b to dst.
func OrInto(dst *Builder, a, b Bitmap) {
	aEntries, bEntries := a.keyEntries(), b.keyEntries()
	i, j := 0, 0
	for i < len(aEntries) || j < len(bEntries) {
		switch {
		case j >= len(bEntries) || i < len(aEntries) && aEntries[i].Key < bEntries[j].Key:
			copyContainerInto(dst, aEntries[i].Key, a.data[aEntries[i].Offset:])
			i++
		case i >= len(aEntries) || aEntries[i].Key > bEntries[j].Key:
			copyContainerInto(dst, bEntries[j].Key, b.data[bEntries[j].Offset:])
			j++
		default:
			orContainerInto(dst, aEntries[i].Key, a.data[aEntries[i].Offset:], b.data[bEntries[j].Offset:])
			i++
			j++
		}
	}
}

func orContainerInto(dst *Builder, key uint64, a, b []byte) {
	ha, hb := containerHeaderAt(a), containerHeaderAt(b)
	switch {
	case ha.Type == uint16(ContainerArray) && hb.Type == uint16(ContainerArray):
		dst.scratchValues = unionArrays(dst.scratchValues[:0], arrayValuesAt(a), arrayValuesAt(b))
		dst.emitValues(key, dst.scratchValues)
	case ha.Type == uint16(ContainerArray):
		orArrayBitmapInto(dst, key, arrayValuesAt(a), bitmapWordsAt(b), hb.Card)
	case hb.Type == uint16(ContainerArray):
		orArrayBitmapInto(dst, key, arrayValuesAt(b), bitmapWordsAt(a), ha.Card)
	default:
		card := orWords(dst.wordsScratch(), bitmapWordsAt(a), bitmapWordsAt(b))
		dst.emitWords(key, card)
	}
}

// orArrayBitmapInto unions a sparse array with a dense container. The
// result is dense because the input bitmap already exceeds the array
// threshold.
func orArrayBitmapInto(dst *Builder, key uint64, values []uint16, words []uint64, baseCard uint32) {
	scratch := dst.wordsScratch()
	copy(scratch, words)
	card := baseCard
	for _, value := range values {
		word, bit := value>>6, value&63
		mask := uint64(1) << bit
		if scratch[word]&mask == 0 {
			scratch[word] |= mask
			card++
		}
	}
	dst.appendBitmap(key, scratch, card)
}

// orWords writes the wordwise union into dst and returns its cardinality.
func orWords(dst, a, b []uint64) uint32 {
	var card uint32
	for index := 0; index < BitmapWords; index++ {
		word := a[index] | b[index]
		dst[index] = word
		card += uint32(bits.OnesCount64(word))
	}
	return card
}
