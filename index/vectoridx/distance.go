package vectoridx

import (
	"math"
	"unsafe"
)

// This file holds the scalar distance kernels. They are the correctness
// reference for future v2 AVX2/NEON kernels (VectorIdx.MD section 22).
// Dispatch is resolved once per index open; the hot loop calls a single
// pre-resolved func(a, b []byte) float64 with no interface or type switch.

// distRaw computes the index-metric distance between two raw little-endian
// vectors of the index's configured length. Smaller is better for every
// metric: L2 returns squared L2, cosine returns 1-cos in [0,2], dot returns
// -dot. Slices must be exactly VectorBytes long.
type distRaw func(a, b []byte) float64

// resolveDistance returns the scalar kernel for cfg. Callers must validate
// cfg first; an unknown element type or metric yields nil.
func resolveDistance(cfg Config) distRaw {
	n := int(cfg.Dimensions)
	switch cfg.Metric {
	case MetricL2, 0:
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
	case MetricCosine:
		switch cfg.ElementType {
		case ElementInt8:
			return func(a, b []byte) float64 { return cosineInt8Bytes(a, b, n) }
		case ElementUint8:
			return func(a, b []byte) float64 { return cosineUint8Bytes(a, b, n) }
		case ElementInt16:
			return func(a, b []byte) float64 { return cosineInt16Bytes(a, b, n) }
		case ElementUint16:
			return func(a, b []byte) float64 { return cosineUint16Bytes(a, b, n) }
		case ElementFloat32:
			return func(a, b []byte) float64 { return cosineFloat32Bytes(a, b, n) }
		default:
			return nil
		}
	case MetricDot:
		switch cfg.ElementType {
		case ElementInt8:
			return func(a, b []byte) float64 { return negDotInt8Bytes(a, b, n) }
		case ElementUint8:
			return func(a, b []byte) float64 { return negDotUint8Bytes(a, b, n) }
		case ElementInt16:
			return func(a, b []byte) float64 { return negDotInt16Bytes(a, b, n) }
		case ElementUint16:
			return func(a, b []byte) float64 { return negDotUint16Bytes(a, b, n) }
		case ElementFloat32:
			return func(a, b []byte) float64 { return negDotFloat32Bytes(a, b, n) }
		default:
			return nil
		}
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

// Dot-product kernels. All use a float64 accumulator; integer inputs are
// exact (max int16 dot ~1.1e12 < 2^53). Positive means similar; the index
// stores -dot so smaller is better.

// DotInt8 returns sum a[i]*b[i] over int8 vectors.
func DotInt8(a, b []int8) float64 {
	var acc float64
	for i := range a {
		acc += float64(a[i]) * float64(b[i])
	}
	return acc
}

// DotUint8 returns sum a[i]*b[i] over uint8 vectors.
func DotUint8(a, b []uint8) float64 {
	var acc float64
	for i := range a {
		acc += float64(a[i]) * float64(b[i])
	}
	return acc
}

// DotInt16 returns sum a[i]*b[i] over int16 vectors.
func DotInt16(a, b []int16) float64 {
	var acc float64
	for i := range a {
		acc += float64(a[i]) * float64(b[i])
	}
	return acc
}

// DotUint16 returns sum a[i]*b[i] over uint16 vectors.
func DotUint16(a, b []uint16) float64 {
	var acc float64
	for i := range a {
		acc += float64(a[i]) * float64(b[i])
	}
	return acc
}

// DotFloat32 returns sum a[i]*b[i] accumulated in float64.
func DotFloat32(a, b []float32) float64 {
	var acc float64
	for i := range a {
		acc += float64(a[i]) * float64(b[i])
	}
	return acc
}

// Cosine kernels return 1-cos in [0,2]; 0 for identical, 1 for orthogonal.
// Zero vectors map to distance 1 (both zero map to 0).

// CosineInt8 returns 1-cos over int8 vectors.
func CosineInt8(a, b []int8) float64 {
	return cosineFromDotNorms(DotInt8(a, b), normInt8(a), normInt8(b))
}

// CosineUint8 returns 1-cos over uint8 vectors.
func CosineUint8(a, b []uint8) float64 {
	return cosineFromDotNorms(DotUint8(a, b), normUint8(a), normUint8(b))
}

// CosineInt16 returns 1-cos over int16 vectors.
func CosineInt16(a, b []int16) float64 {
	return cosineFromDotNorms(DotInt16(a, b), normInt16(a), normInt16(b))
}

// CosineUint16 returns 1-cos over uint16 vectors.
func CosineUint16(a, b []uint16) float64 {
	return cosineFromDotNorms(DotUint16(a, b), normUint16(a), normUint16(b))
}

// CosineFloat32 returns 1-cos over float32 vectors.
func CosineFloat32(a, b []float32) float64 {
	return cosineFromDotNorms(DotFloat32(a, b), normFloat32(a), normFloat32(b))
}

func normInt8(a []int8) float64 {
	var acc float64
	for _, x := range a {
		acc += float64(x) * float64(x)
	}
	return math.Sqrt(acc)
}

func normUint8(a []uint8) float64 {
	var acc float64
	for _, x := range a {
		acc += float64(x) * float64(x)
	}
	return math.Sqrt(acc)
}

func normInt16(a []int16) float64 {
	var acc float64
	for _, x := range a {
		acc += float64(x) * float64(x)
	}
	return math.Sqrt(acc)
}

func normUint16(a []uint16) float64 {
	var acc float64
	for _, x := range a {
		acc += float64(x) * float64(x)
	}
	return math.Sqrt(acc)
}

func normFloat32(a []float32) float64 {
	var acc float64
	for _, x := range a {
		acc += float64(x) * float64(x)
	}
	return math.Sqrt(acc)
}

func cosineFromDotNorms(dot, na, nb float64) float64 {
	if na == 0 && nb == 0 {
		return 0
	}
	if na == 0 || nb == 0 {
		return 1
	}
	cos := dot / (na * nb)
	if cos > 1 {
		cos = 1
	} else if cos < -1 {
		cos = -1
	}
	return 1 - cos
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

func negDotInt8Bytes(a, b []byte, n int) float64 {
	return -DotInt8(bytesAsInt8(a, n), bytesAsInt8(b, n))
}

func negDotUint8Bytes(a, b []byte, n int) float64 {
	return -DotUint8(bytesAsUint8(a, n), bytesAsUint8(b, n))
}

func negDotInt16Bytes(a, b []byte, n int) float64 {
	return -DotInt16(bytesAsInt16(a, n), bytesAsInt16(b, n))
}

func negDotUint16Bytes(a, b []byte, n int) float64 {
	return -DotUint16(bytesAsUint16(a, n), bytesAsUint16(b, n))
}

func negDotFloat32Bytes(a, b []byte, n int) float64 {
	return -DotFloat32(bytesAsFloat32(a, n), bytesAsFloat32(b, n))
}

func cosineInt8Bytes(a, b []byte, n int) float64 {
	return CosineInt8(bytesAsInt8(a, n), bytesAsInt8(b, n))
}

func cosineUint8Bytes(a, b []byte, n int) float64 {
	return CosineUint8(bytesAsUint8(a, n), bytesAsUint8(b, n))
}

func cosineInt16Bytes(a, b []byte, n int) float64 {
	return CosineInt16(bytesAsInt16(a, n), bytesAsInt16(b, n))
}

func cosineUint16Bytes(a, b []byte, n int) float64 {
	return CosineUint16(bytesAsUint16(a, n), bytesAsUint16(b, n))
}

func cosineFloat32Bytes(a, b []byte, n int) float64 {
	return CosineFloat32(bytesAsFloat32(a, n), bytesAsFloat32(b, n))
}

// dotSelfBytes returns |q|^2 for the query, used to zero-base dot scores so
// self-search scores 0 while traversal ordering (offset-invariant) is kept.
func dotSelfBytes(cfg Config, q []byte) float64 {
	n := int(cfg.Dimensions)
	switch cfg.ElementType {
	case ElementInt8:
		v := bytesAsInt8(q, n)
		return DotInt8(v, v)
	case ElementUint8:
		v := bytesAsUint8(q, n)
		return DotUint8(v, v)
	case ElementInt16:
		v := bytesAsInt16(q, n)
		return DotInt16(v, v)
	case ElementUint16:
		v := bytesAsUint16(q, n)
		return DotUint16(v, v)
	case ElementFloat32:
		v := bytesAsFloat32(q, n)
		return DotFloat32(v, v)
	default:
		return 0
	}
}

// EncodeScore maps an internal L2/dot distance to the public uint32
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

// EncodeCosineScore maps a cosine distance in [0,2] to uint32 with 2^30 scale
// for ranking resolution. Zero distance (identical) maps to zero.
func EncodeCosineScore(dist float64) uint32 {
	if math.IsNaN(dist) {
		return math.MaxUint32
	}
	if dist <= 0 {
		return 0
	}
	scaled := dist * (1 << 30)
	if scaled >= float64(math.MaxUint32) {
		return math.MaxUint32
	}
	return uint32(scaled)
}

// EncodeDotScore maps an internal -dot distance to uint32 by zero-basing with
// the query self-dot: score = |q|^2 - dot(q,v), zero for self, monotonic per
// query. Callers compute querySelfDot once via dotSelfBytes.
func EncodeDotScore(negDotDist, querySelfDot float64) uint32 {
	return EncodeScore(querySelfDot + negDotDist)
}

// encodeScoreForMetric maps a raw traversal distance to the public score for
// the index metric. querySelfDot is used only for dot.
func encodeScoreForMetric(metric uint8, dist, querySelfDot float64) uint32 {
	switch metric {
	case MetricCosine:
		return EncodeCosineScore(dist)
	case MetricDot:
		return EncodeDotScore(dist, querySelfDot)
	default:
		return EncodeScore(dist)
	}
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
