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
