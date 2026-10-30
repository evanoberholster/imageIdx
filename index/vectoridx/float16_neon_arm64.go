//go:build arm64

package vectoridx

import (
	"runtime"

	"golang.org/x/sys/cpu"
)

const simdFloat16Block = 32

var simdFloat16Supported = runtime.GOOS == "darwin" || cpu.ARM64.HasASIMDHP

func l2Float16SIMD(a, b []byte) float32  { return l2Float16NEON(a, b) }
func dotFloat16SIMD(a, b []byte) float32 { return dotFloat16NEON(a, b) }
func l2Float16NEON(a, b []byte) float32
func dotFloat16NEON(a, b []byte) float32
