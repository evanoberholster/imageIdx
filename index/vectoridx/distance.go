package vectoridx

import (
	"math"
	"unsafe"
)

// This file holds the V1 scalar L2 kernels. They are the correctness
// reference for future AVX2/NEON kernels (VectorIdx.MD section 22).
// Dispatch is resolved once per index open; the hot loop calls a single
// pre-resolved func(a, b []byte) float64 with no interface or type switch.

// distRaw computes squared L2 distance between two raw little-endian vectors
// of the index's configured length. Slices must be exactly VectorBytes long.
type distRaw func(a, b []byte) float64

// resolveDistance returns the scalar kernel for cfg. Callers must validate
// cfg first; an unknown element type yields nil.
func resolveDistance(cfg Config) distRaw {
	n := int(cfg.Dimensions)
	switch cfg.ElementType {
	case ElementInt8:
		return func(a, b []byte) float64 { return squaredL2Int8Bytes(a, b, n) }
	case ElementUint8:
		return func(a, b []byte) float64 { return squaredL2Uint8Bytes(a, b, n) }
	case ElementInt16:
		return func(a, b []byte) float64 { return squaredL2Int16Bytes(a, b, n) }
	case ElementUint16:
		return func(a, b []byte) float64 { return squaredL2Uint16Bytes(a, b, n) }
	case ElementFloat32:
		return func(a, b []byte) float64 { return squaredL2Float32Bytes(a, b, n) }
	default:
		return nil
	}
}

// Typed scalar kernels. All use a float64 accumulator so integer inputs are
// exact (max int16/1024D distance ~4.4e12 < 2^53) and float32 inputs keep
// reference precision for ranking.

// SquaredL2Int8 returns sum (a[i]-b[i])^2 over int8 vectors.
func SquaredL2Int8(a, b []int8) float64 {
	var acc float64
	for i := range a {
		d := float64(int16(a[i]) - int16(b[i]))
		acc += d * d
	}
	return acc
}

// SquaredL2Uint8 returns sum (a[i]-b[i])^2 over uint8 vectors.
func SquaredL2Uint8(a, b []uint8) float64 {
	var acc float64
	for i := range a {
		d := float64(int16(a[i]) - int16(b[i]))
		acc += d * d
	}
	return acc
}

// SquaredL2Int16 returns sum (a[i]-b[i])^2 over int16 vectors.
func SquaredL2Int16(a, b []int16) float64 {
	var acc float64
	for i := range a {
		d := float64(int32(a[i]) - int32(b[i]))
		acc += d * d
	}
	return acc
}

// SquaredL2Uint16 returns sum (a[i]-b[i])^2 over uint16 vectors.
func SquaredL2Uint16(a, b []uint16) float64 {
	var acc float64
	for i := range a {
		d := float64(int32(a[i]) - int32(b[i]))
		acc += d * d
	}
	return acc
}

// SquaredL2Float32 returns sum (a[i]-b[i])^2 accumulated in float64.
func SquaredL2Float32(a, b []float32) float64 {
	var acc float64
	for i := range a {
		d := float64(a[i]) - float64(b[i])
		acc += d * d
	}
	return acc
}

// Raw-byte kernels reinterpret LE storage without copying. The format
// requires a little-endian host; Open/Create reject other hosts.

func bytesAsInt8(b []byte, n int) []int8 {
	return unsafe.Slice((*int8)(unsafe.Pointer(&b[0])), n)
}

func bytesAsUint8(b []byte, n int) []uint8 { return b[:n] }

func bytesAsInt16(b []byte, n int) []int16 {
	return unsafe.Slice((*int16)(unsafe.Pointer(&b[0])), n)
}

func bytesAsUint16(b []byte, n int) []uint16 {
	return unsafe.Slice((*uint16)(unsafe.Pointer(&b[0])), n)
}

func bytesAsFloat32(b []byte, n int) []float32 {
	return unsafe.Slice((*float32)(unsafe.Pointer(&b[0])), n)
}

func squaredL2Int8Bytes(a, b []byte, n int) float64 {
	return SquaredL2Int8(bytesAsInt8(a, n), bytesAsInt8(b, n))
}

func squaredL2Uint8Bytes(a, b []byte, n int) float64 {
	return SquaredL2Uint8(bytesAsUint8(a, n), bytesAsUint8(b, n))
}

func squaredL2Int16Bytes(a, b []byte, n int) float64 {
	return SquaredL2Int16(bytesAsInt16(a, n), bytesAsInt16(b, n))
}

func squaredL2Uint16Bytes(a, b []byte, n int) float64 {
	return SquaredL2Uint16(bytesAsUint16(a, n), bytesAsUint16(b, n))
}

func squaredL2Float32Bytes(a, b []byte, n int) float64 {
	return SquaredL2Float32(bytesAsFloat32(a, n), bytesAsFloat32(b, n))
}

// EncodeScore maps an internal squared-L2 distance to the public uint32
// score. Mapping is monotonic: smaller distance => smaller-or-equal score.
// Values beyond math.MaxUint32 saturate; this only affects extremely distant
// int16/uint16 pairs (max ~4.4e12 at 1024D) and far float outliers, which tie
// at the ceiling rather than wrap. NaN maps to the ceiling so corrupt
// distances sort last instead of breaking heap order.
func EncodeScore(dist float64) uint32 {
	if math.IsNaN(dist) || dist >= float64(math.MaxUint32) {
		return math.MaxUint32
	}
	if dist <= 0 {
		return 0
	}
	return uint32(dist)
}

// encodeQuery helpers copy typed query vectors into a scratch raw buffer
// so the hot path always works on raw bytes.

func encodeRawInt16(dst []byte, q []int16) []byte {
	need := len(q) * 2
	if cap(dst) < need {
		dst = make([]byte, need)
	} else {
		dst = dst[:need]
	}
	out := bytesAsInt16(dst, len(q))
	copy(out, q)
	return dst
}

func encodeRawUint16(dst []byte, q []uint16) []byte {
	need := len(q) * 2
	if cap(dst) < need {
		dst = make([]byte, need)
	} else {
		dst = dst[:need]
	}
	out := bytesAsUint16(dst, len(q))
	copy(out, q)
	return dst
}

func encodeRawFloat32(dst []byte, q []float32) []byte {
	need := len(q) * 4
	if cap(dst) < need {
		dst = make([]byte, need)
	} else {
		dst = dst[:need]
	}
	out := bytesAsFloat32(dst, len(q))
	copy(out, q)
	return dst
}

// MaxExactDistances documents accumulator headroom for the overflow proof
// tested in distance_test.go: worst-case squared L2 at 1024D.
const (
	maxDistInt8U8   float64 = 65025 * 1024 // 66,585,600 fits uint32
	maxDistInt16U16 float64 = 4294836225 * 1024
)
