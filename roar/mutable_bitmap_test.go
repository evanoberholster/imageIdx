package roar

import (
	"math/rand/v2"
	"slices"
	"testing"
)

func TestMutableBitmapSetOps(t *testing.T) {
	a, _ := FromSorted([]uint64{1, 2, 3})
	b, _ := FromSorted([]uint64{2, 3, 4})

	or := NewMutableBitmapFrom(a)
	or.Or(b)
	if got, want := or.Freeze().AppendTo(nil), []uint64{1, 2, 3, 4}; !slices.Equal(got, want) {
		t.Fatalf("Or = %v, want %v", got, want)
	}

	and := NewMutableBitmapFrom(a)
	and.And(b)
	if got, want := and.Freeze().AppendTo(nil), []uint64{2, 3}; !slices.Equal(got, want) {
		t.Fatalf("And = %v, want %v", got, want)
	}

	andNot := NewMutableBitmapFrom(a)
	andNot.AndNot(b)
	if got, want := andNot.Freeze().AppendTo(nil), []uint64{1}; !slices.Equal(got, want) {
		t.Fatalf("AndNot = %v, want %v", got, want)
	}
}

func TestMutableBitmapMatchesReference(t *testing.T) {
	rng := rand.New(rand.NewPCG(11, 13))
	for round := 0; round < 30; round++ {
		pool := keyPool(rng, 12)
		sets := [][]uint64{randomSetOver(rng, pool), randomSetOver(rng, pool), randomSetOver(rng, pool)}
		bitmaps := make([]Bitmap, len(sets))
		for i := range sets {
			bitmaps[i], _ = FromSorted(sets[i])
		}

		mutable := NewMutableBitmapFrom(bitmaps[0])
		mutable.Or(bitmaps[1])
		mutable.AndNot(bitmaps[2])
		got := mutable.Freeze().AppendTo(nil)

		want := AndNot(Or(bitmaps[0], bitmaps[1]), bitmaps[2]).AppendTo(nil)
		if !slices.Equal(got, want) {
			t.Fatalf("round %d: mutable bitmap mismatch", round)
		}
	}
}

func TestMutableBitmapFromSortedList(t *testing.T) {
	mutable := NewMutableBitmap()
	if err := mutable.FromSortedList([]uint64{1, 2, 3}); err != nil {
		t.Fatal(err)
	}
	if got, want := mutable.Freeze().AppendTo(nil), []uint64{1, 2, 3}; !slices.Equal(got, want) {
		t.Fatalf("FromSortedList = %v, want %v", got, want)
	}
	// FromSortedList replaces content rather than unioning.
	if err := mutable.FromSortedList([]uint64{4, 5}); err != nil {
		t.Fatal(err)
	}
	if got, want := mutable.Freeze().AppendTo(nil), []uint64{4, 5}; !slices.Equal(got, want) {
		t.Fatalf("FromSortedList replace = %v, want %v", got, want)
	}
	if err := mutable.FromSortedList([]uint64{0}); err != ErrInvalidImageID {
		t.Fatalf("zero ID error = %v, want %v", err, ErrInvalidImageID)
	}
}

func TestMutableBitmapFreezeIsZeroCopy(t *testing.T) {
	mutable := NewMutableBitmap()
	if err := mutable.FromSortedList([]uint64{1, 2, 3}); err != nil {
		t.Fatal(err)
	}
	allocs := testing.AllocsPerRun(100, func() {
		_ = mutable.Freeze()
	})
	if allocs != 0 {
		t.Fatalf("Freeze allocated %v times, want 0", allocs)
	}
}

func TestMutableBitmapSeedIsShared(t *testing.T) {
	base, _ := FromSorted([]uint64{1, 2, 3})
	mutable := NewMutableBitmapFrom(base)
	if &mutable.Freeze().Bytes()[0] != &base.Bytes()[0] {
		t.Fatal("seed was copied instead of shared")
	}
}
