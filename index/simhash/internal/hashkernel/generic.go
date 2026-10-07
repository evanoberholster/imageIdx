//go:build !arm64

package hashkernel

// Direct, compile-time selected entry points. On non-arm64 platforms the scalar
// reference is used (on amd64 math/bits lowers to POPCNT).
func PHashDistance(a, b uint64) int     { return scalarPHashDistance(a, b) }
func PDQDistance(a, b *[32]byte) uint16 { return scalarPDQDistance(a, b) }
func PDQBlockMask(q *[32]byte, base []byte, count, limit int) uint64 {
	return scalarPDQBlockMask(q, base, count, limit)
}
