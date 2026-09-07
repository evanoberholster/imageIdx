//go:build goexperiment.simd && amd64

#include "textflag.h"

// andAVX2FixedAsm processes a 65,536-bit dense container. AVX2 handles four
// words per vector; POPCNT counts the four result lanes without a second pass.
// func andAVX2FixedAsm(dst, a, b []uint64) uint32
TEXT ·andAVX2FixedAsm(SB), NOSPLIT, $0-76
	MOVQ dst_base+0(FP), AX
	MOVQ a_base+24(FP), BX
	MOVQ b_base+48(FP), CX
	MOVQ $256, DX
	XORQ R8, R8
	XORQ R13, R13
	XORQ R14, R14
	XORQ R15, R15
and_avx2_fixed_loop:
	VMOVDQU (BX), Y0
	VMOVDQU (CX), Y1
	VPAND Y1, Y0, Y2
	VMOVDQU Y2, (AX)
	POPCNTQ 0(AX), R9
	ADDQ R9, R8
	POPCNTQ 8(AX), R10
	POPCNTQ 16(AX), R11
	POPCNTQ 24(AX), R12
	ADDQ R10, R13
	ADDQ R11, R14
	ADDQ R12, R15
	ADDQ $32, AX
	ADDQ $32, BX
	ADDQ $32, CX
	DECQ DX
	JNZ and_avx2_fixed_loop
	ADDQ R13, R8
	ADDQ R14, R8
	ADDQ R15, R8
	VZEROUPPER
	MOVL R8, ret+72(FP)
	RET

// func orAVX2FixedAsm(dst, a, b []uint64) uint32
TEXT ·orAVX2FixedAsm(SB), NOSPLIT, $0-76
	MOVQ dst_base+0(FP), AX
	MOVQ a_base+24(FP), BX
	MOVQ b_base+48(FP), CX
	MOVQ $256, DX
	XORQ R8, R8
	XORQ R13, R13
	XORQ R14, R14
	XORQ R15, R15
or_avx2_fixed_loop:
	VMOVDQU (BX), Y0
	VMOVDQU (CX), Y1
	VPOR Y1, Y0, Y2
	VMOVDQU Y2, (AX)
	POPCNTQ 0(AX), R9
	ADDQ R9, R8
	POPCNTQ 8(AX), R10
	POPCNTQ 16(AX), R11
	POPCNTQ 24(AX), R12
	ADDQ R10, R13
	ADDQ R11, R14
	ADDQ R12, R15
	ADDQ $32, AX
	ADDQ $32, BX
	ADDQ $32, CX
	DECQ DX
	JNZ or_avx2_fixed_loop
	ADDQ R13, R8
	ADDQ R14, R8
	ADDQ R15, R8
	VZEROUPPER
	MOVL R8, ret+72(FP)
	RET

// func andNotAVX2FixedAsm(dst, a, b []uint64) uint32
TEXT ·andNotAVX2FixedAsm(SB), NOSPLIT, $0-76
	MOVQ dst_base+0(FP), AX
	MOVQ a_base+24(FP), BX
	MOVQ b_base+48(FP), CX
	MOVQ $256, DX
	XORQ R8, R8
	XORQ R13, R13
	XORQ R14, R14
	XORQ R15, R15
andnot_avx2_fixed_loop:
	VMOVDQU (BX), Y0
	VMOVDQU (CX), Y1
	VPANDN Y0, Y1, Y2
	VMOVDQU Y2, (AX)
	POPCNTQ 0(AX), R9
	ADDQ R9, R8
	POPCNTQ 8(AX), R10
	POPCNTQ 16(AX), R11
	POPCNTQ 24(AX), R12
	ADDQ R10, R13
	ADDQ R11, R14
	ADDQ R12, R15
	ADDQ $32, AX
	ADDQ $32, BX
	ADDQ $32, CX
	DECQ DX
	JNZ andnot_avx2_fixed_loop
	ADDQ R13, R8
	ADDQ R14, R8
	ADDQ R15, R8
	VZEROUPPER
	MOVL R8, ret+72(FP)
	RET

// func applyAVX2FixedAsm(dst, base, add, remove []uint64) uint32
TEXT ·applyAVX2FixedAsm(SB), NOSPLIT, $0-100
	MOVQ dst_base+0(FP), AX
	MOVQ base_base+24(FP), BX
	MOVQ add_base+48(FP), CX
	MOVQ remove_base+72(FP), DI
	MOVQ $256, DX
	XORQ R8, R8
	XORQ R13, R13
	XORQ R14, R14
	XORQ R15, R15
apply_avx2_fixed_loop:
	VMOVDQU (BX), Y0
	VMOVDQU (CX), Y1
	VPOR Y1, Y0, Y0
	VMOVDQU (DI), Y1
	VPANDN Y0, Y1, Y2
	VMOVDQU Y2, (AX)
	POPCNTQ 0(AX), R9
	ADDQ R9, R8
	POPCNTQ 8(AX), R10
	POPCNTQ 16(AX), R11
	POPCNTQ 24(AX), R12
	ADDQ R10, R13
	ADDQ R11, R14
	ADDQ R12, R15
	ADDQ $32, AX
	ADDQ $32, BX
	ADDQ $32, CX
	ADDQ $32, DI
	DECQ DX
	JNZ apply_avx2_fixed_loop
	ADDQ R13, R8
	ADDQ R14, R8
	ADDQ R15, R8
	VZEROUPPER
	MOVL R8, ret+96(FP)
	RET

// func intersectsAVX2FixedAsm(a, b []uint64) bool
TEXT ·intersectsAVX2FixedAsm(SB), NOSPLIT, $0-49
	MOVQ a_base+0(FP), AX
	MOVQ b_base+24(FP), BX
	MOVQ $256, CX
	XORQ DX, DX
intersects_avx2_fixed_loop:
	VMOVDQU (AX), Y0
	VMOVDQU (BX), Y1
	VPAND Y1, Y0, Y0
	VPTEST Y0, Y0
	JNZ intersects_avx2_fixed_found
	ADDQ $32, AX
	ADDQ $32, BX
	DECQ CX
	JNZ intersects_avx2_fixed_loop
	MOVB DX, ret+48(FP)
	VZEROUPPER
	RET
intersects_avx2_fixed_found:
	MOVQ $1, DX
	MOVB DX, ret+48(FP)
	VZEROUPPER
	RET

// func andCardinalityAVX2FixedAsm(a, b []uint64) uint64
TEXT ·andCardinalityAVX2FixedAsm(SB), NOSPLIT, $0-56
	MOVQ a_base+0(FP), AX
	MOVQ b_base+24(FP), BX
	MOVQ $256, CX
	XORQ R8, R8
	XORQ R13, R13
	XORQ R14, R14
	XORQ R15, R15
and_card_avx2_fixed_loop:
	VMOVDQU (AX), Y0
	VMOVDQU (BX), Y1
	VPAND Y1, Y0, Y2
	VEXTRACTI128 $1, Y2, X3
	VPEXTRQ $0, X2, R9
	VPEXTRQ $1, X2, R10
	VPEXTRQ $0, X3, R11
	VPEXTRQ $1, X3, R12
	POPCNTQ R9, R9
	POPCNTQ R10, R10
	POPCNTQ R11, R11
	POPCNTQ R12, R12
	ADDQ R9, R8
	ADDQ R10, R13
	ADDQ R11, R14
	ADDQ R12, R15
	ADDQ $32, AX
	ADDQ $32, BX
	DECQ CX
	JNZ and_card_avx2_fixed_loop
	ADDQ R13, R8
	ADDQ R14, R8
	ADDQ R15, R8
	VZEROUPPER
	MOVQ R8, ret+48(FP)
	RET

// func orCardinalityAVX2FixedAsm(a, b []uint64) uint64
TEXT ·orCardinalityAVX2FixedAsm(SB), NOSPLIT, $0-56
	MOVQ a_base+0(FP), AX
	MOVQ b_base+24(FP), BX
	MOVQ $256, CX
	XORQ R8, R8
	XORQ R13, R13
	XORQ R14, R14
	XORQ R15, R15
or_card_avx2_fixed_loop:
	VMOVDQU (AX), Y0
	VMOVDQU (BX), Y1
	VPOR Y1, Y0, Y2
	VEXTRACTI128 $1, Y2, X3
	VPEXTRQ $0, X2, R9
	VPEXTRQ $1, X2, R10
	VPEXTRQ $0, X3, R11
	VPEXTRQ $1, X3, R12
	POPCNTQ R9, R9
	POPCNTQ R10, R10
	POPCNTQ R11, R11
	POPCNTQ R12, R12
	ADDQ R9, R8
	ADDQ R10, R13
	ADDQ R11, R14
	ADDQ R12, R15
	ADDQ $32, AX
	ADDQ $32, BX
	DECQ CX
	JNZ or_card_avx2_fixed_loop
	ADDQ R13, R8
	ADDQ R14, R8
	ADDQ R15, R8
	VZEROUPPER
	MOVQ R8, ret+48(FP)
	RET

// func andNotCardinalityAVX2FixedAsm(a, b []uint64) uint64
TEXT ·andNotCardinalityAVX2FixedAsm(SB), NOSPLIT, $0-56
	MOVQ a_base+0(FP), AX
	MOVQ b_base+24(FP), BX
	MOVQ $256, CX
	XORQ R8, R8
	XORQ R13, R13
	XORQ R14, R14
	XORQ R15, R15
andnot_card_avx2_fixed_loop:
	VMOVDQU (AX), Y0
	VMOVDQU (BX), Y1
	VPANDN Y0, Y1, Y2
	VEXTRACTI128 $1, Y2, X3
	VPEXTRQ $0, X2, R9
	VPEXTRQ $1, X2, R10
	VPEXTRQ $0, X3, R11
	VPEXTRQ $1, X3, R12
	POPCNTQ R9, R9
	POPCNTQ R10, R10
	POPCNTQ R11, R11
	POPCNTQ R12, R12
	ADDQ R9, R8
	ADDQ R10, R13
	ADDQ R11, R14
	ADDQ R12, R15
	ADDQ $32, AX
	ADDQ $32, BX
	DECQ CX
	JNZ andnot_card_avx2_fixed_loop
	ADDQ R13, R8
	ADDQ R14, R8
	ADDQ R15, R8
	VZEROUPPER
	MOVQ R8, ret+48(FP)
	RET

// func cardinalityAVX2FixedAsm(words []uint64) uint64
TEXT ·cardinalityAVX2FixedAsm(SB), NOSPLIT, $0-32
	MOVQ words_base+0(FP), AX
	MOVQ $256, CX
	XORQ R8, R8
	XORQ R13, R13
	XORQ R14, R14
	XORQ R15, R15
cardinality_avx2_fixed_loop:
	VMOVDQU (AX), Y0
	VEXTRACTI128 $1, Y0, X3
	VPEXTRQ $0, X0, R9
	VPEXTRQ $1, X0, R10
	VPEXTRQ $0, X3, R11
	VPEXTRQ $1, X3, R12
	POPCNTQ R9, R9
	POPCNTQ R10, R10
	POPCNTQ R11, R11
	POPCNTQ R12, R12
	ADDQ R9, R8
	ADDQ R10, R13
	ADDQ R11, R14
	ADDQ R12, R15
	ADDQ $32, AX
	DECQ CX
	JNZ cardinality_avx2_fixed_loop
	ADDQ R13, R8
	ADDQ R14, R8
	ADDQ R15, R8
	VZEROUPPER
	MOVQ R8, ret+24(FP)
	RET

// func andMany3AVX2FixedAsm(dst, a, b, c []uint64) uint32
TEXT ·andMany3AVX2FixedAsm(SB), NOSPLIT, $0-100
	MOVQ dst_base+0(FP), AX
	MOVQ a_base+24(FP), BX
	MOVQ b_base+48(FP), CX
	MOVQ c_base+72(FP), DI
	MOVQ $256, DX
	XORQ R8, R8
	XORQ R13, R13
	XORQ R14, R14
	XORQ R15, R15
and_many3_avx2_fixed_loop:
	VMOVDQU (BX), Y0
	VMOVDQU (CX), Y1
	VPAND Y1, Y0, Y0
	VMOVDQU (DI), Y1
	VPAND Y1, Y0, Y0
	VMOVDQU Y0, (AX)
	POPCNTQ 0(AX), R9
	ADDQ R9, R8
	POPCNTQ 8(AX), R10
	ADDQ R10, R13
	POPCNTQ 16(AX), R11
	ADDQ R11, R14
	POPCNTQ 24(AX), R12
	ADDQ R12, R15
	ADDQ $32, AX
	ADDQ $32, BX
	ADDQ $32, CX
	ADDQ $32, DI
	DECQ DX
	JNZ and_many3_avx2_fixed_loop
	ADDQ R13, R8
	ADDQ R14, R8
	ADDQ R15, R8
	VZEROUPPER
	MOVL R8, ret+96(FP)
	RET

// func andMany4AVX2FixedAsm(dst, a, b, c, d []uint64) uint32
TEXT ·andMany4AVX2FixedAsm(SB), NOSPLIT, $0-124
	MOVQ dst_base+0(FP), AX
	MOVQ a_base+24(FP), BX
	MOVQ b_base+48(FP), CX
	MOVQ c_base+72(FP), DI
	MOVQ d_base+96(FP), SI
	MOVQ $256, DX
	XORQ R8, R8
	XORQ R13, R13
	XORQ R14, R14
	XORQ R15, R15
and_many4_avx2_fixed_loop:
	VMOVDQU (BX), Y0
	VMOVDQU (CX), Y1
	VPAND Y1, Y0, Y0
	VMOVDQU (DI), Y1
	VPAND Y1, Y0, Y0
	VMOVDQU (SI), Y1
	VPAND Y1, Y0, Y0
	VMOVDQU Y0, (AX)
	POPCNTQ 0(AX), R9
	ADDQ R9, R8
	POPCNTQ 8(AX), R10
	ADDQ R10, R13
	POPCNTQ 16(AX), R11
	ADDQ R11, R14
	POPCNTQ 24(AX), R12
	ADDQ R12, R15
	ADDQ $32, AX
	ADDQ $32, BX
	ADDQ $32, CX
	ADDQ $32, DI
	ADDQ $32, SI
	DECQ DX
	JNZ and_many4_avx2_fixed_loop
	ADDQ R13, R8
	ADDQ R14, R8
	ADDQ R15, R8
	VZEROUPPER
	MOVL R8, ret+120(FP)
	RET
