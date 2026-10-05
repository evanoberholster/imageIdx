//go:build arm64

package tailkernel

func init() { matchMaskKernel = matchMaskNEON }

// matchMaskNEON compares 16-byte prefixes with NEON. ARM64 always has NEON.
func matchMaskNEON(tails []byte, stride, count int, want []byte) uint64 {
	return matchMaskNEONAsm(&tails[0], stride, count, &want[0])
}

// matchMaskNEONAsm is implemented in assembly; it does not retain either
// pointer, so the compiler can keep the caller's query prefix on the stack.

//go:noescape
func matchMaskNEONAsm(base *byte, stride, count int, want *byte) uint64
