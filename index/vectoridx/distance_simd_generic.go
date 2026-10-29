//go:build !arm64 && !amd64

package vectoridx

const simdSupported = false

func simdL2Float32Bytes(a, b []byte) float64 {
	return SquaredL2Float32(bytesAsFloat32(a, len(a)/4), bytesAsFloat32(b, len(b)/4))
}

func simdDotFloat32Bytes(a, b []byte) float64 {
	return DotFloat32(bytesAsFloat32(a, len(a)/4), bytesAsFloat32(b, len(b)/4))
}
