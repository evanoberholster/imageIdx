package kernels

// int8L2Block is the number of bytes processed by one architecture L2 block.
const int8L2Block = 32

// simdL2Int8Bytes computes exact squared L2 in one pass over int8 inputs.
func simdL2Int8Bytes(a, b []byte) float64 {
	if !simdInt8Supported {
		return SquaredL2Int8(BytesAsInt8(a, len(a)), BytesAsInt8(b, len(b)))
	}
	n := len(a) - len(a)%int8L2Block
	v := l2Int8SIMD(a, b)
	for i := n; i < len(a); i++ {
		d := int32(int8(a[i])) - int32(int8(b[i]))
		v += d * d
	}
	return float64(v)
}

// simdL2Uint8Bytes computes exact squared L2 in one pass over uint8 inputs.
func simdL2Uint8Bytes(a, b []byte) float64 {
	if !simdInt8Supported {
		return SquaredL2Uint8(a, b)
	}
	n := len(a) - len(a)%int8L2Block
	v := l2Uint8SIMD(a, b)
	for i := n; i < len(a); i++ {
		d := int32(a[i]) - int32(b[i])
		v += d * d
	}
	return float64(v)
}

func simdDotInt8Bytes(a, b []byte) float64 {
	block := int8SIMDBlock
	if !simdInt8Supported || block == 0 {
		return DotInt8(BytesAsInt8(a, len(a)), BytesAsInt8(b, len(b)))
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
