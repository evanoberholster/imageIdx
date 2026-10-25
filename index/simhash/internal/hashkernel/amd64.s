//go:build amd64

#include "textflag.h"

// func avx2PDQBlockMaskAsm(q *[32]byte, base *byte, count, limit int) uint64
//
// Option B for AVX2: vector XOR of a 32-byte record against the preloaded query,
// then scalar 64-bit POPCNT of the four words. Result bit i is set when record i
// is within limit. count is clamped to 64 by the caller. AVX2 uses Y0-Y2/X1-X3;
// no R14/R15 (Go's g and reserved registers) are touched.
TEXT ·avx2PDQBlockMaskAsm(SB), NOSPLIT, $0-40
	MOVQ q+0(FP), AX
	MOVQ base+8(FP), SI
	MOVQ count+16(FP), CX
	MOVQ limit+24(FP), DX
	VMOVDQU (AX), Y0

	XORQ R8, R8  // mask
	XORQ R9, R9  // i
	MOVQ $1, R11 // bit = 1 << i

loop:
	// Plan 9 CMPQ computes first-second: loop while i < count.
	CMPQ R9, CX
	JGE  done
	MOVQ R9, R10
	SHLQ $5, R10
	VMOVDQU (SI)(R10*1), Y1
	VPXOR Y0, Y1, Y1

	VEXTRACTI128 $1, Y1, X2
	VMOVQ X1, R12
	VPSRLDQ $8, X1, X3
	VMOVQ X3, R13
	VMOVQ X2, BX
	VPSRLDQ $8, X2, X3
	VMOVQ X3, DI

	POPCNTQ R12, R12
	POPCNTQ R13, R13
	POPCNTQ BX, BX
	POPCNTQ DI, DI
	ADDQ R13, R12
	ADDQ DI, BX
	ADDQ BX, R12
	// R12-DX: set bit when distance <= limit.
	CMPQ R12, DX
	JG   next
	ORQ R11, R8

next:
	INCQ R9
	SHLQ $1, R11
	JMP  loop

done:
	MOVQ R8, ret+32(FP)
	VZEROUPPER
	RET
