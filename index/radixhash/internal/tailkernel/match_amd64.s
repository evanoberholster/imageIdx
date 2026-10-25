//go:build amd64

#include "textflag.h"

// func matchMaskAVX2Asm(base *byte, stride int, count int, want *byte) uint64
TEXT ·matchMaskAVX2Asm(SB), NOSPLIT, $0-40
	MOVQ base+0(FP), AX
	MOVQ stride+8(FP), BX
	MOVQ count+16(FP), CX
	MOVQ want+24(FP), DX

	VMOVDQU (DX), X0
	XORQ R8, R8
	XORQ R9, R9
	MOVQ $1, R10

loop:
	// Plan 9 CMPQ computes first-second: loop while i < count.
	CMPQ R8, CX
	JGE done
	MOVQ R8, R11
	IMULQ BX, R11
	ADDQ AX, R11
	VMOVDQU (R11), X1
	VPCMPEQB X1, X0, X1
	VPMOVMSKB X1, R12
	MOVL $0xFFFF, R13
	CMPL R13, R12
	JNE next
	ORQ R10, R9
next:
	ADDQ R10, R10
	INCQ R8
	JMP loop

done:
	MOVQ R9, ret+32(FP)
	RET
