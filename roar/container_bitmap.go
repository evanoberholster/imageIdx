package roar

import (
	"math/bits"

	"github.com/evanoberholster/imageIdx/roar/internal/bitmapkernel"
)

// bitmapContains reports whether a bitmap container contains value x.
func bitmapContains(words []uint64, x uint16) bool {
	return words[x>>6]&(uint64(1)<<(x&63)) != 0
}

// andWordsCardinality counts the intersection of two bitmap containers
// without materializing it.
func andWordsCardinality(a, b []uint64) uint64 {
	return bitmapkernel.AndCardinality(a, b)
}

// orWordsCardinality counts the union of two bitmap containers without
// materializing it.
func orWordsCardinality(a, b []uint64) uint64 {
	return bitmapkernel.OrCardinality(a, b)
}

// andNotWordsCardinality counts a minus b for two bitmap containers without
// materializing it.
func andNotWordsCardinality(a, b []uint64) uint64 {
	return bitmapkernel.AndNotCardinality(a, b)
}

// wordsIntersect reports whether two bitmap containers share any bit.
func wordsIntersect(a, b []uint64) bool {
	return bitmapkernel.Intersects(a, b)
}

// wordsCardinality sums the set bits in a bitmap container.
func wordsCardinality(words []uint64) uint64 {
	return bitmapkernel.Cardinality(words)
}

// extractWords writes the set values of a bitmap container into *dst in
// ascending order.
func extractWords(dst *[]uint16, words []uint64) {
	out := (*dst)[:0]
	for wordIndex, word := range words {
		for word != 0 {
			bit := bits.TrailingZeros64(word)
			out = append(out, uint16(wordIndex<<6|bit))
			word &= word - 1
		}
	}
	*dst = out
}
