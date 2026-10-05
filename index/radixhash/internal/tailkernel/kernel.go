// Package tailkernel implements the leaf tail-prefix comparison kernel behind a
// scalar reference. RadixHash linearly scans small leaves with it; larger
// leaves use binary search over the sorted tails, which is always correct and
// independent of the backend selected here.
//
// The scalar reference is used unless a supported SIMD backend replaces the
// dispatch variable at init. Backends differ only in speed; every backend
// returns the same match mask.
package tailkernel

// matchMaskKernel is the active backend. It starts as the scalar reference and
// is replaced by an architecture backend at init when the CPU supports it.
var matchMaskKernel = matchMaskScalar

// MatchMask returns a bitmask whose bit i is set when the 16-byte prefix at
// tails[i*stride:] equals want[:16]. At most 64 slots are examined. stride must
// be at least 16, want at least 16 bytes, and tails must contain
// (count-1)*stride+16 bytes; a short buffer yields a zero mask.
//
// Callers must confirm a full-width equality on each set bit: the mask compares
// a 16-byte prefix, which is cheap and vectorizable, not the whole tail.
func MatchMask(tails []byte, stride, count int, want []byte) uint64 {
	if count <= 0 || stride < 16 || len(want) < 16 {
		return 0
	}
	if count > 64 {
		count = 64
	}
	if len(tails) < (count-1)*stride+16 {
		return 0
	}
	return matchMaskKernel(tails, stride, count, want)
}
