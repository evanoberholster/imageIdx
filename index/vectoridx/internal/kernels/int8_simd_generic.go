//go:build !arm64 && !amd64

package kernels

const (
	int8SIMDBlock     = 0
	simdInt8Supported = false
)

func dotInt8SIMD(a, b []byte) int32  { return 0 }
func dotUint8SIMD(a, b []byte) int32 { return 0 }
