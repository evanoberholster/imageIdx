//go:build amd64

package vectoridx

import "golang.org/x/sys/cpu"

var simdSupported = cpu.X86.HasAVX2

// simdL2Float32Bytes computes squared L2 with AVX2 over 128-byte blocks and
// a scalar tail. The assembly accumulates in float32 lanes; the final value
// is widened to float64.
func simdL2Float32Bytes(a, b []byte) float64 {
	if !simdSupported || len(a) < 128 {
		return SquaredL2Float32(bytesAsFloat32(a, len(a)/4), bytesAsFloat32(b, len(b)/4))
	}
	n := len(a) &^ 127
	acc := float64(l2AVX2(a, b))
	if n < len(a) {
		rem := (len(a) - n) / 4
		acc += SquaredL2Float32(bytesAsFloat32(a[n:], rem), bytesAsFloat32(b[n:], rem))
	}
	return acc
}

func simdDotFloat32Bytes(a, b []byte) float64 {
	if !simdSupported || len(a) < 128 {
		return DotFloat32(bytesAsFloat32(a, len(a)/4), bytesAsFloat32(b, len(b)/4))
	}
	n := len(a) &^ 127
	acc := float64(dotAVX2(a, b))
	if n < len(a) {
		rem := (len(a) - n) / 4
		acc += DotFloat32(bytesAsFloat32(a[n:], rem), bytesAsFloat32(b[n:], rem))
	}
	return acc
}

func l2AVX2(a, b []byte) float32
func dotAVX2(a, b []byte) float32
