//go:build goexperiment.simd && arm64

package bitmapkernel

import (
	"math/bits"

	"simd/archsimd"
)

// NEON is baseline on ARM64, so the SIMD backend is always selected when
// compiled in.

func init() {
	andImpl = andNEON
	orImpl = orNEON
	andNotImpl = andNotNEON
	intersectsImpl = intersectsNEON
	andCardinalityImpl = andCardinalityNEON
	orCardinalityImpl = orCardinalityNEON
	andNotCardinalityImpl = andNotCardinalityNEON
	andManyImpl = andManyNEON
	applyImpl = applyNEON
}

func andNEON(dst, a, b []uint64) uint32 {
	var card uint32
	i := 0
	for ; i+2 <= len(dst); i += 2 {
		r := archsimd.LoadUint64x2(a[i:]).And(archsimd.LoadUint64x2(b[i:]))
		r.Store(dst[i:])
		card += uint32(bits.OnesCount64(dst[i]) + bits.OnesCount64(dst[i+1]))
	}
	for ; i < len(dst); i++ {
		w := a[i] & b[i]
		dst[i] = w
		card += uint32(bits.OnesCount64(w))
	}
	return card
}

func orNEON(dst, a, b []uint64) uint32 {
	var card uint32
	i := 0
	for ; i+2 <= len(dst); i += 2 {
		r := archsimd.LoadUint64x2(a[i:]).Or(archsimd.LoadUint64x2(b[i:]))
		r.Store(dst[i:])
		card += uint32(bits.OnesCount64(dst[i]) + bits.OnesCount64(dst[i+1]))
	}
	for ; i < len(dst); i++ {
		w := a[i] | b[i]
		dst[i] = w
		card += uint32(bits.OnesCount64(w))
	}
	return card
}

func andNotNEON(dst, a, b []uint64) uint32 {
	var card uint32
	i := 0
	for ; i+2 <= len(dst); i += 2 {
		r := archsimd.LoadUint64x2(a[i:]).AndNot(archsimd.LoadUint64x2(b[i:]))
		r.Store(dst[i:])
		card += uint32(bits.OnesCount64(dst[i]) + bits.OnesCount64(dst[i+1]))
	}
	for ; i < len(dst); i++ {
		w := a[i] &^ b[i]
		dst[i] = w
		card += uint32(bits.OnesCount64(w))
	}
	return card
}

func intersectsNEON(a, b []uint64) bool {
	i := 0
	for ; i+2 <= len(a); i += 2 {
		r := archsimd.LoadUint64x2(a[i:]).And(archsimd.LoadUint64x2(b[i:]))
		if r.GetElem(0) != 0 || r.GetElem(1) != 0 {
			return true
		}
	}
	for ; i < len(a); i++ {
		if a[i]&b[i] != 0 {
			return true
		}
	}
	return false
}

func andCardinalityNEON(a, b []uint64) uint64 {
	var card uint64
	i := 0
	for ; i+2 <= len(a); i += 2 {
		r := archsimd.LoadUint64x2(a[i:]).And(archsimd.LoadUint64x2(b[i:]))
		card += uint64(bits.OnesCount64(r.GetElem(0)) + bits.OnesCount64(r.GetElem(1)))
	}
	for ; i < len(a); i++ {
		card += uint64(bits.OnesCount64(a[i] & b[i]))
	}
	return card
}

func orCardinalityNEON(a, b []uint64) uint64 {
	var card uint64
	i := 0
	for ; i+2 <= len(a); i += 2 {
		r := archsimd.LoadUint64x2(a[i:]).Or(archsimd.LoadUint64x2(b[i:]))
		card += uint64(bits.OnesCount64(r.GetElem(0)) + bits.OnesCount64(r.GetElem(1)))
	}
	for ; i < len(a); i++ {
		card += uint64(bits.OnesCount64(a[i] | b[i]))
	}
	return card
}

func andNotCardinalityNEON(a, b []uint64) uint64 {
	var card uint64
	i := 0
	for ; i+2 <= len(a); i += 2 {
		r := archsimd.LoadUint64x2(a[i:]).AndNot(archsimd.LoadUint64x2(b[i:]))
		card += uint64(bits.OnesCount64(r.GetElem(0)) + bits.OnesCount64(r.GetElem(1)))
	}
	for ; i < len(a); i++ {
		card += uint64(bits.OnesCount64(a[i] &^ b[i]))
	}
	return card
}

func andManyNEON(dst []uint64, inputs ...[]uint64) uint32 {
	var card uint32
	i := 0
	for ; i+2 <= len(dst); i += 2 {
		r := archsimd.LoadUint64x2(inputs[0][i:])
		for _, in := range inputs[1:] {
			r = r.And(archsimd.LoadUint64x2(in[i:]))
		}
		r.Store(dst[i:])
		card += uint32(bits.OnesCount64(dst[i]) + bits.OnesCount64(dst[i+1]))
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

func applyNEON(dst, base, add, remove []uint64) uint32 {
	var card uint32
	i := 0
	for ; i+2 <= len(dst); i += 2 {
		r := archsimd.LoadUint64x2(base[i:]).
			Or(archsimd.LoadUint64x2(add[i:])).
			AndNot(archsimd.LoadUint64x2(remove[i:]))
		r.Store(dst[i:])
		card += uint32(bits.OnesCount64(dst[i]) + bits.OnesCount64(dst[i+1]))
	}
	for ; i < len(dst); i++ {
		w := (base[i] | add[i]) &^ remove[i]
		dst[i] = w
		card += uint32(bits.OnesCount64(w))
	}
	return card
}
