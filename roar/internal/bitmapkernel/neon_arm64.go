//go:build goexperiment.simd && arm64

package bitmapkernel

import (
	"math/bits"

	"simd/archsimd"
)

func andNEONFixedAsm(dst, a, b []uint64) uint32
func orNEONFixedAsm(dst, a, b []uint64) uint32
func andNotNEONFixedAsm(dst, a, b []uint64) uint32
func applyNEONFixedAsm(dst, base, add, remove []uint64) uint32
func intersectsNEONFixedAsm(a, b []uint64) bool
func andCardinalityNEONFixedAsm(a, b []uint64) uint64
func orCardinalityNEONFixedAsm(a, b []uint64) uint64
func andNotCardinalityNEONFixedAsm(a, b []uint64) uint64
func cardinalityNEONFixedAsm(words []uint64) uint64
func andMany3NEONFixedAsm(dst, a, b, c []uint64) uint32
func andMany4NEONFixedAsm(dst, a, b, c, d []uint64) uint32

// NEON is baseline on ARM64, so the SIMD backend is selected whenever the
// simd experiment is enabled. Popcount uses NEON's byte-count instruction and
// reduces counts in registers instead of storing each lane and calling
// bits.OnesCount64 for every word. Dense 1,024-word containers use unrolled
// assembly fast paths; generic lengths use the scalar reference implementation.

func init() {
	andKernel = andNEON
	orKernel = orNEON
	andNotKernel = andNotNEON
	intersectsKernel = intersectsNEON
	andCardinalityKernel = andCardinalityNEON
	orCardinalityKernel = orCardinalityNEON
	andNotCardinalityKernel = andNotCardinalityNEON
	cardinalityKernel = cardinalityNEON
	andManyKernel = andManyNEON
	andMany3Kernel = andMany3NEON
	andMany4Kernel = andMany4NEON
	applyKernel = applyNEON
}

// popcountBytesNEON returns one byte count for every byte in v. The caller can
// add several such vectors before widening for the horizontal reduction.
func popcountBytesNEON(v archsimd.Uint64x2) archsimd.Uint8x16 {
	return v.ReshapeToUint8s().OnesCount()
}

// reducePopcountNEON horizontally sums sixteen byte counts with NEON's
// horizontal add. The byte lanes may contain counts from many vectors, so
// widen both halves before reducing to avoid an 8-bit horizontal overflow.
func reducePopcountNEON(counts archsimd.Uint8x16) uint32 {
	lo := counts.ExtendLo8ToUint16().ReduceSum()
	hi := counts.HiToLo().ExtendLo8ToUint16().ReduceSum()
	return uint32(lo) + uint32(hi)
}

func countNEON(v0, v1, v2, v3 archsimd.Uint64x2) uint32 {
	counts := popcountBytesNEON(v0).
		Add(popcountBytesNEON(v1)).
		Add(popcountBytesNEON(v2)).
		Add(popcountBytesNEON(v3))
	return reducePopcountNEON(counts)
}

func countOneNEON(v archsimd.Uint64x2) uint32 {
	return reducePopcountNEON(popcountBytesNEON(v))
}

func andNEON(dst, a, b []uint64) uint32 {
	if len(dst) == fixedBitmapWords {
		return andNEONFixedAsm(dst, a, b)
	}
	return andScalar(dst, a, b)
}

func orNEON(dst, a, b []uint64) uint32 {
	if len(dst) == fixedBitmapWords {
		return orNEONFixedAsm(dst, a, b)
	}
	return orScalar(dst, a, b)
}

func andNotNEON(dst, a, b []uint64) uint32 {
	if len(dst) == fixedBitmapWords {
		return andNotNEONFixedAsm(dst, a, b)
	}
	return andNotScalar(dst, a, b)
}

func intersectsNEON(a, b []uint64) bool {
	if len(a) == fixedBitmapWords {
		return intersectsNEONFixedAsm(a, b)
	}
	return intersectsScalar(a, b)
}

func andCardinalityNEON(a, b []uint64) uint64 {
	if len(a) == fixedBitmapWords {
		return andCardinalityNEONFixedAsm(a, b)
	}
	return andCardinalityScalar(a, b)
}

func orCardinalityNEON(a, b []uint64) uint64 {
	if len(a) == fixedBitmapWords {
		return orCardinalityNEONFixedAsm(a, b)
	}
	return orCardinalityScalar(a, b)
}

func andNotCardinalityNEON(a, b []uint64) uint64 {
	if len(a) == fixedBitmapWords {
		return andNotCardinalityNEONFixedAsm(a, b)
	}
	return andNotCardinalityScalar(a, b)
}

func cardinalityNEON(words []uint64) uint64 {
	if len(words) == fixedBitmapWords {
		return cardinalityNEONFixedAsm(words)
	}
	return cardinalityScalar(words)
}

func andManyNEON(dst []uint64, inputs ...[]uint64) uint32 {
	var card uint32
	i := 0
	for ; i+62 <= len(dst); i += 62 {
		v := archsimd.LoadUint64x2(inputs[0][i:])
		for _, in := range inputs[1:] {
			v = v.And(archsimd.LoadUint64x2(in[i:]))
		}
		counts := popcountBytesNEON(v)
		v.Store(dst[i:])
		for j := i + 2; j < i+62; j += 2 {
			v = archsimd.LoadUint64x2(inputs[0][j:])
			for _, in := range inputs[1:] {
				v = v.And(archsimd.LoadUint64x2(in[j:]))
			}
			counts = counts.Add(popcountBytesNEON(v))
			v.Store(dst[j:])
		}
		card += reducePopcountNEON(counts)
	}
	for ; i+8 <= len(dst); i += 8 {
		v0 := archsimd.LoadUint64x2(inputs[0][i:])
		v1 := archsimd.LoadUint64x2(inputs[0][i+2:])
		v2 := archsimd.LoadUint64x2(inputs[0][i+4:])
		v3 := archsimd.LoadUint64x2(inputs[0][i+6:])
		for _, in := range inputs[1:] {
			v0 = v0.And(archsimd.LoadUint64x2(in[i:]))
			v1 = v1.And(archsimd.LoadUint64x2(in[i+2:]))
			v2 = v2.And(archsimd.LoadUint64x2(in[i+4:]))
			v3 = v3.And(archsimd.LoadUint64x2(in[i+6:]))
		}
		v0.Store(dst[i:])
		v1.Store(dst[i+2:])
		v2.Store(dst[i+4:])
		v3.Store(dst[i+6:])
		card += countNEON(v0, v1, v2, v3)
	}
	for ; i+2 <= len(dst); i += 2 {
		v := archsimd.LoadUint64x2(inputs[0][i:])
		for _, in := range inputs[1:] {
			v = v.And(archsimd.LoadUint64x2(in[i:]))
		}
		v.Store(dst[i:])
		card += countOneNEON(v)
	}
	for ; i < len(dst); i++ {
		w := inputs[0][i]
		for _, in := range inputs[1:] {
			w &= in[i]
		}
		dst[i] = w
		card += uint32(bits.OnesCount64(w))
	}
	return card
}

func andMany3NEON(dst, a, b, c []uint64) uint32 {
	if len(dst) == fixedBitmapWords {
		return andMany3NEONFixedAsm(dst, a, b, c)
	}
	var card uint32
	i := 0
	for ; i+8 <= len(dst); i += 8 {
		v0 := archsimd.LoadUint64x2(a[i:]).And(archsimd.LoadUint64x2(b[i:])).And(archsimd.LoadUint64x2(c[i:]))
		v1 := archsimd.LoadUint64x2(a[i+2:]).And(archsimd.LoadUint64x2(b[i+2:])).And(archsimd.LoadUint64x2(c[i+2:]))
		v2 := archsimd.LoadUint64x2(a[i+4:]).And(archsimd.LoadUint64x2(b[i+4:])).And(archsimd.LoadUint64x2(c[i+4:]))
		v3 := archsimd.LoadUint64x2(a[i+6:]).And(archsimd.LoadUint64x2(b[i+6:])).And(archsimd.LoadUint64x2(c[i+6:]))
		v0.Store(dst[i:])
		v1.Store(dst[i+2:])
		v2.Store(dst[i+4:])
		v3.Store(dst[i+6:])
		card += countNEON(v0, v1, v2, v3)
	}
	for ; i < len(dst); i++ {
		w := a[i] & b[i] & c[i]
		dst[i] = w
		card += uint32(bits.OnesCount64(w))
	}
	return card
}

func andMany4NEON(dst, a, b, c, d []uint64) uint32 {
	if len(dst) == fixedBitmapWords {
		return andMany4NEONFixedAsm(dst, a, b, c, d)
	}
	var card uint32
	i := 0
	for ; i+8 <= len(dst); i += 8 {
		v0 := archsimd.LoadUint64x2(a[i:]).And(archsimd.LoadUint64x2(b[i:])).And(archsimd.LoadUint64x2(c[i:])).And(archsimd.LoadUint64x2(d[i:]))
		v1 := archsimd.LoadUint64x2(a[i+2:]).And(archsimd.LoadUint64x2(b[i+2:])).And(archsimd.LoadUint64x2(c[i+2:])).And(archsimd.LoadUint64x2(d[i+2:]))
		v2 := archsimd.LoadUint64x2(a[i+4:]).And(archsimd.LoadUint64x2(b[i+4:])).And(archsimd.LoadUint64x2(c[i+4:])).And(archsimd.LoadUint64x2(d[i+4:]))
		v3 := archsimd.LoadUint64x2(a[i+6:]).And(archsimd.LoadUint64x2(b[i+6:])).And(archsimd.LoadUint64x2(c[i+6:])).And(archsimd.LoadUint64x2(d[i+6:]))
		v0.Store(dst[i:])
		v1.Store(dst[i+2:])
		v2.Store(dst[i+4:])
		v3.Store(dst[i+6:])
		card += countNEON(v0, v1, v2, v3)
	}
	for ; i < len(dst); i++ {
		w := a[i] & b[i] & c[i] & d[i]
		dst[i] = w
		card += uint32(bits.OnesCount64(w))
	}
	return card
}

func applyNEON(dst, base, add, remove []uint64) uint32 {
	if len(dst) == fixedBitmapWords {
		return applyNEONFixedAsm(dst, base, add, remove)
	}
	return applyScalar(dst, base, add, remove)
}
