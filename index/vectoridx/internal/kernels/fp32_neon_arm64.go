//go:build arm64

package kernels

const simdSupported = true

// simdL2Float32Bytes computes squared L2 with NEON over 64-byte blocks and a
// scalar tail. The assembly accumulates in float32 lanes; the final value is
// widened to float64.
func simdL2Float32Bytes(a, b []byte) float64 {
	n := len(a) &^ 63
	var acc float64
	if n != 0 {
		acc = float64(l2NEON(a, b))
	}
	if n < len(a) {
		rem := (len(a) - n) / 4
		acc += SquaredL2Float32(BytesAsFloat32(a[n:], rem), BytesAsFloat32(b[n:], rem))
	}
	return acc
}

func simdDotFloat32Bytes(a, b []byte) float64 {
	n := len(a) &^ 63
	var acc float64
	if n != 0 {
		acc = float64(dotNEON(a, b))
	}
	if n < len(a) {
		rem := (len(a) - n) / 4
		acc += DotFloat32(BytesAsFloat32(a[n:], rem), BytesAsFloat32(b[n:], rem))
	}
	return acc
}

func l2NEON(a, b []byte) float32
func dotNEON(a, b []byte) float32
