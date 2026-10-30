package vectoridx

import (
	"math"

	"github.com/evanoberholster/imageIdx/index/vectoridx/internal/kernels"
)

// This file owns the public scalar kernel API, metric dispatch, and score
// encoding. Scalar and SIMD implementations live in internal/kernels.

// distRaw is a raw little-endian vector kernel. Distance kernels return the
// index-metric distance (smaller is better: squared L2, 1-cos, or -dot);
// resolveDot returns the positive dot-product kernel used with cached norms.
// Slices must be exactly VectorBytes long.
type distRaw = kernels.Func

// resolveDistance returns the kernel for cfg. Callers must validate cfg first;
// an unknown element type or metric yields nil.
func resolveDistance(cfg Config) distRaw {
	e, ok := kernelElement(cfg.ElementType)
	if !ok {
		return nil
	}
	n := int(cfg.Dimensions)
	switch cfg.Metric {
	case MetricL2, 0:
		return kernels.L2(e, n)
	case MetricCosine:
		return kernels.Cosine(e, n)
	case MetricDot:
		return kernels.NegDot(e, n)
	default:
		return nil
	}
}

// resolveDot returns the raw positive dot-product kernel for cfg. The cosine
// metric caches each stored vector's norm, so comparisons need only this one
// pass; callers combine it with cosineFromDotNorms. Unknown element types
// return nil.
func resolveDot(cfg Config) distRaw {
	e, ok := kernelElement(cfg.ElementType)
	if !ok {
		return nil
	}
	return kernels.Dot(e, int(cfg.Dimensions))
}

func kernelElement(t ElementType) (kernels.Element, bool) {
	switch t {
	case ElementInt8:
		return kernels.Int8, true
	case ElementUint8:
		return kernels.Uint8, true
	case ElementInt16:
		return kernels.Int16, true
	case ElementUint16:
		return kernels.Uint16, true
	case ElementFloat32:
		return kernels.Float32, true
	case ElementFloat16:
		return kernels.Float16, true
	default:
		return 0, false
	}
}

// Typed scalar kernels. These forward to the internal scalar reference
// implementations so callers keep a stable vectoridx API.

// SquaredL2Int8 returns sum (a[i]-b[i])^2 over int8 vectors.
func SquaredL2Int8(a, b []int8) float64 { return kernels.SquaredL2Int8(a, b) }

// SquaredL2Uint8 returns sum (a[i]-b[i])^2 over uint8 vectors.
func SquaredL2Uint8(a, b []uint8) float64 { return kernels.SquaredL2Uint8(a, b) }

// SquaredL2Int16 returns sum (a[i]-b[i])^2 over int16 vectors.
func SquaredL2Int16(a, b []int16) float64 { return kernels.SquaredL2Int16(a, b) }

// SquaredL2Uint16 returns sum (a[i]-b[i])^2 over uint16 vectors.
func SquaredL2Uint16(a, b []uint16) float64 { return kernels.SquaredL2Uint16(a, b) }

// SquaredL2Float32 returns sum (a[i]-b[i])^2 accumulated in float64.
func SquaredL2Float32(a, b []float32) float64 { return kernels.SquaredL2Float32(a, b) }

// SquaredL2Float16 returns sum (a[i]-b[i])^2 over binary16 values.
func SquaredL2Float16(a, b []uint16) float64 { return kernels.SquaredL2Float16(a, b) }

// DotInt8 returns sum a[i]*b[i] over int8 vectors.
func DotInt8(a, b []int8) float64 { return kernels.DotInt8(a, b) }

// DotUint8 returns sum a[i]*b[i] over uint8 vectors.
func DotUint8(a, b []uint8) float64 { return kernels.DotUint8(a, b) }

// DotInt16 returns sum a[i]*b[i] over int16 vectors.
func DotInt16(a, b []int16) float64 { return kernels.DotInt16(a, b) }

// DotUint16 returns sum a[i]*b[i] over uint16 vectors.
func DotUint16(a, b []uint16) float64 { return kernels.DotUint16(a, b) }

// DotFloat32 returns sum a[i]*b[i] accumulated in float64.
func DotFloat32(a, b []float32) float64 { return kernels.DotFloat32(a, b) }

// DotFloat16 returns sum a[i]*b[i] over binary16 values.
func DotFloat16(a, b []uint16) float64 { return kernels.DotFloat16(a, b) }

// CosineInt8 returns 1-cos over int8 vectors.
func CosineInt8(a, b []int8) float64 { return kernels.CosineInt8(a, b) }

// CosineUint8 returns 1-cos over uint8 vectors.
func CosineUint8(a, b []uint8) float64 { return kernels.CosineUint8(a, b) }

// CosineInt16 returns 1-cos over int16 vectors.
func CosineInt16(a, b []int16) float64 { return kernels.CosineInt16(a, b) }

// CosineUint16 returns 1-cos over uint16 vectors.
func CosineUint16(a, b []uint16) float64 { return kernels.CosineUint16(a, b) }

// CosineFloat32 returns 1-cos over float32 vectors.
func CosineFloat32(a, b []float32) float64 { return kernels.CosineFloat32(a, b) }

// CosineFloat16 returns 1-cos over binary16 vectors.
func CosineFloat16(a, b []uint16) float64 { return kernels.CosineFloat16(a, b) }

// Raw-byte helpers reinterpret LE storage without copying. The format
// requires a little-endian host; Open/Create reject other hosts.

func bytesAsInt8(b []byte, n int) []int8 { return kernels.BytesAsInt8(b, n) }

func bytesAsUint8(b []byte, n int) []uint8 { return kernels.BytesAsUint8(b, n) }

func bytesAsInt16(b []byte, n int) []int16 { return kernels.BytesAsInt16(b, n) }

func bytesAsUint16(b []byte, n int) []uint16 { return kernels.BytesAsUint16(b, n) }

func bytesAsFloat32(b []byte, n int) []float32 { return kernels.BytesAsFloat32(b, n) }

func cosineFromDotNorms(dot, na, nb float64) float64 {
	return kernels.CosineFromDotNorms(dot, na, nb)
}

func normFloat32(a []float32) float64 { return math.Sqrt(DotFloat32(a, a)) }

// dotSelfBytes returns |q|^2 for the query, used to zero-base dot scores so
// self-search scores 0 while traversal ordering (offset-invariant) is kept.
func dotSelfBytes(cfg Config, q []byte) float64 {
	e, ok := kernelElement(cfg.ElementType)
	if !ok {
		return 0
	}
	return kernels.SelfDot(e, q, int(cfg.Dimensions))
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

func encodeRawInt8(dst []byte, q []int8) []byte {
	need := len(q)
	if cap(dst) < need {
		dst = make([]byte, need)
	} else {
		dst = dst[:need]
	}
	copy(bytesAsInt8(dst, len(q)), q)
	return dst
}

func encodeRawUint8(dst []byte, q []uint8) []byte {
	need := len(q)
	if cap(dst) < need {
		dst = make([]byte, need)
	} else {
		dst = dst[:need]
	}
	copy(dst, q)
	return dst
}

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

func encodeRawFloat16(dst []byte, q []uint16) []byte {
	need := len(q) * 2
	if cap(dst) < need {
		dst = make([]byte, need)
	} else {
		dst = dst[:need]
	}
	copy(bytesAsUint16(dst, len(q)), q)
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
