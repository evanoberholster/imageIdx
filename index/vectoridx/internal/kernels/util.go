package kernels

import "unsafe"

// Func is a raw little-endian vector kernel. Distance kernels return the index
// metric distance (smaller is better: squared L2, 1-cos, or -dot); positive-dot
// kernels are used with cached norms. Slices must be exactly the vector byte
// length.
type Func func(a, b []byte) float64

// CosineFromDotNorms converts a dot product and two precomputed norms to the
// index cosine distance (1-cos). Zero vectors map to distance 1; two zero
// vectors map to 0. The endpoint snap keeps self-search exactly zero when SIMD
// and scalar accumulation differ by a few ulps.
func CosineFromDotNorms(dot, na, nb float64) float64 {
	if na == 0 && nb == 0 {
		return 0
	}
	if na == 0 || nb == 0 {
		return 1
	}
	cos := dot / (na * nb)
	const cosineSnap = 1e-7
	if cos > 1-cosineSnap {
		cos = 1
	} else if cos < -1+cosineSnap {
		cos = -1
	}
	return 1 - cos
}

// The raw-byte helpers reinterpret little-endian storage without copying. The
// format requires a little-endian host; Open/Create reject other hosts.

func BytesAsInt8(b []byte, n int) []int8 {
	return unsafe.Slice((*int8)(unsafe.Pointer(&b[0])), n)
}

func BytesAsUint8(b []byte, n int) []uint8 { return b[:n] }

func BytesAsInt16(b []byte, n int) []int16 {
	return unsafe.Slice((*int16)(unsafe.Pointer(&b[0])), n)
}

func BytesAsUint16(b []byte, n int) []uint16 {
	return unsafe.Slice((*uint16)(unsafe.Pointer(&b[0])), n)
}

func BytesAsFloat32(b []byte, n int) []float32 {
	return unsafe.Slice((*float32)(unsafe.Pointer(&b[0])), n)
}
