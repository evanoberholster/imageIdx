//go:build arm64

#include "textflag.h"

// func neonPDQDistanceAsm(a, b *[32]byte) uint16
TEXT ·neonPDQDistanceAsm(SB), NOSPLIT, $0-18
	MOVD a+0(FP), R0
	MOVD b+8(FP), R1
	VLD1 (R0), [V0.B16]
	ADD  $16, R0, R2
	VLD1 (R2), [V1.B16]
	VLD1 (R1), [V2.B16]
	ADD  $16, R1, R3
	VLD1 (R3), [V3.B16]
	VEOR V0.B16, V2.B16, V0.B16
	VEOR V1.B16, V3.B16, V1.B16
	VCNT V0.B16, V0.B16
	VCNT V1.B16, V1.B16
	VUADDLV V0.B16, V0
	VUADDLV V1.B16, V1
	VMOV V0.H[0], R4
	VMOV V1.H[0], R5
	ADD  R4, R5, R4
	MOVH R4, ret+16(FP)
	RET

// func neonPDQBlockMaskAsm(q *[32]byte, base *byte, count, limit int) uint64
TEXT ·neonPDQBlockMaskAsm(SB), NOSPLIT, $0-40
	MOVD q+0(FP), R3
	MOVD base+8(FP), R0
	MOVD count+16(FP), R2
	MOVD limit+24(FP), R4

	// Preload the query as two 16-byte vectors.
	VLD1 (R3), [V0.B16]
	ADD  $16, R3, R12
	VLD1 (R12), [V1.B16]

	MOVD $0, R5  // mask
	MOVD $0, R7  // i
	MOVD $1, R9  // bit = 1 << i
	MOVD $32, R13

loop:
	CMP R2, R7
	BGE done
	MUL R13, R7, R8 // byte offset of record i
	ADD R0, R8, R8

	VLD1 (R8), [V2.B16]
	VEOR V0.B16, V2.B16, V2.B16
	VCNT V2.B16, V2.B16
	VUADDLV V2.B16, V2
	VMOV V2.H[0], R10
	CMP  R4, R10
	BGT  next

	ADD  $16, R8, R12
	VLD1 (R12), [V3.B16]
	VEOR V1.B16, V3.B16, V3.B16
	VCNT V3.B16, V3.B16
	VUADDLV V3.B16, V3
	VMOV V3.H[0], R11
	ADD  R11, R10, R10
	CMP  R4, R10
	BGT  next

	ORR R9, R5, R5

next:
	ADD $1, R7, R7
	ADD R9, R9, R9
	B   loop

done:
	MOVD R5, ret+32(FP)
	RET
