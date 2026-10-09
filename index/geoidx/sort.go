package geoidx

import (
	"math/bits"
	"slices"

	common "github.com/evanoberholster/imageIdx/common"
)

// radixSortThreshold is the suffix length above which the specialized sorts
// replace the comparison sort used for small results.
const radixSortThreshold = 1 << 13

// bitsetMaxID caps the dense bitset accelerator (2^27 ids -> 16 MiB of bits).
const bitsetMaxID = 1 << 27

// sortIDs sorts ids ascending. Small suffixes use the comparison sort; broad
// results use a dense bitset when the ImageID domain is compact, otherwise an
// LSD radix sort. The sorted initial-query contract forces this work, so the
// goal is to keep it far below a general comparison sort.
func sortIDs(ids []common.ImageID, maxID common.ImageID) {
	if len(ids) < radixSortThreshold {
		slices.Sort(ids)
		return
	}
	if sortIDsBitset(ids, maxID) {
		return
	}
	radixSortIDs(ids)
}

// sortIDsBitset sorts ids ascending with a dense bitmap over [0, maxID]. It also
// de-duplicates. It returns false when the bitset is not worthwhile (small or
// sparse ID domains, or a too-large maxID).
func sortIDsBitset(ids []common.ImageID, maxID common.ImageID) bool {
	if maxID == 0 || uint64(maxID) > bitsetMaxID {
		return false
	}
	// Reject sparse ID spaces where scanning the bitset would dominate. The
	// dense bitset costs maxID/64 words plus len(ids) marks; it wins while that
	// stays within a small multiple of the result size.
	if uint64(maxID) > 256*uint64(len(ids)) {
		return false
	}
	words := int(uint64(maxID)>>6) + 1
	set := make([]uint64, words)
	for _, id := range ids {
		set[id>>6] |= 1 << (id & 63)
	}
	out := 0
	for w, word := range set {
		for word != 0 {
			b := bits.TrailingZeros64(word)
			ids[out] = common.ImageID(uint64(w)<<6 | uint64(b))
			out++
			word &= word - 1
		}
	}
	return true
}

// radixSortIDs sorts a uint64 slice with an 8-pass, base-256 LSD radix sort. One
// temp buffer the size of the result is allocated; eight passes guarantee the
// sorted data lands back in ids.
func radixSortIDs(ids []common.ImageID) {
	const shiftBits = 8
	const bucketCount = 1 << shiftBits
	const mask = bucketCount - 1
	buf := make([]common.ImageID, len(ids))
	src, dst := ids, buf
	var counts [bucketCount]int
	for shift := uint(0); shift < 64; shift += shiftBits {
		for i := range counts {
			counts[i] = 0
		}
		for _, v := range src {
			counts[(v>>shift)&mask]++
		}
		sum := 0
		for i := range counts {
			c := counts[i]
			counts[i] = sum
			sum += c
		}
		for _, v := range src {
			k := (v >> shift) & mask
			dst[counts[k]] = v
			counts[k]++
		}
		src, dst = dst, src
	}
}
