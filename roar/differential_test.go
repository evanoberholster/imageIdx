package roar

import (
	"math/rand/v2"
	"slices"
	"testing"

	"github.com/weaviate/sroar"
)

// TestDifferentialAgainstSroar compares every implemented operation against
// weaviate/sroar over randomized sets with shared keys, mixing tiny arrays,
// medium arrays, and dense bitmap containers.
func TestDifferentialAgainstSroar(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	for round := 0; round < 30; round++ {
		pool := keyPool(rng, 24)
		sets := [][]uint64{
			randomSetOver(rng, pool),
			randomSetOver(rng, pool),
			randomSetOver(rng, pool),
		}
		views := make([]Bitmap, len(sets))
		bitmaps := make([]*sroar.Bitmap, len(sets))
		for index, ids := range sets {
			bitmap, err := FromSorted(ids)
			if err != nil {
				t.Fatal(err)
			}
			views[index] = bitmap
			bitmaps[index] = sroar.FromSortedList(ids)
		}

		for index := range views {
			if views[index].Cardinality() != uint64(bitmaps[index].GetCardinality()) {
				t.Fatalf("round %d set %d: Cardinality = %d, sroar = %d",
					round, index, views[index].Cardinality(), bitmaps[index].GetCardinality())
			}
			want := bitmaps[index].ToArray()
			if got := collect(views[index]); !slices.Equal(got, want) {
				t.Fatalf("round %d set %d: iteration mismatch", round, index)
			}
		}

		queries := sampleQueries(rng, sets)
		for _, id := range queries {
			for index := range views {
				if views[index].Contains(id) != bitmaps[index].Contains(id) {
					t.Fatalf("round %d: Contains(%d) mismatch on set %d", round, id, index)
				}
			}
		}

		for left := 0; left < len(sets); left++ {
			for right := left + 1; right < len(sets); right++ {
				checkAnd(t, round, left, right, views[left], views[right], bitmaps[left], bitmaps[right])
				checkOr(t, round, left, right, views[left], views[right], bitmaps[left], bitmaps[right])
				checkAndNot(t, round, left, right, views[left], views[right], bitmaps[left], bitmaps[right])
				if Intersects(views[left], views[right]) != bitmaps[left].Intersects(bitmaps[right]) {
					t.Fatalf("round %d: Intersects(%d,%d) mismatch", round, left, right)
				}
			}
		}

		checkAndMany(t, round, views, bitmaps)
	}
}

func checkAnd(t *testing.T, round, left, right int, a, b Bitmap, sa, sb *sroar.Bitmap) {
	t.Helper()
	// sroar's And/Or/AndNot mutate their receiver, so clone before combining.
	wantCard := uint64(sa.Clone().And(sb).GetCardinality())
	if got := AndCardinality(a, b); got != wantCard {
		t.Fatalf("round %d: AndCardinality(%d,%d) = %d, sroar = %d", round, left, right, got, wantCard)
	}
	builder := NewBuilder(0)
	AndInto(builder, a, b)
	got := collect(OpenTrusted(builder.Compact()))
	want := sa.Clone().And(sb).ToArray()
	if !slices.Equal(got, want) {
		t.Fatalf("round %d: AndInto(%d,%d) mismatch", round, left, right)
	}
}

func checkOr(t *testing.T, round, left, right int, a, b Bitmap, sa, sb *sroar.Bitmap) {
	t.Helper()
	wantCard := uint64(sa.Clone().Or(sb).GetCardinality())
	if got := OrCardinality(a, b); got != wantCard {
		t.Fatalf("round %d: OrCardinality(%d,%d) = %d, sroar = %d", round, left, right, got, wantCard)
	}
	builder := NewBuilder(0)
	OrInto(builder, a, b)
	got := collect(OpenTrusted(builder.Compact()))
	want := sa.Clone().Or(sb).ToArray()
	if !slices.Equal(got, want) {
		t.Fatalf("round %d: OrInto(%d,%d) mismatch", round, left, right)
	}
}

func checkAndNot(t *testing.T, round, left, right int, a, b Bitmap, sa, sb *sroar.Bitmap) {
	t.Helper()
	wantCard := uint64(sa.Clone().AndNot(sb).GetCardinality())
	if got := AndNotCardinality(a, b); got != wantCard {
		t.Fatalf("round %d: AndNotCardinality(%d,%d) = %d, sroar = %d", round, left, right, got, wantCard)
	}
	builder := NewBuilder(0)
	AndNotInto(builder, a, b)
	got := collect(OpenTrusted(builder.Compact()))
	want := sa.Clone().AndNot(sb).ToArray()
	if !slices.Equal(got, want) {
		t.Fatalf("round %d: AndNotInto(%d,%d) mismatch", round, left, right)
	}
}

func checkAndMany(t *testing.T, round int, views []Bitmap, bitmaps []*sroar.Bitmap) {
	t.Helper()
	// sroar.FastAnd consumes its first argument; clone before combining.
	clones := make([]*sroar.Bitmap, len(bitmaps))
	for index, bitmap := range bitmaps {
		clones[index] = bitmap.Clone()
	}
	combined := sroar.FastAnd(clones...)
	wantCard := uint64(combined.GetCardinality())
	if got := AndManyCardinality(views...); got != wantCard {
		t.Fatalf("round %d: AndManyCardinality = %d, sroar = %d", round, got, wantCard)
	}
	builder := NewBuilder(0)
	AndManyInto(builder, views...)
	got := collect(OpenTrusted(builder.Compact()))
	want := combined.ToArray()
	if !slices.Equal(got, want) {
		t.Fatalf("round %d: AndManyInto mismatch", round)
	}
}

func sampleQueries(rng *rand.Rand, sets [][]uint64) []uint64 {
	queries := make([]uint64, 0, 512)
	for index, ids := range sets {
		for _, id := range ids {
			if index == 0 {
				queries = append(queries, id)
			}
			if rng.IntN(64) == 0 {
				queries = append(queries, id)
			}
		}
	}
	for range 256 {
		queries = append(queries, rng.Uint64())
	}
	return queries
}

func collect(view Bitmap) []uint64 {
	ids := make([]uint64, 0, view.Cardinality())
	for iterator := view.Iterator(); iterator.Next(); {
		ids = append(ids, iterator.Value())
	}
	return ids
}
