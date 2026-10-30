//go:build !arm64 && !amd64

package kernels

const (
	simdFloat16Block     = 0
	simdFloat16Supported = false
)

func l2Float16SIMD(a, b []byte) float32  { return 0 }
func dotFloat16SIMD(a, b []byte) float32 { return 0 }
