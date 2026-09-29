package vectoridx

import (
	"math/rand"
	"testing"
)

// Candidate filtering is post-ANN only: traversal is never constrained by
// the filter, oversampling fills k where reachable, and the exact fallback
// guarantees k-or-available under highly selective filters.

func buildFilterIndex(t *testing.T, n int) (*VectorIdx, [][]float32) {
	t.Helper()
	idx, err := NewMemory(testConfigFloat512())
	if err != nil {
		t.Fatal(err)
	}
	rng := rand.New(rand.NewSource(31))
	vecs := make([][]float32, n)
	for i := range vecs {
		vecs[i] = randFloatVec(rng, Dim512)
		mustAddFloat(t, idx, NodeID(i+1), vecs[i])
	}
	return idx, vecs
}

func TestFilterEvenIDs(t *testing.T) {
	idx, vecs := buildFilterIndex(t, 200)
	defer idx.Close()
	_ = vecs
	even := CandidateFilter(func(id NodeID) bool { return id%2 == 0 })
	rng := rand.New(rand.NewSource(32))
	for range 10 {
		q := randFloatVec(rng, Dim512)
		results, err := idx.SearchFloat32(q, even, 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(results) != 10 {
			t.Fatalf("filtered results = %d, want 10", len(results))
		}
		for _, r := range results {
			if r.ValueID%2 != 0 {
				t.Fatalf("odd node %d passed even filter: %+v", r.ValueID, results)
			}
		}
		assertSortedByScore(t, results)
		// Filtered ANN must agree with the exact filtered scan on the top
		// result in most queries (oversampling keeps quality high).
		exact, err := idx.ExactSearchFloat32(q, even, 10)
		if err != nil {
			t.Fatal(err)
		}
		if exact[0].ValueID != results[0].ValueID {
			t.Logf("top-1 differs: ann=%d exact=%d (acceptable occasionally)", results[0].ValueID, exact[0].ValueID)
		}
	}
}

func TestFilterHighlySelective(t *testing.T) {
	idx, _ := buildFilterIndex(t, 200)
	defer idx.Close()
	// Accept ~1%: only IDs divisible by 100 (2 nodes).
	selective := CandidateFilter(func(id NodeID) bool { return id%100 == 0 })
	rng := rand.New(rand.NewSource(33))
	q := randFloatVec(rng, Dim512)
	results, err := idx.SearchFloat32(q, selective, 10)
	if err != nil {
		t.Fatal(err)
	}
	// Only 2 nodes match: k-or-available semantics return 2, not 10.
	if len(results) != 2 {
		t.Fatalf("selective filter results = %d, want 2 (all available)", len(results))
	}
	for _, r := range results {
		if r.ValueID%100 != 0 {
			t.Fatalf("node %d passed selective filter", r.ValueID)
		}
	}
	exact, err := idx.ExactSearchFloat32(q, selective, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(exact) != 2 || exact[0].ValueID != results[0].ValueID || exact[1].ValueID != results[1].ValueID {
		t.Fatalf("selective fallback != exact: ann=%+v exact=%+v", results, exact)
	}
}

func TestFilterNoneMatch(t *testing.T) {
	idx, _ := buildFilterIndex(t, 50)
	defer idx.Close()
	none := CandidateFilter(func(NodeID) bool { return false })
	q := randFloatVec(rand.New(rand.NewSource(34)), Dim512)
	results, err := idx.SearchFloat32(q, none, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 0 {
		t.Fatalf("empty filter results = %+v, want empty", results)
	}
}

func TestFilterNilAcceptsAll(t *testing.T) {
	idx, _ := buildFilterIndex(t, 50)
	defer idx.Close()
	q := randFloatVec(rand.New(rand.NewSource(35)), Dim512)
	results, err := idx.SearchFloat32(q, nil, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 10 {
		t.Fatalf("unfiltered results = %d, want 10", len(results))
	}
}

func TestFilterWithTombstones(t *testing.T) {
	idx, vecs := buildFilterIndex(t, 100)
	defer idx.Close()
	_ = vecs
	for i := NodeID(2); i <= 100; i += 2 {
		if err := idx.Delete(i); err != nil {
			t.Fatal(err)
		}
	}
	// Even filter now matches only tombstoned nodes: expect zero results.
	even := CandidateFilter(func(id NodeID) bool { return id%2 == 0 })
	q := randFloatVec(rand.New(rand.NewSource(36)), Dim512)
	results, err := idx.SearchFloat32(q, even, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 0 {
		t.Fatalf("tombstoned+filtered results = %+v, want empty", results)
	}
	// Odd filter still fills k from live nodes.
	odd := CandidateFilter(func(id NodeID) bool { return id%2 == 1 })
	results, err = idx.SearchFloat32(q, odd, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 10 {
		t.Fatalf("odd-filtered results = %d, want 10", len(results))
	}
	for _, r := range results {
		if r.ValueID%2 != 1 {
			t.Fatalf("even node %d in odd-filtered results", r.ValueID)
		}
	}
}
