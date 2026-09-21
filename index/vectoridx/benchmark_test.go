package vectoridx

import (
	"math/rand"
	"testing"
)

func BenchmarkDistanceFloat32_512(b *testing.B) {
	benchDistance(b, ElementFloat32, Dim512)
}

func BenchmarkDistanceFloat32_768(b *testing.B) {
	benchDistance(b, ElementFloat32, Dim768)
}

func BenchmarkDistanceFloat32_1024(b *testing.B) {
	benchDistance(b, ElementFloat32, Dim1024)
}

func BenchmarkDistanceInt8_512(b *testing.B) {
	benchDistance(b, ElementInt8, Dim512)
}

func BenchmarkDistanceInt16_1024(b *testing.B) {
	benchDistance(b, ElementInt16, Dim1024)
}

func BenchmarkDistanceUint16_1024(b *testing.B) {
	benchDistance(b, ElementUint16, Dim1024)
}

func benchDistance(b *testing.B, elem ElementType, dims int) {
	b.Helper()
	rng := rand.New(rand.NewSource(77))
	cfg := Config{Dimensions: uint16(dims), ElementType: elem}
	validated, err := cfg.withDefaults()
	if err != nil {
		b.Fatal(err)
	}
	dist := resolveDistance(validated)
	vecBytes := validated.VectorBytes()
	a := make([]byte, vecBytes)
	b2 := make([]byte, vecBytes)
	if _, err := rng.Read(a); err != nil {
		b.Fatal(err)
	}
	if _, err := rng.Read(b2); err != nil {
		b.Fatal(err)
	}
	b.SetBytes(int64(vecBytes))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = dist(a, b2)
	}
}

func buildBenchIndex(b *testing.B, n int) (*VectorIdx, [][]float32) {
	b.Helper()
	idx, err := New(Config{
		TypeID:         1,
		Dimensions:     Dim512,
		ElementType:    ElementFloat32,
		M:              DefaultM,
		M0:             DefaultM0,
		EfConstruction: 100,
		EfSearch:       64,
	})
	if err != nil {
		b.Fatal(err)
	}
	rng := rand.New(rand.NewSource(78))
	vecs := make([][]float32, n)
	for i := range vecs {
		vecs[i] = randFloatVec(rng, Dim512)
		if err := idx.AddFloat32(NodeID(i+1), vecs[i]); err != nil {
			b.Fatal(err)
		}
	}
	return idx, vecs
}

func BenchmarkSearchFloat32(b *testing.B) {
	idx, vecs := buildBenchIndex(b, 1000)
	defer idx.Close()
	q := vecs[0]
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := idx.SearchFloat32(q, nil, 10); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkSearchIntoFloat32Reuse(b *testing.B) {
	idx, vecs := buildBenchIndex(b, 1000)
	defer idx.Close()
	q := vecs[0]
	dst := make([]Result, 0, 10)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var err error
		dst, err = idx.SearchIntoFloat32(dst[:0], q, nil, 10)
		if err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkExactSearchFloat32(b *testing.B) {
	idx, vecs := buildBenchIndex(b, 1000)
	defer idx.Close()
	q := vecs[0]
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := idx.ExactSearchFloat32(q, nil, 10); err != nil {
			b.Fatal(err)
		}
	}
}

func TestSearchSteadyStateAllocs(t *testing.T) {
	if raceEnabled {
		t.Skip("allocation counts are calibrated for non-race builds; the race detector adds bookkeeping allocs")
	}
	idx, err := New(Config{TypeID: 1, Dimensions: Dim512, ElementType: ElementFloat32})
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()
	rng := rand.New(rand.NewSource(99))
	for i := NodeID(1); i <= 200; i++ {
		mustAddFloat(t, idx, i, randFloatVec(rng, Dim512))
	}
	q := randFloatVec(rng, Dim512)
	dst := make([]Result, 0, 10)
	for range 20 {
		var err error
		dst, err = idx.SearchIntoFloat32(dst[:0], q, nil, 10)
		if err != nil {
			t.Fatal(err)
		}
	}
	// SearchInto reuses caller storage and pooled scratch: zero allocs.
	if n := testing.AllocsPerRun(50, func() {
		var err error
		dst, err = idx.SearchIntoFloat32(dst[:0], q, nil, 10)
		if err != nil {
			t.Fatal(err)
		}
	}); n != 0 {
		t.Fatalf("SearchInto steady-state allocs = %v, want 0", n)
	}
	// Search allocates exactly once: the caller-owned result copy.
	if n := testing.AllocsPerRun(50, func() {
		if _, err := idx.SearchFloat32(q, nil, 10); err != nil {
			t.Fatal(err)
		}
	}); n != 1 {
		t.Fatalf("Search steady-state allocs = %v, want 1", n)
	}
}
