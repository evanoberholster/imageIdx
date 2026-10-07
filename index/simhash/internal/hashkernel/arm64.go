//go:build arm64

package hashkernel

// ARM64 always has NEON. The block kernel loads two 16-byte vectors per record,
// XORs against the preloaded query, popcounts with VCNT, and reduces with
// VUADDLV, rejecting after the first 16 bytes when the partial count already
// exceeds the limit.
func init() {
	Current.PDQDistance = neonPDQDistance
	Current.PDQBlockMask = neonPDQBlockMask
}

// Direct, compile-time selected entry points. Calling these (rather than the
// Current func fields) keeps pointer arguments from escaping to the heap,
// because the assembly kernels are //go:noescape.
func PHashDistance(a, b uint64) int     { return scalarPHashDistance(a, b) }
func PDQDistance(a, b *[32]byte) uint16 { return neonPDQDistance(a, b) }
func PDQBlockMask(q *[32]byte, base []byte, count, limit int) uint64 {
	return neonPDQBlockMask(q, base, count, limit)
}

func neonPDQDistance(a, b *[32]byte) uint16 { return neonPDQDistanceAsm(a, b) }

func neonPDQBlockMask(q *[32]byte, base []byte, count, limit int) uint64 {
	if count <= 0 {
		return 0
	}
	if count > BlockRecords {
		count = BlockRecords
	}
	return neonPDQBlockMaskAsm(q, &base[0], count, limit)
}

//go:noescape
func neonPDQDistanceAsm(a, b *[32]byte) uint16

//go:noescape
func neonPDQBlockMaskAsm(q *[32]byte, base *byte, count, limit int) uint64
