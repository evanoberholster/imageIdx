//go:build amd64

#include "textflag.h"

// func dotInt8AVX2(a, b []byte) int32
// Processes floor(len(a)/32) blocks of 32 int8 values.
TEXT ·dotInt8AVX2(SB), NOSPLIT, $0-52
	MOVQ a_base+0(FP), AX
	MOVQ a_len+8(FP), CX
	MOVQ b_base+24(FP), BX
	SHRQ $5, CX
	VXORPS Y0, Y0, Y0
	VXORPS Y1, Y1, Y1
	TESTQ CX, CX
	JZ   done
loop:
	VPMOVSXBW (AX), Y4
	VPMOVSXBW 16(AX), Y5
	VPMOVSXBW (BX), Y6
	VPMOVSXBW 16(BX), Y7
	VPMADDWD Y6, Y4, Y8
	VPMADDWD Y7, Y5, Y9
	VPADDD Y8, Y0, Y0
	VPADDD Y9, Y1, Y1
	ADDQ $32, AX
	ADDQ $32, BX
	DECQ CX
	JNZ  loop
done:
	VPADDD Y1, Y0, Y0
	VEXTRACTI128 $1, Y0, X1
	VPADDD X1, X0, X0
	VPHADDD X0, X0, X0
	VPHADDD X0, X0, X0
	VMOVD X0, AX
	MOVL AX, ret+48(FP)
	VZEROUPPER
	RET

// func dotUint8AVX2(a, b []byte) int32
TEXT ·dotUint8AVX2(SB), NOSPLIT, $0-52
	MOVQ a_base+0(FP), AX
	MOVQ a_len+8(FP), CX
	MOVQ b_base+24(FP), BX
	SHRQ $5, CX
	VXORPS Y0, Y0, Y0
	VXORPS Y1, Y1, Y1
	TESTQ CX, CX
	JZ   done2
loop2:
	VPMOVZXBW (AX), Y4
	VPMOVZXBW 16(AX), Y5
	VPMOVZXBW (BX), Y6
	VPMOVZXBW 16(BX), Y7
	VPMADDWD Y6, Y4, Y8
	VPMADDWD Y7, Y5, Y9
	VPADDD Y8, Y0, Y0
	VPADDD Y9, Y1, Y1
	ADDQ $32, AX
	ADDQ $32, BX
	DECQ CX
	JNZ  loop2
done2:
	VPADDD Y1, Y0, Y0
	VEXTRACTI128 $1, Y0, X1
	VPADDD X1, X0, X0
	VPHADDD X0, X0, X0
	VPHADDD X0, X0, X0
	VMOVD X0, AX
	MOVL AX, ret+48(FP)
	VZEROUPPER
	RET

// func l2Int8AVX2(a, b []byte) int32
// Processes floor(len(a)/32) blocks of 32 int8 values with one widening pass:
// sign-extend to int16, subtract, square-accumulate to int32.
TEXT ·l2Int8AVX2(SB), NOSPLIT, $0-52
	MOVQ a_base+0(FP), AX
	MOVQ a_len+8(FP), CX
	MOVQ b_base+24(FP), BX
	SHRQ $5, CX
	VXORPS Y0, Y0, Y0
	VXORPS Y1, Y1, Y1
	TESTQ CX, CX
	JZ   doneL2
loopL2:
	VPMOVSXBW (AX), Y4
	VPMOVSXBW 16(AX), Y5
	VPMOVSXBW (BX), Y6
	VPMOVSXBW 16(BX), Y7
	VPSUBW Y6, Y4, Y8
	VPSUBW Y7, Y5, Y9
	VPMADDWD Y8, Y8, Y8
	VPMADDWD Y9, Y9, Y9
	VPADDD Y8, Y0, Y0
	VPADDD Y9, Y1, Y1
	ADDQ $32, AX
	ADDQ $32, BX
	DECQ CX
	JNZ  loopL2
doneL2:
	VPADDD Y1, Y0, Y0
	VEXTRACTI128 $1, Y0, X1
	VPADDD X1, X0, X0
	VPHADDD X0, X0, X0
	VPHADDD X0, X0, X0
	VMOVD X0, AX
	MOVL AX, ret+48(FP)
	VZEROUPPER
	RET

// func l2Uint8AVX2(a, b []byte) int32
// Zero-extended differences are interpreted as signed int16, which is exact
// because every difference is in [-255, 255].
TEXT ·l2Uint8AVX2(SB), NOSPLIT, $0-52
	MOVQ a_base+0(FP), AX
	MOVQ a_len+8(FP), CX
	MOVQ b_base+24(FP), BX
	SHRQ $5, CX
	VXORPS Y0, Y0, Y0
	VXORPS Y1, Y1, Y1
	TESTQ CX, CX
	JZ   doneL2u
loopL2u:
	VPMOVZXBW (AX), Y4
	VPMOVZXBW 16(AX), Y5
	VPMOVZXBW (BX), Y6
	VPMOVZXBW 16(BX), Y7
	VPSUBW Y6, Y4, Y8
	VPSUBW Y7, Y5, Y9
	VPMADDWD Y8, Y8, Y8
	VPMADDWD Y9, Y9, Y9
	VPADDD Y8, Y0, Y0
	VPADDD Y9, Y1, Y1
	ADDQ $32, AX
	ADDQ $32, BX
	DECQ CX
	JNZ  loopL2u
doneL2u:
	VPADDD Y1, Y0, Y0
	VEXTRACTI128 $1, Y0, X1
	VPADDD X1, X0, X0
	VPHADDD X0, X0, X0
	VPHADDD X0, X0, X0
	VMOVD X0, AX
	MOVL AX, ret+48(FP)
	VZEROUPPER
	RET
