//go:build goexperiment.simd && arm64

package bitmapkernel

import (
	"math/bits"
	"math/rand/v2"
	"testing"

	"simd/archsimd"
)

var popcountBenchmarkSink uint64

func BenchmarkPopcountScalar(b *testing.B) {
	rng := rand.New(rand.NewPCG(31, 37))
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
		popcountBenchmarkSink = total
	}
}

func BenchmarkPopcountNEON(b *testing.B) {
	rng := rand.New(rand.NewPCG(31, 37))
	words := make([]uint64, testWords)
	for i := range words {
		words[i] = rng.Uint64()
	}
	b.ReportAllocs()
	for range b.N {
		var total uint64
		for i := 0; i < len(words); i += 2 {
			total += uint64(countOneNEON(archsimd.LoadUint64x2(words[i:])))
		}
		popcountBenchmarkSink = total
	}
}

func BenchmarkPopcountNEONAsm(b *testing.B) {
	rng := rand.New(rand.NewPCG(31, 37))
	words := make([]uint64, testWords)
	for i := range words {
		words[i] = rng.Uint64()
	}
	b.ReportAllocs()
	for range b.N {
		popcountBenchmarkSink = cardinalityNEON(words)
	}
}
