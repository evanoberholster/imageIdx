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
func cardinalityPrefixAVX2Asm(words []uint64, vectorCount int) uint64
func andMany3AVX2FixedAsm(dst, a, b, c []uint64) uint32
func andMany4AVX2FixedAsm(dst, a, b, c, d []uint64) uint32
func andMany3CardinalityAVX2FixedAsm(a, b, c []uint64) uint64
func andMany4CardinalityAVX2FixedAsm(a, b, c, d []uint64) uint64

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
	cardinalityPrefixKernel = cardinalityPrefixAVX2
	andManyKernel = andManyAVX2
	andMany3Kernel = andMany3AVX2
	andMany4Kernel = andMany4AVX2
	andMany3CardinalityKernel = andMany3CardinalityAVX2
	andMany4CardinalityKernel = andMany4CardinalityAVX2
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

func cardinalityPrefixAVX2(words []uint64, wordCount int) uint64 {
	vectorWords := wordCount &^ 3
	var card uint64
	if vectorWords != 0 {
		card = cardinalityPrefixAVX2Asm(words, vectorWords/4)
	}
	return card + cardinalityPrefixScalar(words[vectorWords:], wordCount-vectorWords)
}

func andManyAVX2(dst []uint64, inputs ...[]uint64) uint32 {
	switch len(inputs) {
	case 5:
		return andMany5AVX2(dst, inputs[0], inputs[1], inputs[2], inputs[3], inputs[4])
	case 6:
		return andMany6AVX2(dst, inputs[0], inputs[1], inputs[2], inputs[3], inputs[4], inputs[5])
	case 7:
		return andMany7AVX2(dst, inputs[0], inputs[1], inputs[2], inputs[3], inputs[4], inputs[5], inputs[6])
	case 8:
		return andMany8AVX2(dst, inputs[0], inputs[1], inputs[2], inputs[3], inputs[4], inputs[5], inputs[6], inputs[7])
	}

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

func andMany5AVX2(dst, a, b, c, d, e []uint64) uint32 {
	var card uint32
	i := 0
	for ; i+4 <= len(dst); i += 4 {
		r := archsimd.LoadUint64x4(a[i:]).
			And(archsimd.LoadUint64x4(b[i:])).
			And(archsimd.LoadUint64x4(c[i:])).
			And(archsimd.LoadUint64x4(d[i:])).
			And(archsimd.LoadUint64x4(e[i:]))
		r.Store(dst[i:])
		card += uint32(bits.OnesCount64(dst[i]) + bits.OnesCount64(dst[i+1]) + bits.OnesCount64(dst[i+2]) + bits.OnesCount64(dst[i+3]))
	}
	for ; i < len(dst); i++ {
		w := a[i] & b[i] & c[i] & d[i] & e[i]
		dst[i] = w
		card += uint32(bits.OnesCount64(w))
	}
	return card
}

func andMany6AVX2(dst, a, b, c, d, e, f []uint64) uint32 {
	var card uint32
	i := 0
	for ; i+4 <= len(dst); i += 4 {
		r := archsimd.LoadUint64x4(a[i:]).
			And(archsimd.LoadUint64x4(b[i:])).
			And(archsimd.LoadUint64x4(c[i:])).
			And(archsimd.LoadUint64x4(d[i:])).
			And(archsimd.LoadUint64x4(e[i:])).
			And(archsimd.LoadUint64x4(f[i:]))
		r.Store(dst[i:])
		card += uint32(bits.OnesCount64(dst[i]) + bits.OnesCount64(dst[i+1]) + bits.OnesCount64(dst[i+2]) + bits.OnesCount64(dst[i+3]))
	}
	for ; i < len(dst); i++ {
		w := a[i] & b[i] & c[i] & d[i] & e[i] & f[i]
		dst[i] = w
		card += uint32(bits.OnesCount64(w))
	}
	return card
}

func andMany7AVX2(dst, a, b, c, d, e, f, g []uint64) uint32 {
	var card uint32
	i := 0
	for ; i+4 <= len(dst); i += 4 {
		r := archsimd.LoadUint64x4(a[i:]).
			And(archsimd.LoadUint64x4(b[i:])).
			And(archsimd.LoadUint64x4(c[i:])).
			And(archsimd.LoadUint64x4(d[i:])).
			And(archsimd.LoadUint64x4(e[i:])).
			And(archsimd.LoadUint64x4(f[i:])).
			And(archsimd.LoadUint64x4(g[i:]))
		r.Store(dst[i:])
		card += uint32(bits.OnesCount64(dst[i]) + bits.OnesCount64(dst[i+1]) + bits.OnesCount64(dst[i+2]) + bits.OnesCount64(dst[i+3]))
	}
	for ; i < len(dst); i++ {
		w := a[i] & b[i] & c[i] & d[i] & e[i] & f[i] & g[i]
		dst[i] = w
		card += uint32(bits.OnesCount64(w))
	}
	return card
}

func andMany8AVX2(dst, a, b, c, d, e, f, g, h []uint64) uint32 {
	var card uint32
	i := 0
	for ; i+4 <= len(dst); i += 4 {
		r := archsimd.LoadUint64x4(a[i:]).
			And(archsimd.LoadUint64x4(b[i:])).
			And(archsimd.LoadUint64x4(c[i:])).
			And(archsimd.LoadUint64x4(d[i:])).
			And(archsimd.LoadUint64x4(e[i:])).
			And(archsimd.LoadUint64x4(f[i:])).
			And(archsimd.LoadUint64x4(g[i:])).
			And(archsimd.LoadUint64x4(h[i:]))
		r.Store(dst[i:])
		card += uint32(bits.OnesCount64(dst[i]) + bits.OnesCount64(dst[i+1]) + bits.OnesCount64(dst[i+2]) + bits.OnesCount64(dst[i+3]))
	}
	for ; i < len(dst); i++ {
		w := a[i] & b[i] & c[i] & d[i] & e[i] & f[i] & g[i] & h[i]
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

func andMany3CardinalityAVX2(a, b, c []uint64) uint64 {
	if len(a) == fixedBitmapWords {
		return andMany3CardinalityAVX2FixedAsm(a, b, c)
	}
	return andMany3CardinalityScalar(a, b, c)
}

func andMany4CardinalityAVX2(a, b, c, d []uint64) uint64 {
	if len(a) == fixedBitmapWords {
		return andMany4CardinalityAVX2FixedAsm(a, b, c, d)
	}
	return andMany4CardinalityScalar(a, b, c, d)
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
