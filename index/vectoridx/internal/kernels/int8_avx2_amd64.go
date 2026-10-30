//go:build amd64

package kernels

import "golang.org/x/sys/cpu"

const int8SIMDBlock = 32

var simdInt8Supported = cpu.X86.HasAVX2

func dotInt8SIMD(a, b []byte) int32  { return dotInt8AVX2(a, b) }
func dotUint8SIMD(a, b []byte) int32 { return dotUint8AVX2(a, b) }

func dotInt8AVX2(a, b []byte) int32
func dotUint8AVX2(a, b []byte) int32

func l2Int8SIMD(a, b []byte) int32  { return l2Int8AVX2(a, b) }
func l2Uint8SIMD(a, b []byte) int32 { return l2Uint8AVX2(a, b) }

func l2Int8AVX2(a, b []byte) int32
func l2Uint8AVX2(a, b []byte) int32
