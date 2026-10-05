//go:build amd64

package tailkernel

import "golang.org/x/sys/cpu"

func init() {
	if cpu.X86.HasAVX2 {
		matchMaskKernel = matchMaskAVX2
	}
}

// matchMaskAVX2 compares 16-byte prefixes with AVX2 when available.
func matchMaskAVX2(tails []byte, stride, count int, want []byte) uint64 {
	return matchMaskAVX2Asm(&tails[0], stride, count, &want[0])
}

// matchMaskAVX2Asm is implemented in assembly; it does not retain either
// pointer, so the compiler can keep the caller's query prefix on the stack.

//go:noescape
func matchMaskAVX2Asm(base *byte, stride, count int, want *byte) uint64
