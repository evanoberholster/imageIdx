package vectoridx

// simdInt8Block is the number of bytes processed by one architecture SIMD
// block. A zero block means the architecture has no int8 SIMD kernel.
func simdDotInt8Bytes(a, b []byte) float64 {
	block := int8SIMDBlock
	if !simdInt8Supported || block == 0 {
		return DotInt8(bytesAsInt8(a, len(a)), bytesAsInt8(b, len(b)))
	}
	n := len(a) - len(a)%block
	v := dotInt8SIMD(a, b)
	for i := n; i < len(a); i++ {
		v += int32(int8(a[i])) * int32(int8(b[i]))
	}
	return float64(v)
}

func simdDotUint8Bytes(a, b []byte) float64 {
	block := int8SIMDBlock
	if !simdInt8Supported || block == 0 {
		return DotUint8(a, b)
	}
	n := len(a) - len(a)%block
	v := dotUint8SIMD(a, b)
	for i := n; i < len(a); i++ {
		v += int32(a[i]) * int32(b[i])
	}
	return float64(v)
}

func simdL2Int8Bytes(a, b []byte) float64 {
	if !simdInt8Supported {
		return SquaredL2Int8(bytesAsInt8(a, len(a)), bytesAsInt8(b, len(b)))
	}
	aa := dotInt8Pair(a, a)
	bb := dotInt8Pair(b, b)
	ab := dotInt8Pair(a, b)
	return float64(aa + bb - 2*ab)
}

func simdL2Uint8Bytes(a, b []byte) float64 {
	if !simdInt8Supported {
		return SquaredL2Uint8(a, b)
	}
	aa := dotUint8Pair(a, a)
	bb := dotUint8Pair(b, b)
	ab := dotUint8Pair(a, b)
	return float64(aa + bb - 2*ab)
}

func dotInt8Pair(a, b []byte) int32 {
	block := int8SIMDBlock
	if block == 0 {
		return 0
	}
	n := len(a) - len(a)%block
	v := dotInt8SIMD(a, b)
	for i := n; i < len(a); i++ {
		v += int32(int8(a[i])) * int32(int8(b[i]))
	}
	return v
}

func dotUint8Pair(a, b []byte) int32 {
	block := int8SIMDBlock
	if block == 0 {
		return 0
	}
	n := len(a) - len(a)%block
	v := dotUint8SIMD(a, b)
	for i := n; i < len(a); i++ {
		v += int32(a[i]) * int32(b[i])
	}
	return v
}
