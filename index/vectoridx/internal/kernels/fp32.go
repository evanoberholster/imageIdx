package kernels

import "math"

// SquaredL2Float32 returns sum (a[i]-b[i])^2 accumulated in float64. Four
// independent accumulators break the serial dependency chain on the vector
// units; the final sum is still float64.
func SquaredL2Float32(a, b []float32) float64 {
	var a0, a1, a2, a3 float64
	i := 0
	for ; i+4 <= len(a); i += 4 {
		d0 := float64(a[i+0]) - float64(b[i+0])
		d1 := float64(a[i+1]) - float64(b[i+1])
		d2 := float64(a[i+2]) - float64(b[i+2])
		d3 := float64(a[i+3]) - float64(b[i+3])
		a0 += d0 * d0
		a1 += d1 * d1
		a2 += d2 * d2
		a3 += d3 * d3
	}
	for ; i < len(a); i++ {
		d := float64(a[i]) - float64(b[i])
		a0 += d * d
	}
	return (a0 + a1) + (a2 + a3)
}

// DotFloat32 returns sum a[i]*b[i] accumulated in float64. Four independent
// accumulators break the serial dependency chain on the vector units; the final
// sum is still float64.
func DotFloat32(a, b []float32) float64 {
	var a0, a1, a2, a3 float64
	i := 0
	for ; i+4 <= len(a); i += 4 {
		a0 += float64(a[i+0]) * float64(b[i+0])
		a1 += float64(a[i+1]) * float64(b[i+1])
		a2 += float64(a[i+2]) * float64(b[i+2])
		a3 += float64(a[i+3]) * float64(b[i+3])
	}
	for ; i < len(a); i++ {
		a0 += float64(a[i]) * float64(b[i])
	}
	return (a0 + a1) + (a2 + a3)
}

// CosineFloat32 returns 1-cos over float32 vectors.
func CosineFloat32(a, b []float32) float64 {
	return CosineFromDotNorms(DotFloat32(a, b), normFloat32(a), normFloat32(b))
}

func normFloat32(a []float32) float64 {
	var a0, a1, a2, a3 float64
	i := 0
	for ; i+4 <= len(a); i += 4 {
		a0 += float64(a[i+0]) * float64(a[i+0])
		a1 += float64(a[i+1]) * float64(a[i+1])
		a2 += float64(a[i+2]) * float64(a[i+2])
		a3 += float64(a[i+3]) * float64(a[i+3])
	}
	for ; i < len(a); i++ {
		a0 += float64(a[i]) * float64(a[i])
	}
	return math.Sqrt((a0 + a1) + (a2 + a3))
}
