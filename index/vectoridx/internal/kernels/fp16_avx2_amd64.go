//go:build amd64

package kernels

import "golang.org/x/sys/cpu"

const simdFloat16Block = 32

// AVX2 plus F16C. AVX2 hosts in the supported deployment range provide F16C.
var simdFloat16Supported = cpu.X86.HasAVX2

func l2Float16SIMD(a, b []byte) float32  { return l2Float16AVX2(a, b) }
func dotFloat16SIMD(a, b []byte) float32 { return dotFloat16AVX2(a, b) }

func l2Float16AVX2(a, b []byte) float32
func dotFloat16AVX2(a, b []byte) float32
