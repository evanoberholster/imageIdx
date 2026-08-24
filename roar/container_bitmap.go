package roar

import "math/bits"

// bitmapContains reports whether a bitmap container contains value x.
func bitmapContains(words []uint64, x uint16) bool {
	return words[x>>6]&(uint64(1)<<(x&63)) != 0
}

// andWordsCardinality counts the intersection of two bitmap containers
// without materializing it.
func andWordsCardinality(a, b []uint64) uint64 {
	var count uint64
	for index := 0; index < BitmapWords; index++ {
		count += uint64(bits.OnesCount64(a[index] & b[index]))
	}
	return count
}

// orWordsCardinality counts the union of two bitmap containers without
// materializing it.
func orWordsCardinality(a, b []uint64) uint64 {
	var count uint64
	for index := 0; index < BitmapWords; index++ {
		count += uint64(bits.OnesCount64(a[index] | b[index]))
	}
	return count
}

// andNotWordsCardinality counts a minus b for two bitmap containers without
// materializing it.
func andNotWordsCardinality(a, b []uint64) uint64 {
	var count uint64
	for index := 0; index < BitmapWords; index++ {
		count += uint64(bits.OnesCount64(a[index] &^ b[index]))
	}
	return count
}

// wordsIntersect reports whether two bitmap containers share any bit.
func wordsIntersect(a, b []uint64) bool {
	for index := 0; index < BitmapWords; index++ {
		if a[index]&b[index] != 0 {
			return true
		}
	}
	return false
}

// wordsCardinality sums the set bits in a bitmap container.
func wordsCardinality(words []uint64) uint64 {
	var count uint64
	for index := 0; index < BitmapWords; index++ {
		count += uint64(bits.OnesCount64(words[index]))
	}
	return count
}

// extractWords writes the set values of a bitmap container into dst in
// ascending order.
func extractWords(dst []uint16, words []uint64) []uint16 {
	for wordIndex, word := range words {
		for word != 0 {
			bit := bits.TrailingZeros64(word)
			dst = append(dst, uint16(wordIndex<<6|bit))
			word &= word - 1
		}
	}
	return dst
}
