package geoidx

import (
	"slices"
	"testing"

	"github.com/evanoberholster/imageIdx/roar"
)

// filterTestIndex builds an index with sparse cells (k1: 2 IDs, k2: 4 IDs),
// a dense cell (k3: 10 IDs), and a second dense cell (k4: 7 IDs).
func filterTestIndex(t *testing.T) (*Index, []S2Key) {
	t.Helper()
	k1 := keyAt(t, 37.0, -122.5)
	k2 := keyAt(t, 37.01, -122.49)
	k3 := keyAt(t, 37.02, -122.48)
	k4 := keyAt(t, 37.03, -122.47)
	dense := func(base uint64, count int) []ImageID {
		ids := make([]ImageID, count)
		for i := range ids {
			ids[i] = ImageID(base + uint64(i))
		}
		return ids
	}
	index := testIndexFromCells(t, 1, map[S2Key][]ImageID{
		k1: {101, 102},
		k2: {201, 202, 203, 204},
		k3: dense(301, 10),
		k4: dense(401, 7),
	})
	return index, []S2Key{k1, k2, k3, k4}
}

func collectSorted(index *Index, ranges []Range) []ImageID {
	got := index.SearchRanges(nil, ranges)
	slices.Sort(got)
	return slices.Compact(got)
}

func TestFilterRangesMatchesCollect(t *testing.T) {
	index, keys := filterTestIndex(t)
	defer index.Close()
	k1, k2, k3, k4 := keys[0], keys[1], keys[2], keys[3]
	hi, lo := k1, k1
	for _, key := range keys[1:] {
		if key > hi {
			hi = key
		}
		if key < lo {
			lo = key
		}
	}

	// Candidate universes: empty, disjoint, partial, and covering.
	candidateSets := [][]ImageID{
		nil,
		{},
		{1, 2, 3},
		{102, 203, 305, 405, 999},
		{101, 102, 201, 202, 203, 204},
		{301, 302, 303, 304, 305, 306, 307, 308, 309, 310, 401, 402},
	}
	ranges := [][]Range{
		nil,
		{},
		{{Start: k1, End: k1}},
		{{Start: k3, End: k3}},
		{{Start: k1, End: k2}},
		{{Start: k1, End: k4}},
		{{Start: k2, End: k3}, {Start: k4, End: k4}},
		{{Start: hi, End: lo}}, // empty: End < Start
	}
	for _, query := range ranges {
		collected := collectSorted(index, query)
		for _, candidates := range candidateSets {
			want := slices.Clone(collected)
			want = intersectSortedForTest(want, candidates)
			got := index.FilterRanges(nil, candidates, query)
			slices.Sort(got)
			if !slices.Equal(got, want) {
				t.Fatalf("FilterRanges(%v, %v) = %v, want %v (collect %v)", query, candidates, got, want, collected)
			}
		}
		if got := index.CountCells(query); got != countCellsForTest(index, query) {
			t.Fatalf("CountCells(%v) = %d, want %d", query, got, countCellsForTest(index, query))
		}
	}
}

func intersectSortedForTest(left, right []ImageID) []ImageID {
	out := left[:0]
	li, ri := 0, 0
	for li < len(left) && ri < len(right) {
		switch {
		case left[li] < right[ri]:
			li++
		case left[li] > right[ri]:
			ri++
		default:
			out = append(out, left[li])
			li++
			ri++
		}
	}
	return out
}

// countCellsForTest counts covered cells by scanning the published view
// without using CountCells.
func countCellsForTest(index *Index, ranges []Range) int {
	view := index.view.Load()
	count := 0
	for _, keyRange := range ranges {
		if keyRange.End < keyRange.Start {
			continue
		}
		for _, shard := range view.shards {
			if shard == nil {
				continue
			}
			for _, key := range shard.sparse.keys {
				if key >= keyRange.Start && key <= keyRange.End {
					count++
				}
			}
			for _, key := range shard.dense.keys {
				if key >= keyRange.Start && key <= keyRange.End {
					count++
				}
			}
		}
	}
	return count
}

func TestFilterRangesAfterPromotionAndDemotion(t *testing.T) {
	index, keys := filterTestIndex(t)
	defer index.Close()
	k1, k3 := keys[0], keys[2]

	// Promote k1 to dense with four inserts, then demote k3 to sparse with
	// eight removals.
	var moves []Mutation
	for id := ImageID(103); id <= 106; id++ {
		moves = append(moves, Mutation{ImageID: id, To: k1})
	}
	for id := ImageID(303); id <= 310; id++ {
		moves = append(moves, Mutation{ImageID: id, From: k3, To: 0})
	}
	if err := index.Apply(moves); err != nil {
		t.Fatal(err)
	}
	if err := index.Flush(); err != nil {
		t.Fatal(err)
	}
	query := []Range{{Start: k1, End: k3}}
	candidates := []ImageID{101, 102, 103, 106, 301, 302, 311, 999}
	want := intersectSortedForTest(collectSorted(index, query), candidates)
	got := index.FilterRanges(nil, candidates, query)
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Fatalf("FilterRanges after flush = %v, want %v", got, want)
	}
	if got := index.CountCells(query); got != countCellsForTest(index, query) {
		t.Fatalf("CountCells after flush = %d, want %d", got, countCellsForTest(index, query))
	}
}

func TestCollectRangesMatchesSearchRanges(t *testing.T) {
	index, keys := filterTestIndex(t)
	defer index.Close()
	lo, hi := keys[0], keys[0]
	for _, key := range keys[1:] {
		if key < lo {
			lo = key
		}
		if key > hi {
			hi = key
		}
	}
	queries := [][]Range{
		{{Start: lo, End: hi}},
		{{Start: keys[0], End: keys[0]}, {Start: keys[2], End: keys[2]}},
		{{Start: hi + 1, End: hi}},
		{{Start: 0, End: ^S2Key(0)}},
	}
	mutable := roar.NewMutableBitmap()
	for _, query := range queries {
		want := collectSorted(index, query)
		mutable.Reset()
		index.CollectRanges(mutable, query)
		if got := mutable.AppendTo(nil); !slices.Equal(got, want) {
			t.Fatalf("CollectRanges(%v) = %v, want %v", query, got, want)
		}
		if got := mutable.Cardinality(); got != uint64(len(want)) {
			t.Fatalf("CollectRanges(%v) cardinality = %d, want %d", query, got, len(want))
		}
	}
}
