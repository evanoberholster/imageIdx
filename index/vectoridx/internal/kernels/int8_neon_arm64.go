//go:build arm64

package kernels

import (
	"runtime"

	"golang.org/x/sys/cpu"
)

const int8SIMDBlock = 64

// Darwin arm64 targets Apple Silicon, which provides ASIMD dot-product, but
// the generic hwcap probe does not report it there. Linux uses the hwcap flag.
var simdInt8Supported = runtime.GOOS == "darwin" || cpu.ARM64.HasASIMDDP

func dotInt8SIMD(a, b []byte) int32  { return dotInt8NEON(a, b) }
func dotUint8SIMD(a, b []byte) int32 { return dotUint8NEON(a, b) }

func dotInt8NEON(a, b []byte) int32
func dotUint8NEON(a, b []byte) int32
