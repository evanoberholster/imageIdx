package vectoridx

import (
	"math/rand"
	"path/filepath"
	"testing"
	"time"
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

func benchDistance(b *testing.B, elem ElementType, dims Dimensions) {
	b.Helper()
	rng := rand.New(rand.NewSource(77))
	cfg := Config{Namespace: testNamespace, Dimensions: dims, ElementType: elem}
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
	return buildBenchIndexCfg(b, n, Config{
		Namespace:      testNamespace,
		Dimensions:     Dim512,
		ElementType:    ElementFloat32,
		M:              DefaultM,
		M0:             DefaultM0,
		EfConstruction: 100,
		EfSearch:       64,
	})
}

func buildBenchIndexCfg(b *testing.B, n int, cfg Config) (*VectorIdx, [][]float32) {
	b.Helper()
	idx, err := NewMemory(cfg)
	if err != nil {
		b.Fatal(err)
	}
	rng := rand.New(rand.NewSource(78))
	vecs := make([][]float32, n)
	for i := range vecs {
		vecs[i] = randFloatVec(rng, cfg.Dimensions)
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

func BenchmarkSearchFloat32_768ANN(b *testing.B) {
	idx, vecs := buildBenchIndexCfg(b, 1000, Config{
		Namespace:      testNamespace,
		Dimensions:     Dim768,
		ElementType:    ElementFloat32,
		M:              DefaultM,
		M0:             DefaultM0,
		EfConstruction: 100,
		EfSearch:       64,
		// Force ANN even though the fixture is below the default flat cutoff.
		FlatThreshold: -1,
	})
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

func BenchmarkSearchFloat32_512ANN(b *testing.B) {
	idx, vecs := buildBenchIndexCfg(b, 1000, Config{
		Namespace:      testNamespace,
		Dimensions:     Dim512,
		ElementType:    ElementFloat32,
		M:              DefaultM,
		M0:             DefaultM0,
		EfConstruction: 100,
		EfSearch:       64,
		FlatThreshold:  -1,
	})
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

func BenchmarkSearchFloat32_512CosineANN(b *testing.B) {
	idx, vecs := buildBenchIndexCfg(b, 1000, Config{
		Namespace:      testNamespace,
		Dimensions:     Dim512,
		ElementType:    ElementFloat32,
		Metric:         MetricCosine,
		M:              DefaultM,
		M0:             DefaultM0,
		EfConstruction: 100,
		EfSearch:       64,
		FlatThreshold:  -1,
	})
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

func BenchmarkSearchFloat32_768CosineANN(b *testing.B) {
	idx, vecs := buildBenchIndexCfg(b, 1000, Config{
		Namespace:      testNamespace,
		Dimensions:     Dim768,
		ElementType:    ElementFloat32,
		Metric:         MetricCosine,
		M:              DefaultM,
		M0:             DefaultM0,
		EfConstruction: 100,
		EfSearch:       64,
		FlatThreshold:  -1,
	})
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

func BenchmarkAddFloat32_512(b *testing.B) {
	idx, _ := buildBenchIndexCfg(b, 1000, Config{
		Namespace:      testNamespace,
		Dimensions:     Dim512,
		ElementType:    ElementFloat32,
		M:              DefaultM,
		M0:             DefaultM0,
		EfConstruction: 100,
		EfSearch:       64,
	})
	defer idx.Close()
	rng := rand.New(rand.NewSource(84))
	vec := randFloatVec(rng, Dim512)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := idx.AddFloat32(NodeID(1_000_000+i), vec); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkAddFloat32_768(b *testing.B) {
	idx, _ := buildBenchIndexCfg(b, 1000, Config{
		Namespace:      testNamespace,
		Dimensions:     Dim768,
		ElementType:    ElementFloat32,
		M:              DefaultM,
		M0:             DefaultM0,
		EfConstruction: 100,
		EfSearch:       64,
	})
	defer idx.Close()
	rng := rand.New(rand.NewSource(83))
	vec := randFloatVec(rng, Dim768)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := idx.AddFloat32(NodeID(1_000_000+i), vec); err != nil {
			b.Fatal(err)
		}
	}
}

func TestSearchSteadyStateAllocs(t *testing.T) {
	if raceEnabled {
		t.Skip("allocation counts are calibrated for non-race builds; the race detector adds bookkeeping allocs")
	}
	idx, err := NewMemory(Config{Namespace: testNamespace, Dimensions: Dim512, ElementType: ElementFloat32})
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

func benchPersistentIndex(b *testing.B, n int, durability Durability, opts ...Option) *VectorIdx {
	b.Helper()
	path := filepath.Join(b.TempDir(), "bench.idx")
	idx, err := NewStore(path, testConfigFloat512(), durability, opts...)
	if err != nil {
		b.Fatal(err)
	}
	rng := rand.New(rand.NewSource(79))
	vec := randFloatVec(rng, Dim512)
	for i := 0; i < n; i++ {
		if err := idx.AddFloat32(NodeID(i+1), vec); err != nil {
			b.Fatal(err)
		}
	}
	return idx
}

// BenchmarkWALAppendOS measures append cost without per-record fsync.
func BenchmarkWALAppendOS(b *testing.B) {
	idx := benchPersistentIndex(b, 500, DurabilityOS)
	defer idx.Close()
	rng := rand.New(rand.NewSource(80))
	vec := randFloatVec(rng, Dim512)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := idx.AddFloat32(NodeID(1_000_000+i), vec); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkWALAppendSync measures append cost with per-record fsync.
func BenchmarkWALAppendSync(b *testing.B) {
	idx := benchPersistentIndex(b, 500, DurabilitySync)
	defer idx.Close()
	rng := rand.New(rand.NewSource(81))
	vec := randFloatVec(rng, Dim512)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := idx.AddFloat32(NodeID(1_000_000+i), vec); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkWALAppendGroupCommit measures append cost with DurabilityOS plus a
// sync interval: fsync cost is amortized across the write batch.
func BenchmarkWALAppendGroupCommit(b *testing.B) {
	idx := benchPersistentIndex(b, 500, DurabilityOS, WithSyncInterval(time.Millisecond))
	defer idx.Close()
	rng := rand.New(rand.NewSource(82))
	vec := randFloatVec(rng, Dim512)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := idx.AddFloat32(NodeID(1_000_000+i), vec); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkCheckpoint measures an incremental delta checkpoint: one pending
// node is published as a one-record delta generation plus manifest update.
func BenchmarkCheckpoint(b *testing.B) {
	idx := benchPersistentIndex(b, 5000, DurabilityOS)
	defer idx.Close()
	mark := func(id NodeID) {
		idx.writeMu.Lock()
		if idx.pending == nil {
			idx.pending = make(map[NodeID]struct{})
		}
		idx.pending[id] = struct{}{}
		idx.pendingMutations++
		idx.writeMu.Unlock()
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		mark(NodeID(1 + i%5000))
		if err := idx.Checkpoint(); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkVacuum measures a full base rewrite (the previous Checkpoint
// cost) plus manifest reset.
func BenchmarkVacuum(b *testing.B) {
	idx := benchPersistentIndex(b, 2000, DurabilityOS)
	defer idx.Close()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := idx.Vacuum(); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkCompact measures a full graph rebuild plus checkpoint.
func BenchmarkCompact(b *testing.B) {
	idx := benchPersistentIndex(b, 1000, DurabilityOS)
	defer idx.Close()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := idx.Compact(); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkOpen measures cold open of a vacuumed base generation (no delta
// or WAL replay).
func BenchmarkOpen(b *testing.B) {
	idx := benchPersistentIndex(b, 2000, DurabilityOS)
	path := idx.path
	if err := idx.Vacuum(); err != nil {
		b.Fatal(err)
	}
	if err := idx.Close(); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		re, err := OpenStore(path, DurabilityOS)
		if err != nil {
			b.Fatal(err)
		}
		if err := re.Close(); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkOpenDelta measures cold open of a base plus one delta generation
// holding the whole index: delta application re-inserts nodes like WAL
// replay, which is why Vacuum or auto-vacuum materializes bases over time.
func BenchmarkOpenDelta(b *testing.B) {
	idx := benchPersistentIndex(b, 100, DurabilityOS)
	path := idx.path
	if err := idx.Checkpoint(); err != nil {
		b.Fatal(err)
	}
	if err := idx.Close(); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		re, err := OpenStore(path, DurabilityOS)
		if err != nil {
			b.Fatal(err)
		}
		if err := re.Close(); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkOpenReplay measures open when the whole index is still in the WAL
// (no checkpoint since Create): replay re-inserts every node.
func BenchmarkOpenReplay(b *testing.B) {
	idx := benchPersistentIndex(b, 1000, DurabilityOS)
	path := idx.path
	if err := idx.Close(); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		re, err := OpenStore(path, DurabilityOS)
		if err != nil {
			b.Fatal(err)
		}
		if err := re.Close(); err != nil {
			b.Fatal(err)
		}
	}
}
