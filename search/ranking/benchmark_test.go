package ranking

import (
	"math/rand"
	"testing"
)

func benchData(n int) (ids []ImageID, dist []uint32, sims []float32) {
	rng := rand.New(rand.NewSource(42))
	ids = make([]ImageID, n)
	dist = make([]uint32, n)
	sims = make([]float32, n)
	for i := range ids {
		ids[i] = ImageID(i + 1)
		// Realistic skewed distances: many near, few far.
		dist[i] = uint32(rng.ExpFloat64() * 3600)
		sims[i] = float32(rng.NormFloat64() * 0.3)
		if sims[i] > 1 {
			sims[i] = 1
		}
		if sims[i] < -1 {
			sims[i] = -1
		}
	}
	return ids, dist, sims
}

func benchmarkEncodeDistance(b *testing.B, n int) {
	_, dist, _ := benchData(n)
	dst := make([]Rank, n)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		EncodeDistances(dst, dist)
	}
}

func benchmarkCosine(b *testing.B, n int) {
	_, _, sims := benchData(n)
	dst := make([]Rank, n)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		QuantizeCosines(dst, sims)
	}
}

func benchmarkFusion(b *testing.B, n int) {
	ids, dist, sims := benchData(n)
	s := &Signals{ImageIDs: ids, Count: 2, Kinds: [MaxSignals]Kind{KindTime, KindVector}}
	s.Values[0] = make([]Rank, n)
	s.Values[1] = make([]Rank, n)
	EncodeDistances(s.Values[0], dist)
	QuantizeCosines(s.Values[1], sims)
	dst := make([]Rank, n)
	var f RRF
	_ = f.Fuse(dst, s)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = f.Fuse(dst, s)
	}
}

func benchmarkTopK(b *testing.B, n, k int) {
	ids, dist, sims := benchData(n)
	s := &Signals{ImageIDs: ids, Count: 2}
	s.Values[0] = make([]Rank, n)
	s.Values[1] = make([]Rank, n)
	EncodeDistances(s.Values[0], dist)
	QuantizeCosines(s.Values[1], sims)
	fused := make([]Rank, n)
	var f RRF
	_ = f.Fuse(fused, s)
	dstIDs := make([]ImageID, 0, k)
	dstRanks := make([]Rank, 0, k)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		dstIDs, dstRanks = TopK(dstIDs[:0], dstRanks[:0], ids, fused, k)
	}
}

func benchmarkSort(b *testing.B, n int) {
	ids, dist, _ := benchData(n)
	ranks := make([]Rank, n)
	EncodeDistances(ranks, dist)
	cids := make([]ImageID, n)
	cranks := make([]Rank, n)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		copy(cids, ids)
		copy(cranks, ranks)
		Sort(cids, cranks)
	}
}

func BenchmarkEncodeDistance100(b *testing.B) { benchmarkEncodeDistance(b, 100) }
func BenchmarkEncodeDistance1k(b *testing.B)  { benchmarkEncodeDistance(b, 1000) }
func BenchmarkEncodeDistance10k(b *testing.B) { benchmarkEncodeDistance(b, 10000) }
func BenchmarkCosine100(b *testing.B)         { benchmarkCosine(b, 100) }
func BenchmarkCosine1k(b *testing.B)          { benchmarkCosine(b, 1000) }
func BenchmarkCosine10k(b *testing.B)         { benchmarkCosine(b, 10000) }
func BenchmarkFusion100(b *testing.B)         { benchmarkFusion(b, 100) }
func BenchmarkFusion1k(b *testing.B)          { benchmarkFusion(b, 1000) }
func BenchmarkFusion10k(b *testing.B)         { benchmarkFusion(b, 10000) }
func BenchmarkTopK10kK10(b *testing.B)        { benchmarkTopK(b, 10000, 10) }
func BenchmarkTopK10kK100(b *testing.B)       { benchmarkTopK(b, 10000, 100) }
func BenchmarkTopK10kK1000(b *testing.B)      { benchmarkTopK(b, 10000, 1000) }
func BenchmarkSort100(b *testing.B)           { benchmarkSort(b, 100) }
func BenchmarkSort1k(b *testing.B)            { benchmarkSort(b, 1000) }
func BenchmarkSort10k(b *testing.B)           { benchmarkSort(b, 10000) }
