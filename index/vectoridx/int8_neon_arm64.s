//go:build arm64

#include "textflag.h"

// func dotInt8NEON(a, b []byte) int32
// Processes floor(len(a)/64) blocks of 64 int8 values using ARMv8.2 SDOT.
TEXT ·dotInt8NEON(SB), NOSPLIT, $0-52
	MOVD a_base+0(FP), R0
	MOVD a_len+8(FP), R1
	MOVD b_base+24(FP), R2
	VEOR V0.B16, V0.B16, V0.B16
	VEOR V1.B16, V1.B16, V1.B16
	VEOR V2.B16, V2.B16, V2.B16
	VEOR V3.B16, V3.B16, V3.B16
	LSR  $6, R1, R3
	CBZ  R3, done
loop:
	VLD1.P 64(R0), [V4.B16, V5.B16, V6.B16, V7.B16]
	VLD1.P 64(R2), [V8.B16, V9.B16, V10.B16, V11.B16]
	// sdot v0.4s, v4.16b, v8.16b ... v3.4s, v7.16b, v11.16b
	WORD $0x4e889480
	WORD $0x4e8994a1
	WORD $0x4e8a94c2
	WORD $0x4e8b94e3
	SUBS $1, R3, R3
	BNE  loop
done:
	VADD V1.S4, V0.S4, V0.S4
	VADD V3.S4, V2.S4, V2.S4
	VADD V2.S4, V0.S4, V0.S4
	VADDP V0.S4, V0.S4, V0.S4
	VADDP V0.S4, V0.S4, V0.S4
	VMOV V0.S[0], R0
	MOVW R0, ret+48(FP)
	RET

// func dotUint8NEON(a, b []byte) int32
TEXT ·dotUint8NEON(SB), NOSPLIT, $0-52
	MOVD a_base+0(FP), R0
	MOVD a_len+8(FP), R1
	MOVD b_base+24(FP), R2
	VEOR V0.B16, V0.B16, V0.B16
	VEOR V1.B16, V1.B16, V1.B16
	VEOR V2.B16, V2.B16, V2.B16
	VEOR V3.B16, V3.B16, V3.B16
	LSR  $6, R1, R3
	CBZ  R3, done2
loop2:
	VLD1.P 64(R0), [V4.B16, V5.B16, V6.B16, V7.B16]
	VLD1.P 64(R2), [V8.B16, V9.B16, V10.B16, V11.B16]
	// udot v0.4s, v4.16b, v8.16b ... v3.4s, v7.16b, v11.16b
	WORD $0x6e889480
	WORD $0x6e8994a1
	WORD $0x6e8a94c2
	WORD $0x6e8b94e3
	SUBS $1, R3, R3
	BNE  loop2
done2:
	VADD V1.S4, V0.S4, V0.S4
	VADD V3.S4, V2.S4, V2.S4
	VADD V2.S4, V0.S4, V0.S4
	VADDP V0.S4, V0.S4, V0.S4
	VADDP V0.S4, V0.S4, V0.S4
	VMOV V0.S[0], R0
	MOVW R0, ret+48(FP)
	RET
