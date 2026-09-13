package roar

import (
	"slices"
	"testing"
)

func mustBitmap(t *testing.T, ids []uint64) Bitmap {
	t.Helper()
	bitmap, err := FromSorted(ids)
	if err != nil {
		t.Fatal(err)
	}
	return bitmap
}

func referenceUnion(bitmaps []Bitmap) []uint64 {
	var all []uint64
	for _, bm := range bitmaps {
		all = append(all, bm.AppendTo(nil)...)
	}
	slices.Sort(all)
	return slices.Compact(all)
}

func referenceIntersection(bitmaps []Bitmap) []uint64 {
	if len(bitmaps) == 0 {
		return nil
	}
	counts := make(map[uint64]int, bitmaps[0].Cardinality())
	for _, bm := range bitmaps {
		for _, id := range bm.AppendTo(nil) {
			counts[id]++
		}
	}
	var out []uint64
	for id, count := range counts {
		if count == len(bitmaps) {
			out = append(out, id)
		}
	}
	slices.Sort(out)
	return out
}

func TestOrManyMatchesSequential(t *testing.T) {
	rng := rngForTest()
	pool := keyPool(rng, 24)
	var views []Bitmap
	for range 5 {
		views = append(views, mustBitmap(t, randomSetOver(rng, pool)))
	}
	views = append(views, mustBitmap(t, denseSortedIDs(rng, 2)))

	folded := views[0]
	for _, bm := range views[1:] {
		folded = Or(folded, bm)
	}
	if got, want := OrMany(views...).AppendTo(nil), folded.AppendTo(nil); !slices.Equal(got, want) {
		t.Fatal("OrMany differs from sequential Or fold")
	}
	if got, want := OrMany(views...).AppendTo(nil), referenceUnion(views); !slices.Equal(got, want) {
		t.Fatal("OrMany differs from sorted-union reference")
	}

	builder := NewBuilder(0)
	OrManyInto(builder, views...)
	if got, want := OpenTrusted(builder.Compact()).AppendTo(nil), folded.AppendTo(nil); !slices.Equal(got, want) {
		t.Fatal("OrManyInto differs from sequential Or fold")
	}
}

func TestOrManyEmptyAndSingle(t *testing.T) {
	other := mustBitmap(t, []uint64{1, 2, 3})
	if got := OrMany(); !got.IsEmpty() {
		t.Fatal("OrMany() is not empty")
	}
	if got := OrMany(Bitmap{}, Bitmap{}); !got.IsEmpty() {
		t.Fatal("OrMany(empty, empty) is not empty")
	}
	if got := OrMany(Bitmap{}, other); !slices.Equal(got.AppendTo(nil), []uint64{1, 2, 3}) {
		t.Fatalf("OrMany(empty, x) = %v", got.AppendTo(nil))
	}
	if got := OrMany(other); !slices.Equal(got.AppendTo(nil), []uint64{1, 2, 3}) {
		t.Fatalf("OrMany(x) = %v", got.AppendTo(nil))
	}
	builder := NewBuilder(0)
	OrManyInto(builder)
	if builder.KeyCount() != 0 {
		t.Fatal("OrManyInto() produced containers")
	}
}

func TestOrManyWideFanIn(t *testing.T) {
	rng := rngForTest()
	pool := keyPool(rng, 8)
	var views []Bitmap
	for range 10 {
		views = append(views, mustBitmap(t, randomSetOver(rng, pool)))
	}
	if got, want := OrMany(views...).AppendTo(nil), referenceUnion(views); !slices.Equal(got, want) {
		t.Fatal("OrMany with 10 views differs from reference")
	}
}

func TestAndManyMatchesPairwise(t *testing.T) {
	rng := rngForTest()
	pool := keyPool(rng, 24)
	a := mustBitmap(t, randomSetOver(rng, pool))
	b := mustBitmap(t, randomSetOver(rng, pool))
	if got, want := AndMany(a, b).AppendTo(nil), And(a, b).AppendTo(nil); !slices.Equal(got, want) {
		t.Fatal("AndMany(a, b) differs from And(a, b)")
	}

	var six []Bitmap
	for range 6 {
		six = append(six, mustBitmap(t, randomSetOver(rng, pool)))
	}
	if got, want := AndMany(six...).AppendTo(nil), referenceIntersection(six); !slices.Equal(got, want) {
		t.Fatal("AndMany with 6 views differs from reference")
	}
}

func TestCursorResetAndFilterSorted(t *testing.T) {
	a := mustBitmap(t, []uint64{1, 2, 3, 1 << 16, 2 << 16})
	b := mustBitmap(t, []uint64{2, 3, 4, 1 << 16, 3 << 16})

	cursor := a.NewCursor()
	for _, id := range []uint64{1, 2, 4, 1 << 16, 3 << 16} {
		if cursor.Contains(id) != a.Contains(id) {
			t.Fatalf("pre-reset Contains(%d) mismatch", id)
		}
	}
	cursor.Reset(b)
	for _, id := range []uint64{1, 2, 3, 4, 1 << 16, 2 << 16, 3 << 16} {
		if cursor.Contains(id) != b.Contains(id) {
			t.Fatalf("post-reset Contains(%d) mismatch", id)
		}
	}
	cursor.Reset(Bitmap{})
	if cursor.Contains(1) {
		t.Fatal("reset to empty still contains IDs")
	}

	candidates := []uint64{1, 2, 3, 4, 5, 1 << 16, 2 << 16, 3 << 16}
	cursor.Reset(b)
	if got, want := b.FilterSorted(nil, candidates), cursor.FilterSorted(nil, candidates); !slices.Equal(got, want) {
		t.Fatalf("Cursor.FilterSorted = %v, want %v", got, want)
	}
	// Reuse across bitmaps without reallocating the cursor value.
	cursor.Reset(a)
	if got, want := a.FilterSorted(nil, candidates), cursor.FilterSorted(nil, candidates); !slices.Equal(got, want) {
		t.Fatalf("reused Cursor.FilterSorted = %v, want %v", got, want)
	}
}

func TestCloneToBuf(t *testing.T) {
	bitmap := mustBitmap(t, []uint64{1, 2, 1 << 16, 3 << 48})
	reuse := make([]byte, 0, bitmap.LenInBytes())
	clone := bitmap.CloneToBuf(reuse)
	if !slices.Equal(clone.AppendTo(nil), bitmap.AppendTo(nil)) {
		t.Fatal("CloneToBuf content differs")
	}
	if len(reuse) != 0 && &clone.Bytes()[0] != &reuse[:cap(reuse)][0] {
		t.Fatal("CloneToBuf did not reuse the backing array")
	}
	// Independence: mutating the clone must not affect the original bytes.
	original := slices.Clone(bitmap.Bytes())
	clone.Bytes()[len(clone.Bytes())-1] ^= 0xff
	if slices.Equal(bitmap.Bytes(), clone.Bytes()) || !slices.Equal(bitmap.Bytes(), original) {
		t.Fatal("CloneToBuf shares storage with the original")
	}

	small := make([]byte, 0, 8)
	fallback := bitmap.CloneToBuf(small)
	if !slices.Equal(fallback.AppendTo(nil), mustBitmap(t, []uint64{1, 2, 1 << 16, 3 << 48}).AppendTo(nil)) {
		t.Fatal("CloneToBuf fallback content differs")
	}

	var empty Bitmap
	if got := empty.CloneToBuf(make([]byte, 0, 64)); !got.IsEmpty() {
		t.Fatal("CloneToBuf of empty is not empty")
	}
}

func TestMustFromSorted(t *testing.T) {
	bitmap := MustFromSorted([]uint64{1, 2, 3})
	if !slices.Equal(bitmap.AppendTo(nil), []uint64{1, 2, 3}) {
		t.Fatalf("MustFromSorted = %v", bitmap.AppendTo(nil))
	}
	defer func() {
		if recover() == nil {
			t.Fatal("MustFromSorted did not panic on invalid input")
		}
	}()
	_ = MustFromSorted([]uint64{2, 1})
}

func TestBuilderReservePreserves(t *testing.T) {
	builder := NewBuilder(1)
	builder.appendArray(7<<16, []uint16{1, 2, 3})
	keysBefore := slices.Clone(builder.keys)
	dataBefore := slices.Clone(builder.data)
	builder.reserveKeys(64)
	builder.reserveData(4096)
	if !slices.Equal(builder.keys, keysBefore) || !slices.Equal(builder.data, dataBefore) {
		t.Fatal("reserve discarded accumulated containers")
	}
	if cap(builder.keys) < len(keysBefore)+64 || cap(builder.data) < len(dataBefore)+4096 {
		t.Fatal("reserve did not grow capacity")
	}
}

func TestRemoveRangeDenseWordBoundaries(t *testing.T) {
	dense := make([]uint64, 5000)
	for index := range dense {
		dense[index] = uint64(index + 1)
	}
	bitmap := MustFromSorted(dense)
	bruteForce := func(lo, hi uint64) []uint64 {
		var out []uint64
		for _, id := range dense {
			if id < lo || id >= hi {
				out = append(out, id)
			}
		}
		return out
	}
	for _, span := range [][2]uint64{
		{100, 200},   // spans words 1..3
		{63, 65},     // crosses a word boundary
		{64, 128},    // word-aligned span
		{1, 1 << 16}, // clears the whole dense container
		{4999, 5001}, // tail edge
	} {
		if got, want := bitmap.RemoveRange(span[0], span[1]).AppendTo(nil), bruteForce(span[0], span[1]); !slices.Equal(got, want) {
			t.Fatalf("RemoveRange(%d,%d) mismatch", span[0], span[1])
		}
	}
}
