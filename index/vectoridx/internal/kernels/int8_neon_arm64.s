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

// func l2Int8NEON(a, b []byte) int32
// Processes floor(len(a)/32) blocks of 32 int8 values with one pass over the
// inputs: SABDL widens the absolute difference, then VUMLAL accumulates its
// square in int32 lanes.
TEXT ·l2Int8NEON(SB), NOSPLIT, $0-52
	MOVD a_base+0(FP), R0
	MOVD a_len+8(FP), R1
	MOVD b_base+24(FP), R2
	VEOR V0.B16, V0.B16, V0.B16
	VEOR V1.B16, V1.B16, V1.B16
	VEOR V2.B16, V2.B16, V2.B16
	VEOR V3.B16, V3.B16, V3.B16
	LSR  $5, R1, R3
	CBZ  R3, doneL2
loopL2:
	VLD1.P 32(R0), [V4.B16, V5.B16]
	VLD1.P 32(R2), [V6.B16, V7.B16]
	// sabdl v8.8h, v4.8b, v6.8b
	WORD $0x0e267088
	// sabdl2 v9.8h, v4.16b, v6.16b
	WORD $0x4e267089
	VUMLAL  V8.H4, V8.H4, V0.S4
	VUMLAL2 V8.H8, V8.H8, V0.S4
	VUMLAL  V9.H4, V9.H4, V1.S4
	VUMLAL2 V9.H8, V9.H8, V1.S4
	// sabdl v10.8h, v5.8b, v7.8b
	WORD $0x0e2770aa
	// sabdl2 v11.8h, v5.16b, v7.16b
	WORD $0x4e2770ab
	VUMLAL  V10.H4, V10.H4, V2.S4
	VUMLAL2 V10.H8, V10.H8, V2.S4
	VUMLAL  V11.H4, V11.H4, V3.S4
	VUMLAL2 V11.H8, V11.H8, V3.S4
	SUBS $1, R3, R3
	BNE  loopL2
doneL2:
	VADD V1.S4, V0.S4, V0.S4
	VADD V3.S4, V2.S4, V2.S4
	VADD V2.S4, V0.S4, V0.S4
	VADDP V0.S4, V0.S4, V0.S4
	VADDP V0.S4, V0.S4, V0.S4
	VMOV V0.S[0], R0
	MOVW R0, ret+48(FP)
	RET

// func l2Uint8NEON(a, b []byte) int32
TEXT ·l2Uint8NEON(SB), NOSPLIT, $0-52
	MOVD a_base+0(FP), R0
	MOVD a_len+8(FP), R1
	MOVD b_base+24(FP), R2
	VEOR V0.B16, V0.B16, V0.B16
	VEOR V1.B16, V1.B16, V1.B16
	VEOR V2.B16, V2.B16, V2.B16
	VEOR V3.B16, V3.B16, V3.B16
	LSR  $5, R1, R3
	CBZ  R3, doneL2u
loopL2u:
	VLD1.P 32(R0), [V4.B16, V5.B16]
	VLD1.P 32(R2), [V6.B16, V7.B16]
	// uabdl v8.8h, v4.8b, v6.8b
	WORD $0x2e267088
	// uabdl2 v9.8h, v4.16b, v6.16b
	WORD $0x6e267089
	VUMLAL  V8.H4, V8.H4, V0.S4
	VUMLAL2 V8.H8, V8.H8, V0.S4
	VUMLAL  V9.H4, V9.H4, V1.S4
	VUMLAL2 V9.H8, V9.H8, V1.S4
	// uabdl v10.8h, v5.8b, v7.8b
	WORD $0x2e2770aa
	// uabdl2 v11.8h, v5.16b, v7.16b
	WORD $0x6e2770ab
	VUMLAL  V10.H4, V10.H4, V2.S4
	VUMLAL2 V10.H8, V10.H8, V2.S4
	VUMLAL  V11.H4, V11.H4, V3.S4
	VUMLAL2 V11.H8, V11.H8, V3.S4
	SUBS $1, R3, R3
	BNE  loopL2u
doneL2u:
	VADD V1.S4, V0.S4, V0.S4
	VADD V3.S4, V2.S4, V2.S4
	VADD V2.S4, V0.S4, V0.S4
	VADDP V0.S4, V0.S4, V0.S4
	VADDP V0.S4, V0.S4, V0.S4
	VMOV V0.S[0], R0
	MOVW R0, ret+48(FP)
	RET
