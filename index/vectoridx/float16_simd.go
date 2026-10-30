package vectoridx

func simdL2Float16Bytes(a, b []byte) float64 {
	block := simdFloat16Block
	if !simdFloat16Supported || block == 0 {
		return SquaredL2Float16(bytesAsUint16(a, len(a)/2), bytesAsUint16(b, len(b)/2))
	}
	n := len(a) - len(a)%block
	acc := float64(l2Float16SIMD(a, b))
	if n < len(a) {
		rem := (len(a) - n) / 2
		acc += SquaredL2Float16(bytesAsUint16(a[n:], rem), bytesAsUint16(b[n:], rem))
	}
	return acc
}

func simdDotFloat16Bytes(a, b []byte) float64 {
	block := simdFloat16Block
	if !simdFloat16Supported || block == 0 {
		return DotFloat16(bytesAsUint16(a, len(a)/2), bytesAsUint16(b, len(b)/2))
	}
	n := len(a) - len(a)%block
	acc := float64(dotFloat16SIMD(a, b))
	if n < len(a) {
		rem := (len(a) - n) / 2
		acc += DotFloat16(bytesAsUint16(a[n:], rem), bytesAsUint16(b[n:], rem))
	}
	return acc
}
