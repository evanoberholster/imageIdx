//go:build goexperiment.simd && amd64

package bitmapkernel

import (
	"math/bits"
	"math/rand/v2"
	"testing"

	"simd/archsimd"
)

var popcountAVX2BenchmarkSink uint64

func BenchmarkPopcountScalarAVX2(b *testing.B) {
	rng := rand.New(rand.NewPCG(41, 43))
	words := make([]uint64, testWords)
	for i := range words {
		words[i] = rng.Uint64()
	}
	b.ReportAllocs()
	for range b.N {
		var total uint64
		for _, word := range words {
			total += uint64(bits.OnesCount64(word))
		}
		popcountAVX2BenchmarkSink = total
	}
}

func BenchmarkAndAVX2Go(b *testing.B) {
	rng := rand.New(rand.NewPCG(47, 53))
	a := make([]uint64, testWords)
	bb := make([]uint64, testWords)
	dst := make([]uint64, testWords)
	for i := range a {
		a[i], bb[i] = rng.Uint64(), rng.Uint64()
	}
	b.ReportAllocs()
	for range b.N {
		var card uint32
		for i := 0; i < len(dst); i += 4 {
			r := archsimd.LoadUint64x4(a[i:]).And(archsimd.LoadUint64x4(bb[i:]))
			r.Store(dst[i:])
			card += uint32(bits.OnesCount64(dst[i]) + bits.OnesCount64(dst[i+1]) + bits.OnesCount64(dst[i+2]) + bits.OnesCount64(dst[i+3]))
		}
		popcountAVX2BenchmarkSink = uint64(card)
	}
}

func BenchmarkPopcountAVX2Asm(b *testing.B) {
	rng := rand.New(rand.NewPCG(41, 43))
	words := make([]uint64, testWords)
	for i := range words {
		words[i] = rng.Uint64()
	}
	b.ReportAllocs()
	for range b.N {
		popcountAVX2BenchmarkSink = cardinalityAVX2(words)
	}
}
