package kernels

import "math"

// Typed integer scalar kernels. All use a float64 accumulator so integer inputs
// are exact (max int16/1024D distance ~4.4e12 < 2^53).

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

// CosineInt8 returns 1-cos over int8 vectors.
func CosineInt8(a, b []int8) float64 {
	return CosineFromDotNorms(DotInt8(a, b), normInt8(a), normInt8(b))
}

// CosineUint8 returns 1-cos over uint8 vectors.
func CosineUint8(a, b []uint8) float64 {
	return CosineFromDotNorms(DotUint8(a, b), normUint8(a), normUint8(b))
}

// CosineInt16 returns 1-cos over int16 vectors.
func CosineInt16(a, b []int16) float64 {
	return CosineFromDotNorms(DotInt16(a, b), normInt16(a), normInt16(b))
}

// CosineUint16 returns 1-cos over uint16 vectors.
func CosineUint16(a, b []uint16) float64 {
	return CosineFromDotNorms(DotUint16(a, b), normUint16(a), normUint16(b))
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
