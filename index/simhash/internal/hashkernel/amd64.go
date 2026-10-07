//go:build amd64

package hashkernel

import "golang.org/x/sys/cpu"

// AVX2 available at runtime. Selection is left to the caller/tests until an
// amd64 benchmark shows the AVX2 kernel beats the scalar POPCNT loop
// end-to-end; AVX2 vector XOR plus scalar POPCNT is not obviously faster for a
// single 32-byte record.
var avx2Available = cpu.X86.HasAVX2

//go:noescape
func avx2PDQBlockMaskAsm(q *[32]byte, base *byte, count, limit int) uint64

// avx2PDQBlockMask is the AVX2 bulk PDQ kernel: it loads 32 bytes per record
// with VMOVDQU, XORs against the preloaded query, and reduces to scalar 64-bit
// POPCNT. It is only safe on quiescent memory (no concurrent writer).
func avx2PDQBlockMask(q *[32]byte, base []byte, count, limit int) uint64 {
	if count <= 0 {
		return 0
	}
	if count > BlockRecords {
		count = BlockRecords
	}
	return avx2PDQBlockMaskAsm(q, &base[0], count, limit)
}
