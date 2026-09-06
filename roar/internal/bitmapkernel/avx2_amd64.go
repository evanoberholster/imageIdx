//go:build goexperiment.simd && amd64

package bitmapkernel

import (
	"math/bits"

	"simd/archsimd"
)

func andAVX2FixedAsm(dst, a, b []uint64) uint32
func orAVX2FixedAsm(dst, a, b []uint64) uint32
func andNotAVX2FixedAsm(dst, a, b []uint64) uint32
func applyAVX2FixedAsm(dst, base, add, remove []uint64) uint32
func intersectsAVX2FixedAsm(a, b []uint64) bool
func andCardinalityAVX2FixedAsm(a, b []uint64) uint64
func orCardinalityAVX2FixedAsm(a, b []uint64) uint64
func andNotCardinalityAVX2FixedAsm(a, b []uint64) uint64
func cardinalityAVX2FixedAsm(words []uint64) uint64
func andMany3AVX2FixedAsm(dst, a, b, c []uint64) uint32
func andMany4AVX2FixedAsm(dst, a, b, c, d []uint64) uint32

// The AVX2 backend processes four uint64 words per 256-bit vector. It is
// selected only when the CPU reports AVX2 support.

func init() {
	if !archsimd.X86.AVX2() {
		return
	}
	andKernel = andAVX2
	orKernel = orAVX2
	andNotKernel = andNotAVX2
	intersectsKernel = intersectsAVX2
	andCardinalityKernel = andCardinalityAVX2
	orCardinalityKernel = orCardinalityAVX2
	andNotCardinalityKernel = andNotCardinalityAVX2
	cardinalityKernel = cardinalityAVX2
	andManyKernel = andManyAVX2
	andMany3Kernel = andMany3AVX2
	andMany4Kernel = andMany4AVX2
	applyKernel = applyAVX2
}

func andAVX2(dst, a, b []uint64) uint32 {
	if len(dst) == fixedBitmapWords {
		return andAVX2FixedAsm(dst, a, b)
	}
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
	if len(dst) == fixedBitmapWords {
		return orAVX2FixedAsm(dst, a, b)
	}
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
	if len(dst) == fixedBitmapWords {
		return andNotAVX2FixedAsm(dst, a, b)
	}
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
	if len(a) == fixedBitmapWords {
		return intersectsAVX2FixedAsm(a, b)
	}
	i := 0
	for ; i+4 <= len(a); i += 4 {
		r := archsimd.LoadUint64x4(a[i:]).And(archsimd.LoadUint64x4(b[i:]))
		if !r.IsZero() {
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
	if len(a) == fixedBitmapWords {
		return andCardinalityAVX2FixedAsm(a, b)
	}
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
	if len(a) == fixedBitmapWords {
		return orCardinalityAVX2FixedAsm(a, b)
	}
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
	if len(a) == fixedBitmapWords {
		return andNotCardinalityAVX2FixedAsm(a, b)
	}
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

func cardinalityAVX2(words []uint64) uint64 {
	if len(words) == fixedBitmapWords {
		return cardinalityAVX2FixedAsm(words)
	}
	var card uint64
	i := 0
	for ; i+4 <= len(words); i += 4 {
		var tmp [4]uint64
		archsimd.LoadUint64x4(words[i:]).StoreArray(&tmp)
		card += uint64(bits.OnesCount64(tmp[0]) + bits.OnesCount64(tmp[1]) + bits.OnesCount64(tmp[2]) + bits.OnesCount64(tmp[3]))
	}
	for ; i < len(words); i++ {
		card += uint64(bits.OnesCount64(words[i]))
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

func andMany3AVX2(dst, a, b, c []uint64) uint32 {
	if len(dst) == fixedBitmapWords {
		return andMany3AVX2FixedAsm(dst, a, b, c)
	}
	var card uint32
	i := 0
	for ; i+4 <= len(dst); i += 4 {
		r := archsimd.LoadUint64x4(a[i:]).And(archsimd.LoadUint64x4(b[i:])).And(archsimd.LoadUint64x4(c[i:]))
		r.Store(dst[i:])
		card += uint32(bits.OnesCount64(dst[i]) + bits.OnesCount64(dst[i+1]) + bits.OnesCount64(dst[i+2]) + bits.OnesCount64(dst[i+3]))
	}
	for ; i < len(dst); i++ {
		w := a[i] & b[i] & c[i]
		dst[i] = w
		card += uint32(bits.OnesCount64(w))
	}
	return card
}

func andMany4AVX2(dst, a, b, c, d []uint64) uint32 {
	if len(dst) == fixedBitmapWords {
		return andMany4AVX2FixedAsm(dst, a, b, c, d)
	}
	var card uint32
	i := 0
	for ; i+4 <= len(dst); i += 4 {
		r := archsimd.LoadUint64x4(a[i:]).And(archsimd.LoadUint64x4(b[i:])).And(archsimd.LoadUint64x4(c[i:])).And(archsimd.LoadUint64x4(d[i:]))
		r.Store(dst[i:])
		card += uint32(bits.OnesCount64(dst[i]) + bits.OnesCount64(dst[i+1]) + bits.OnesCount64(dst[i+2]) + bits.OnesCount64(dst[i+3]))
	}
	for ; i < len(dst); i++ {
		w := a[i] & b[i] & c[i] & d[i]
		dst[i] = w
		card += uint32(bits.OnesCount64(w))
	}
	return card
}

func applyAVX2(dst, base, add, remove []uint64) uint32 {
	if len(dst) == fixedBitmapWords {
		return applyAVX2FixedAsm(dst, base, add, remove)
	}
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
