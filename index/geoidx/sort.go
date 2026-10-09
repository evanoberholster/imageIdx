package geoidx

import (
	"math/bits"
	"slices"

	common "github.com/evanoberholster/imageIdx/common"
)

// radixSortThreshold is the suffix length above which the specialized sorts
// replace the comparison sort used for small results.
const radixSortThreshold = 1 << 13

// bitsetMaxID caps the dense bitset accelerator (2^30 ids -> 128 MiB of bits).
const bitsetMaxID = 1 << 30

// sortIDs sorts ids ascending and removes duplicates, returning the shortened
// slice. Small suffixes use the comparison sort; broad results use a dense
// bitset when the ImageID domain is compact, otherwise an LSD radix sort. The
// sorted initial-query contract forces this work, so the goal is to keep it far
// below a general comparison sort. The input may alias the destination suffix.
func sortIDs(ids []common.ImageID, maxID common.ImageID) []common.ImageID {
	switch len(ids) {
	case 0, 1:
		return ids
	}
	if len(ids) < radixSortThreshold {
		slices.Sort(ids)
		return slices.Compact(ids)
	}
	if out, ok := sortIDsBitset(ids, maxID); ok {
		return out
	}
	radixSortIDs(ids)
	return slices.Compact(ids)
}

// sortIDsBitset sorts and de-duplicates ids with a dense bitmap over [0, maxID].
// It returns the shortened slice and true when the bitset was worthwhile (small
// enough, non-sparse domain), otherwise false. The actual maximum of ids is used
// so a stale maxID hint can never overrun the bitset.
func sortIDsBitset(ids []common.ImageID, maxID common.ImageID) ([]common.ImageID, bool) {
	if maxID == 0 {
		return ids, false
	}
	limit := maxID
	for _, id := range ids {
		if id > limit {
			limit = id
		}
	}
	if uint64(limit) > bitsetMaxID {
		return ids, false
	}
	// Reject sparse ID spaces where scanning the bitset would dominate. The
	// dense bitset costs limit/64 words plus len(ids) marks; it wins while that
	// stays within a small multiple of the result size.
	if uint64(limit) > 256*uint64(len(ids)) {
		return ids, false
	}
	words := int(uint64(limit)>>6) + 1
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
	return ids[:out], true
}

// radixSortIDs sorts a uint64 slice with an 8-pass, base-256 LSD radix sort. One
// temp buffer the size of the result is allocated; eight passes guarantee the
// sorted data lands back in ids. Duplicates are preserved; the caller compacts.
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
