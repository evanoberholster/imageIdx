//go:build arm64

package tailkernel

func init() { matchMaskKernel = matchMaskNEON }

// matchMaskNEON compares 16-byte prefixes with NEON. ARM64 always has NEON.
func matchMaskNEON(tails []byte, stride, count int, want []byte) uint64 {
	return matchMaskNEONAsm(&tails[0], stride, count, &want[0])
}

func matchMaskNEONAsm(base *byte, stride, count int, want *byte) uint64
