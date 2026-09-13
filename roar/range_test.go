package roar

import (
	"math/rand/v2"
	"slices"
	"testing"

	"github.com/weaviate/sroar"
)

func TestRemoveRange(t *testing.T) {
	ids := []uint64{1, 2, 3, 4, 5, 1<<16 + 10, 1<<16 + 20, 2<<16 + 100, 3<<16 + 65535}
	bitmap, err := FromSorted(ids)
	if err != nil {
		t.Fatal(err)
	}

	for _, test := range []struct {
		name string
		lo   uint64
		hi   uint64
		want []uint64
	}{
		{"empty range", 2, 2, ids},
		{"reversed", 5, 1, ids},
		{"single low", 3, 4, []uint64{1, 2, 4, 5, 1<<16 + 10, 1<<16 + 20, 2<<16 + 100, 3<<16 + 65535}},
		{"across keys", 1<<16 + 15, 3 << 16, []uint64{1, 2, 3, 4, 5, 1<<16 + 10, 3<<16 + 65535}},
		{"full span", 0, ^uint64(0), nil},
		{"from start", 0, 3, []uint64{3, 4, 5, 1<<16 + 10, 1<<16 + 20, 2<<16 + 100, 3<<16 + 65535}},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := bitmap.RemoveRange(test.lo, test.hi).AppendTo(nil)
			if !slices.Equal(got, test.want) {
				t.Fatalf("RemoveRange(%d,%d) = %v, want %v", test.lo, test.hi, got, test.want)
			}
		})
	}
}

func TestRemoveRangeAgainstSroar(t *testing.T) {
	rng := rand.New(rand.NewPCG(3, 4))
	for round := 0; round < 30; round++ {
		pool := keyPool(rng, 16)
		ids := randomSetOver(rng, pool)
		ids = append(ids, denseSortedIDs(rng, 2)...)
		slices.Sort(ids)
		ids = slices.Compact(ids)

		bitmap, err := FromSorted(ids)
		if err != nil {
			t.Fatal(err)
		}
		reference := sroar.FromSortedList(ids)

		lo := rng.Uint64()
		hi := lo + rng.Uint64()/2
		if hi < lo {
			hi = ^uint64(0)
		}

		got := bitmap.RemoveRange(lo, hi).AppendTo(nil)
		reference.RemoveRange(lo, hi)
		want := reference.ToArray()

		if !slices.Equal(got, want) {
			t.Fatalf("round %d: RemoveRange(%d,%d) mismatch", round, lo, hi)
		}
	}
}

func TestRemoveRanges(t *testing.T) {
	ids := []uint64{1, 2, 3, 4, 5, 1<<16 + 10, 1<<16 + 20, 2<<16 + 100, 3<<16 + 65535}
	bitmap, err := FromSorted(ids)
	if err != nil {
		t.Fatal(err)
	}

	for _, test := range []struct {
		name   string
		ranges []IDRange
		want   []uint64
	}{
		{"nil", nil, ids},
		{"empty", []IDRange{}, ids},
		{"all empty", []IDRange{{5, 5}, {9, 3}}, ids},
		{"single", []IDRange{{3, 4}}, []uint64{1, 2, 4, 5, 1<<16 + 10, 1<<16 + 20, 2<<16 + 100, 3<<16 + 65535}},
		{"two disjoint", []IDRange{{2, 3}, {1<<16 + 15, 3 << 16}}, []uint64{1, 3, 4, 5, 1<<16 + 10, 3<<16 + 65535}},
		{"unsorted", []IDRange{{1<<16 + 15, 3 << 16}, {2, 3}}, []uint64{1, 3, 4, 5, 1<<16 + 10, 3<<16 + 65535}},
		{"overlapping merge", []IDRange{{1, 4}, {3, 6}}, []uint64{1<<16 + 10, 1<<16 + 20, 2<<16 + 100, 3<<16 + 65535}},
		{"adjacent merge", []IDRange{{1, 3}, {3, 5}}, []uint64{5, 1<<16 + 10, 1<<16 + 20, 2<<16 + 100, 3<<16 + 65535}},
		{"full span", []IDRange{{0, ^uint64(0)}}, nil},
		{"top key edge", []IDRange{{3<<16 + 65534, ^uint64(0)}}, []uint64{1, 2, 3, 4, 5, 1<<16 + 10, 1<<16 + 20, 2<<16 + 100}},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := bitmap.RemoveRanges(test.ranges).AppendTo(nil)
			if !slices.Equal(got, test.want) {
				t.Fatalf("RemoveRanges(%v) = %v, want %v", test.ranges, got, test.want)
			}
		})
	}
}

func TestRemoveRangesAgainstSequential(t *testing.T) {
	rng := rand.New(rand.NewPCG(11, 17))
	for round := range 30 {
		pool := keyPool(rng, 16)
		ids := randomSetOver(rng, pool)
		ids = append(ids, denseSortedIDs(rng, 2)...)
		slices.Sort(ids)
		ids = slices.Compact(ids)

		bitmap, err := FromSorted(ids)
		if err != nil {
			t.Fatal(err)
		}

		var ranges []IDRange
		for range 1 + rng.IntN(8) {
			lo := rng.Uint64()
			hi := lo + rng.Uint64()/16
			if hi < lo {
				hi = ^uint64(0)
			}
			ranges = append(ranges, IDRange{Lo: lo, Hi: hi})
		}

		want := bitmap
		for _, r := range ranges {
			want = want.RemoveRange(r.Lo, r.Hi)
		}
		got := bitmap.RemoveRanges(ranges).AppendTo(nil)
		if !slices.Equal(got, want.AppendTo(nil)) {
			t.Fatalf("round %d: RemoveRanges batch/sequential mismatch", round)
		}

		// The batch result must also match sroar applied sequentially.
		reference := sroar.FromSortedList(ids)
		for _, r := range ranges {
			reference.RemoveRange(r.Lo, r.Hi)
		}
		if !slices.Equal(got, reference.ToArray()) {
			t.Fatalf("round %d: RemoveRanges/sroar mismatch", round)
		}
	}
}
