//go:build arm64

#include "textflag.h"

// func matchMaskNEONAsm(base *byte, stride int, count int, want *byte) uint64
TEXT ·matchMaskNEONAsm(SB), NOSPLIT, $0-40
	MOVD base+0(FP), R0
	MOVD stride+8(FP), R1
	MOVD count+16(FP), R2
	MOVD want+24(FP), R3

	VLD1 (R3), [V0.B16]
	MOVD $0, R4
	MOVD $0, R5
	MOVD $1, R6

loop:
	CMP R2, R4
	BGE done
	MOVD R4, R7
	MUL R1, R7, R7
	ADD R0, R7, R7
	VLD1 (R7), [V1.B16]
	VEOR V0.B16, V1.B16, V2.B16
	VUADDLV V2.B16, V3
	VMOV V3.H[0], R8
	CMP $0, R8
	BNE next
	ORR R6, R5, R5
next:
	ADD R6, R6, R6
	ADD $1, R4, R4
	B loop

done:
	MOVD R5, ret+32(FP)
	RET
