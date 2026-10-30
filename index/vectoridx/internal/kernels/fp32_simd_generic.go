//go:build !arm64 && !amd64

package kernels

const simdSupported = false

func simdL2Float32Bytes(a, b []byte) float64 {
	return SquaredL2Float32(BytesAsFloat32(a, len(a)/4), BytesAsFloat32(b, len(b)/4))
}

func simdDotFloat32Bytes(a, b []byte) float64 {
	return DotFloat32(BytesAsFloat32(a, len(a)/4), BytesAsFloat32(b, len(b)/4))
}
