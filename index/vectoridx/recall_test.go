package vectoridx

import (
	"math/rand"
	"testing"
)

// HNSW recall against the exact-scan oracle. These tests pin graph quality:
// an optimization that tanks recall must fail here, not just in benchmarks.

func buildRecallIndex(t *testing.T, n int, seed int64) (*VectorIdx, [][]float32) {
	t.Helper()
	idx, err := New(Config{
		SpaceID:        NewSpaceID(IndexTypeImage, 9),
		Dimensions:     Dim512,
		ElementType:    ElementFloat32,
		M:              DefaultM,
		M0:             DefaultM0,
		EfConstruction: 128,
		EfSearch:       64,
	})
	if err != nil {
		t.Fatal(err)
	}
	rng := rand.New(rand.NewSource(seed))
	vecs := make([][]float32, n)
	for i := range vecs {
		vecs[i] = randFloatVec(rng, Dim512)
		mustAddFloat(t, idx, NodeID(i+1), vecs[i])
	}
	return idx, vecs
}

func recallAtK(t *testing.T, idx *VectorIdx, vecs [][]float32, queries int, k int) float64 {
	t.Helper()
	rng := rand.New(rand.NewSource(999))
	hits, total := 0, 0
	for range queries {
		q := randFloatVec(rng, Dim512)
		approx, err := idx.SearchFloat32(q, nil, k)
		if err != nil {
			t.Fatal(err)
		}
		exact, err := idx.ExactSearchFloat32(q, nil, k)
		if err != nil {
			t.Fatal(err)
		}
		if len(approx) != k || len(exact) != k {
			t.Fatalf("approx %d exact %d, want %d", len(approx), len(exact), k)
		}
		assertSortedByScore(t, approx)
		exactSet := make(map[NodeID]bool, k)
		for _, r := range exact {
			exactSet[NodeID(r.ImageID)] = true
		}
		for _, r := range approx {
			if exactSet[NodeID(r.ImageID)] {
				hits++
			}
			total++
		}
	}
	_ = vecs
	return float64(hits) / float64(total)
}

func TestHNSWRecallAt5(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping recall in short mode")
	}
	idx, vecs := buildRecallIndex(t, 300, 21)
	defer idx.Close()
	if recall := recallAtK(t, idx, vecs, 20, 5); recall < 0.7 {
		t.Fatalf("recall@5 = %.3f, want >= 0.70", recall)
	}
}

func TestHNSWRecallAfterCompact(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping recall in short mode")
	}
	idx, vecs := buildRecallIndex(t, 200, 22)
	defer idx.Close()
	rng := rand.New(rand.NewSource(23))
	for i := NodeID(1); i <= 20; i++ {
		_ = i
		_ = rng
	}
	// Delete every 10th node, then compact and re-measure recall on survivors.
	for i := 10; i <= 200; i += 10 {
		if err := idx.Delete(NodeID(i)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := idx.Compact(); err != nil {
		t.Fatal(err)
	}
	if got := idx.TombstoneCount(); got != 0 {
		t.Fatalf("TombstoneCount after compact = %d, want 0", got)
	}
	if got := idx.LiveCount(); got != 180 {
		t.Fatalf("LiveCount after compact = %d, want 180", got)
	}
	if recall := recallAtK(t, idx, vecs, 10, 5); recall < 0.6 {
		t.Fatalf("post-compact recall@5 = %.3f, want >= 0.60", recall)
	}
}

func TestExactSearchIsGroundTruth(t *testing.T) {
	idx, err := New(testConfigFloat512())
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()
	rng := rand.New(rand.NewSource(24))
	for i := NodeID(1); i <= 30; i++ {
		mustAddFloat(t, idx, i, randFloatVec(rng, Dim512))
	}
	q := randFloatVec(rng, Dim512)
	exact, err := idx.ExactSearchFloat32(q, nil, 5)
	if err != nil {
		t.Fatal(err)
	}
	// Brute-force in the test itself with the scalar kernel (lock-free).
	token := idx.readers.Acquire()
	snap := idx.view.Load()
	expected := make(map[NodeID]float64, len(snap.ids))
	rawQ := encodeRawFloat32(nil, q)
	for ord, id := range snap.ids {
		expected[id] = snap.dist(rawQ, snap.vectorAt(ord))
	}
	idx.readers.Release(token)
	if len(expected) != 30 {
		t.Fatalf("scanned %d nodes, want 30", len(expected))
	}
	for i, r := range exact {
		d, ok := expected[NodeID(r.ImageID)]
		if !ok {
			t.Fatalf("result %d unknown node %d", i, r.ImageID)
		}
		if r.Score != EncodeScore(d) {
			t.Fatalf("result %d score mismatch", i)
		}
	}
}
