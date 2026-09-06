// Package bitmapkernel provides dense bitmap-container kernels over fixed
// 1,024-word (65,536-bit) containers. It is an internal implementation detail
// of the roar package: the serialized format, container layout, and public
// APIs are unchanged. Scalar implementations are the reference and fallback;
// architecture-specific SIMD backends are selected once at initialization.
package bitmapkernel

import "math/bits"

// Scalar reference implementations. These run on every architecture and when
// SIMD is disabled, and they define the exact semantics every backend must
// match.

func andScalar(dst, a, b []uint64) uint32 {
	var card uint32
	for i := range dst {
		w := a[i] & b[i]
		dst[i] = w
		card += uint32(bits.OnesCount64(w))
	}
	return card
}

func orScalar(dst, a, b []uint64) uint32 {
	var card uint32
	for i := range dst {
		w := a[i] | b[i]
		dst[i] = w
		card += uint32(bits.OnesCount64(w))
	}
	return card
}

func andNotScalar(dst, a, b []uint64) uint32 {
	var card uint32
	for i := range dst {
		w := a[i] &^ b[i]
		dst[i] = w
		card += uint32(bits.OnesCount64(w))
	}
	return card
}

func intersectsScalar(a, b []uint64) bool {
	for i := range a {
		if a[i]&b[i] != 0 {
			return true
		}
	}
	return false
}

func andCardinalityScalar(a, b []uint64) uint64 {
	var card uint64
	for i := range a {
		card += uint64(bits.OnesCount64(a[i] & b[i]))
	}
	return card
}

func orCardinalityScalar(a, b []uint64) uint64 {
	var card uint64
	for i := range a {
		card += uint64(bits.OnesCount64(a[i] | b[i]))
	}
	return card
}

func andNotCardinalityScalar(a, b []uint64) uint64 {
	var card uint64
	for i := range a {
		card += uint64(bits.OnesCount64(a[i] &^ b[i]))
	}
	return card
}

func cardinalityScalar(w []uint64) uint64 {
	var card uint64
	for _, x := range w {
		card += uint64(bits.OnesCount64(x))
	}
	return card
}

func andManyScalar(dst []uint64, inputs ...[]uint64) uint32 {
	copy(dst, inputs[0])
	for _, in := range inputs[1:] {
		for i := range dst {
			dst[i] &= in[i]
		}
	}
	return uint32(cardinalityScalar(dst))
}

func applyScalar(dst, base, add, remove []uint64) uint32 {
	var card uint32
	for i := range dst {
		w := (base[i] | add[i]) &^ remove[i]
		dst[i] = w
		card += uint32(bits.OnesCount64(w))
	}
	return card
}
