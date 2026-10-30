package vectoridx

import (
	"context"
	"math/rand"
	"testing"
	"time"
)

func testConfigCosine512() Config {
	return Config{
		Namespace:   testNamespace,
		Dimensions:  Dim512,
		ElementType: ElementFloat32,
		Metric:      MetricCosine,
	}
}

func testConfigDot512() Config {
	return Config{
		Namespace:   testNamespace,
		Dimensions:  Dim512,
		ElementType: ElementFloat32,
		Metric:      MetricDot,
	}
}

func TestCosineMetricRoundTrip(t *testing.T) {
	idx, err := NewMemory(testConfigCosine512())
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()
	rng := rand.New(rand.NewSource(101))
	var query []float32
	for i := NodeID(1); i <= 20; i++ {
		vec := randFloatVec(rng, Dim512)
		if i == 1 {
			query = append([]float32(nil), vec...)
		}
		mustAddFloat(t, idx, i, vec)
	}
	results, err := idx.SearchFloat32(query, nil, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].ValueID != 1 || results[0].Score != 0 {
		t.Fatalf("cosine self-search = %+v", results)
	}
	assertSortedByScore(t, results)
	exact, err := idx.ExactSearchFloat32(query, nil, 5)
	if err != nil {
		t.Fatal(err)
	}
	approx, err := idx.SearchFloat32(query, nil, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(approx) == 0 || approx[0].ValueID != exact[0].ValueID {
		t.Fatalf("cosine top-1 ann=%v exact=%v", approx, exact)
	}
}

func TestDotMetricRoundTrip(t *testing.T) {
	idx, err := NewMemory(testConfigDot512())
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()
	rng := rand.New(rand.NewSource(102))
	var query []float32
	for i := NodeID(1); i <= 20; i++ {
		vec := randFloatVec(rng, Dim512)
		if i == 1 {
			query = append([]float32(nil), vec...)
		}
		mustAddFloat(t, idx, i, vec)
	}
	results, err := idx.SearchFloat32(query, nil, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].ValueID != 1 || results[0].Score != 0 {
		t.Fatalf("dot self-search = %+v", results)
	}
}

func TestCosineOrthogonalOrdering(t *testing.T) {
	idx, err := NewMemory(testConfigCosine512())
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()
	a := make([]float32, Dim512)
	b := make([]float32, Dim512)
	a[0], b[1] = 1, 1
	mustAddFloat(t, idx, 1, a)
	mustAddFloat(t, idx, 2, b)
	// Query equal to a: a must rank first with score 0.
	results, err := idx.SearchFloat32(a, nil, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 || results[0].ValueID != 1 || results[0].Score != 0 {
		t.Fatalf("orthogonal ordering = %+v", results)
	}
	if results[1].Score <= results[0].Score {
		t.Fatalf("orthogonal not farther: %+v", results)
	}
}

func TestMetricConfigValidation(t *testing.T) {
	bad := testConfigFloat512()
	bad.Metric = 9
	if _, err := NewMemory(bad); err == nil {
		t.Fatal("invalid metric accepted")
	}
}

func TestDynamicEfSearch(t *testing.T) {
	cfg := testConfigFloat512()
	cfg.DynamicEfFactor = 4
	cfg.DynamicEfMin = 8
	cfg.DynamicEfMax = 64
	idx, err := NewMemory(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()
	rng := rand.New(rand.NewSource(103))
	var query []float32
	for i := NodeID(1); i <= 60; i++ {
		vec := randFloatVec(rng, Dim512)
		if i == 1 {
			query = append([]float32(nil), vec...)
		}
		mustAddFloat(t, idx, i, vec)
	}
	results, err := idx.SearchFloat32(query, nil, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 5 || results[0].ValueID != 1 {
		t.Fatalf("dynamic-ef search = %+v", results)
	}
	if err := idx.SetSearchParams(32, 0, 0, 0, 0, 0, 0); err != nil {
		t.Fatal(err)
	}
	results, err = idx.SearchFloat32(query, nil, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 5 || results[0].ValueID != 1 {
		t.Fatalf("static-ef search = %+v", results)
	}
	if err := idx.SetSearchParams(0, 0, 0, 0, 0, 0, 999); err == nil {
		t.Fatal("invalid oversample accepted")
	}
}

func TestHeuristicToggle(t *testing.T) {
	idx, err := NewMemory(testConfigFloat512())
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()
	if !idx.Config().UseHeuristic {
		t.Fatal("heuristic should default on")
	}
	rng := rand.New(rand.NewSource(104))
	var query []float32
	for i := NodeID(1); i <= 40; i++ {
		vec := randFloatVec(rng, Dim512)
		if i == 1 {
			query = append([]float32(nil), vec...)
		}
		mustAddFloat(t, idx, i, vec)
	}
	if err := idx.SetHeuristic(false); err != nil {
		t.Fatal(err)
	}
	if idx.Config().UseHeuristic {
		t.Fatal("heuristic should be off")
	}
	results, err := idx.SearchFloat32(query, nil, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].ValueID != 1 {
		t.Fatalf("closest-only self-search = %+v", results)
	}
	if err := idx.SetHeuristic(true); err != nil {
		t.Fatal(err)
	}
}

func TestFlatThresholdAndFilterHint(t *testing.T) {
	cfg := testConfigFloat512()
	cfg.FlatThreshold = 10000 // force exact path
	idx, err := NewMemory(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()
	rng := rand.New(rand.NewSource(105))
	var query []float32
	for i := NodeID(1); i <= 30; i++ {
		vec := randFloatVec(rng, Dim512)
		if i == 1 {
			query = append([]float32(nil), vec...)
		}
		mustAddFloat(t, idx, i, vec)
	}
	results, err := idx.SearchFloat32(query, nil, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 5 || results[0].ValueID != 1 {
		t.Fatalf("flat search = %+v", results)
	}
	// Filter hint at/below the cutoff must take the exact path with identical
	// results to the unhinted search.
	even := CandidateFilter(func(id NodeID) bool { return id%2 == 0 })
	a, err := idx.SearchFloat32(query, even, 5)
	if err != nil {
		t.Fatal(err)
	}
	b, err := idx.SearchWithFilterHint(query, even, 5, 15)
	if err != nil {
		t.Fatal(err)
	}
	if len(a) != len(b) {
		t.Fatalf("hint/plain len %d vs %d", len(a), len(b))
	}
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("hint/plain differ at %d: %+v vs %+v", i, a, b)
		}
	}
}

func TestAsyncAddDelete(t *testing.T) {
	idx, err := NewMemory(testConfigFloat512())
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()
	rng := rand.New(rand.NewSource(106))
	vec := randFloatVec(rng, Dim512)
	idx.AddAsync(7, vec)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := idx.WaitIdle(ctx); err != nil {
		t.Fatal(err)
	}
	results, err := idx.SearchFloat32(vec, nil, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].ValueID != 7 {
		t.Fatalf("async add search = %+v", results)
	}
	idx.DeleteAsync(7)
	if err := idx.WaitIdle(ctx); err != nil {
		t.Fatal(err)
	}
	if got := idx.LiveCount(); got != 0 {
		t.Fatalf("LiveCount after async delete = %d", got)
	}
	if got := idx.PendingOps(); got != 0 {
		t.Fatalf("PendingOps = %d", got)
	}
}

func TestMetricPersistenceRoundTrip(t *testing.T) {
	path := t.TempDir() + "/cos.vec"
	idx, err := NewStore(path, testConfigCosine512(), DurabilitySync)
	if err != nil {
		t.Fatal(err)
	}
	rng := rand.New(rand.NewSource(107))
	vec := randFloatVec(rng, Dim512)
	mustAddFloat(t, idx, 3, vec)
	if err := idx.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	if err := idx.Close(); err != nil {
		t.Fatal(err)
	}
	re, err := OpenStore(path, DurabilitySync)
	if err != nil {
		t.Fatal(err)
	}
	defer re.Close()
	if re.Config().Metric != MetricCosine {
		t.Fatalf("persisted metric = %d", re.Config().Metric)
	}
	results, err := re.SearchFloat32(vec, nil, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].ValueID != 3 || results[0].Score != 0 {
		t.Fatalf("reopened cosine search = %+v", results)
	}
}

func TestDotScoreEncoding(t *testing.T) {
	// Zero-based: self dot maps to score 0, smaller dots map higher.
	if got := EncodeDotScore(-10, 10); got != 0 {
		t.Fatalf("self dot score = %d", got)
	}
	if a, b := EncodeDotScore(-10, 10), EncodeDotScore(-5, 10); a >= b {
		t.Fatalf("dot scores not monotonic: %d vs %d", a, b)
	}
	if got := EncodeCosineScore(0); got != 0 {
		t.Fatalf("cosine zero = %d", got)
	}
	if a, b := EncodeCosineScore(0.5), EncodeCosineScore(1.5); a >= b {
		t.Fatalf("cosine scores not monotonic: %d vs %d", a, b)
	}
}

func TestAdaptiveFilterOversample(t *testing.T) {
	s := &vectorSnapshot{ids: make([]NodeID, 10000)}
	s.cfg.EfSearch = 48
	s.cfg.OversampleFactor = 8
	filter := CandidateFilter(func(NodeID) bool { return true })
	cases := []struct {
		name       string
		hint       int
		oversample uint32
		want       int
	}{
		{"unknown", UnknownFilterHint, 0, 80},
		{"all-match", 10000, 0, 60},
		{"half", 5000, 0, 60},
		{"quarter", 2500, 0, 80},
		{"selective", 1000, 0, 80},
		{"explicit", 5000, 4, 48},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := s.effectiveEf(10, filter, c.hint, 0, c.oversample); got != c.want {
				t.Fatalf("effectiveEf = %d, want %d", got, c.want)
			}
		})
	}
	if got := s.effectiveEf(10, nil, 5000, 0, 0); got != 48 {
		t.Fatalf("unfiltered effectiveEf = %d, want 48", got)
	}
}
