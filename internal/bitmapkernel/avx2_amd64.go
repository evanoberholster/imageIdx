//go:build goexperiment.simd && amd64

package bitmapkernel

import (
	"math/bits"

	"simd/archsimd"
)

// The AVX2 backend processes four uint64 words per 256-bit vector. It is
// selected only when the CPU reports AVX2 support.

func init() {
	if !archsimd.X86.AVX2() {
		return
	}
	andImpl = andAVX2
	orImpl = orAVX2
	andNotImpl = andNotAVX2
	intersectsImpl = intersectsAVX2
	andCardinalityImpl = andCardinalityAVX2
	orCardinalityImpl = orCardinalityAVX2
	andNotCardinalityImpl = andNotCardinalityAVX2
	andManyImpl = andManyAVX2
	applyImpl = applyAVX2
}

func andAVX2(dst, a, b []uint64) uint32 {
	var card uint32
	i := 0
	for ; i+4 <= len(dst); i += 4 {
		r := archsimd.LoadUint64x4(a[i:]).And(archsimd.LoadUint64x4(b[i:]))
		r.Store(dst[i:])
		card += uint32(bits.OnesCount64(dst[i]) + bits.OnesCount64(dst[i+1]) + bits.OnesCount64(dst[i+2]) + bits.OnesCount64(dst[i+3]))
	}
	for ; i < len(dst); i++ {
		w := a[i] & b[i]
		dst[i] = w
		card += uint32(bits.OnesCount64(w))
	}
	return card
}

func orAVX2(dst, a, b []uint64) uint32 {
	var card uint32
	i := 0
	for ; i+4 <= len(dst); i += 4 {
		r := archsimd.LoadUint64x4(a[i:]).Or(archsimd.LoadUint64x4(b[i:]))
		r.Store(dst[i:])
		card += uint32(bits.OnesCount64(dst[i]) + bits.OnesCount64(dst[i+1]) + bits.OnesCount64(dst[i+2]) + bits.OnesCount64(dst[i+3]))
	}
	for ; i < len(dst); i++ {
		w := a[i] | b[i]
		dst[i] = w
		card += uint32(bits.OnesCount64(w))
	}
	return card
}

func andNotAVX2(dst, a, b []uint64) uint32 {
	var card uint32
	i := 0
	for ; i+4 <= len(dst); i += 4 {
		r := archsimd.LoadUint64x4(a[i:]).AndNot(archsimd.LoadUint64x4(b[i:]))
		r.Store(dst[i:])
		card += uint32(bits.OnesCount64(dst[i]) + bits.OnesCount64(dst[i+1]) + bits.OnesCount64(dst[i+2]) + bits.OnesCount64(dst[i+3]))
	}
	for ; i < len(dst); i++ {
		w := a[i] &^ b[i]
		dst[i] = w
		card += uint32(bits.OnesCount64(w))
	}
	return card
}

func intersectsAVX2(a, b []uint64) bool {
	i := 0
	for ; i+4 <= len(a); i += 4 {
		r := archsimd.LoadUint64x4(a[i:]).And(archsimd.LoadUint64x4(b[i:]))
		var tmp [4]uint64
		r.StoreArray(&tmp)
		if tmp[0] != 0 || tmp[1] != 0 || tmp[2] != 0 || tmp[3] != 0 {
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

func andCardinalityAVX2(a, b []uint64) uint64 {
	var card uint64
	i := 0
	for ; i+4 <= len(a); i += 4 {
		var tmp [4]uint64
		archsimd.LoadUint64x4(a[i:]).And(archsimd.LoadUint64x4(b[i:])).StoreArray(&tmp)
		card += uint64(bits.OnesCount64(tmp[0]) + bits.OnesCount64(tmp[1]) + bits.OnesCount64(tmp[2]) + bits.OnesCount64(tmp[3]))
	}
	for ; i < len(a); i++ {
		card += uint64(bits.OnesCount64(a[i] & b[i]))
	}
	return card
}

func orCardinalityAVX2(a, b []uint64) uint64 {
	var card uint64
	i := 0
	for ; i+4 <= len(a); i += 4 {
		var tmp [4]uint64
		archsimd.LoadUint64x4(a[i:]).Or(archsimd.LoadUint64x4(b[i:])).StoreArray(&tmp)
		card += uint64(bits.OnesCount64(tmp[0]) + bits.OnesCount64(tmp[1]) + bits.OnesCount64(tmp[2]) + bits.OnesCount64(tmp[3]))
	}
	for ; i < len(a); i++ {
		card += uint64(bits.OnesCount64(a[i] | b[i]))
	}
	return card
}

func andNotCardinalityAVX2(a, b []uint64) uint64 {
	var card uint64
	i := 0
	for ; i+4 <= len(a); i += 4 {
		var tmp [4]uint64
		archsimd.LoadUint64x4(a[i:]).AndNot(archsimd.LoadUint64x4(b[i:])).StoreArray(&tmp)
		card += uint64(bits.OnesCount64(tmp[0]) + bits.OnesCount64(tmp[1]) + bits.OnesCount64(tmp[2]) + bits.OnesCount64(tmp[3]))
	}
	for ; i < len(a); i++ {
		card += uint64(bits.OnesCount64(a[i] &^ b[i]))
	}
	return card
}

func andManyAVX2(dst []uint64, inputs ...[]uint64) uint32 {
	var card uint32
	i := 0
	for ; i+4 <= len(dst); i += 4 {
		r := archsimd.LoadUint64x4(inputs[0][i:])
		for _, in := range inputs[1:] {
			r = r.And(archsimd.LoadUint64x4(in[i:]))
		}
		r.Store(dst[i:])
		card += uint32(bits.OnesCount64(dst[i]) + bits.OnesCount64(dst[i+1]) + bits.OnesCount64(dst[i+2]) + bits.OnesCount64(dst[i+3]))
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

func applyAVX2(dst, base, add, remove []uint64) uint32 {
	var card uint32
	i := 0
	for ; i+4 <= len(dst); i += 4 {
		r := archsimd.LoadUint64x4(base[i:]).
			Or(archsimd.LoadUint64x4(add[i:])).
			AndNot(archsimd.LoadUint64x4(remove[i:]))
		r.Store(dst[i:])
		card += uint32(bits.OnesCount64(dst[i]) + bits.OnesCount64(dst[i+1]) + bits.OnesCount64(dst[i+2]) + bits.OnesCount64(dst[i+3]))
	}
	for ; i < len(dst); i++ {
		w := (base[i] | add[i]) &^ remove[i]
		dst[i] = w
		card += uint32(bits.OnesCount64(w))
	}
	return card
}
