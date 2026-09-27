package rankkernel

import "math"

// encodeDistanceScalar maps a native uint32 distance to a uint16 rank where
// higher is better and 0 is reserved for missing. Exact (0) maps to 65535.
// Positive distances map monotonically decreasing to 1..65534 using a cheap
// integer log2 scale: logFixed = (len-1)*2048 + 11 fractional bits. Rank =
// 65534 - logFixed, clamped to 1. Nearby distances keep more resolution than
// far distances because equal rank steps cover exponentially larger gaps.
func encodeDistanceScalar(d uint32) uint16 {
	if d == 0 {
		return 65535
	}
	l := uint32(32 - leadingZeros32(d)) // bit length, 1..32
	var frac uint32
	if l <= 12 {
		frac = (d - (uint32(1) << (l - 1))) << (11 - (l - 1))
	} else {
		frac = (d >> (l - 12)) & 0x7FF
	}
	logFixed := ((l - 1) << 11) | frac // 0..65535
	if logFixed >= 65533 {
		return 1
	}
	return uint16(65534 - logFixed)
}

func leadingZeros32(x uint32) uint32 {
	n := uint32(0)
	if x>>16 == 0 {
		n += 16
		x <<= 16
	}
	if x>>24 == 0 {
		n += 8
		x <<= 8
	}
	if x>>28 == 0 {
		n += 4
		x <<= 4
	}
	if x>>30 == 0 {
		n += 2
		x <<= 2
	}
	if x>>31 == 0 {
		n++
	}
	return n
}

func encodeDistancesScalar(dst []uint16, src []uint32) {
	for i, v := range src {
		dst[i] = encodeDistanceScalar(v)
	}
}

// quantizeCosineScalar maps cosine similarity in [-1,+1] to higher-is-better
// rank: +1 -> 65535, -1 -> 1. NaN maps to 0 (missing). Out-of-range values
// clamp. Numeric quantization only; no float-bit reinterpretation.
func quantizeCosineScalar(sim float32) uint16 {
	if math.IsNaN(float64(sim)) {
		return 0
	}
	if sim >= 1 {
		return 65535
	}
	if sim <= -1 {
		return 1
	}
	t := (float64(sim) + 1) * 0.5 // 0..1
	r := int32(t*65534.0 + 0.5)
	return uint16(1 + r)
}

func quantizeCosinesScalar(dst []uint16, src []float32) {
	for i, v := range src {
		dst[i] = quantizeCosineScalar(v)
	}
}
