package roar

import (
	"math/rand/v2"
	"slices"
	"testing"

	"github.com/weaviate/sroar"
)

func TestMarshalBinaryRoundTrip(t *testing.T) {
	for _, ids := range [][]uint64{
		nil,
		{1 << 48},
		{1, 1<<16 + 1, 3 << 48, ^uint64(0)},
		denseSortedIDs(rngForTest(), 2),
	} {
		bitmap, err := FromSorted(ids)
		if err != nil {
			t.Fatal(err)
		}
		data, err := bitmap.MarshalBinary()
		if err != nil {
			t.Fatal(err)
		}
		if bitmap.LenInBytes() != len(data) && len(ids) != 0 {
			t.Fatalf("LenInBytes = %d, MarshalBinary = %d", bitmap.LenInBytes(), len(data))
		}
		reopened, err := Open(data)
		if err != nil {
			t.Fatalf("Open(MarshalBinary) = %v", err)
		}
		if reopened.Cardinality() != bitmap.Cardinality() {
			t.Fatalf("round-trip cardinality = %d, want %d", reopened.Cardinality(), bitmap.Cardinality())
		}
		if got, want := reopened.AppendTo(nil), bitmap.AppendTo(nil); !slices.Equal(got, want) {
			t.Fatalf("round-trip content = %v, want %v", got, want)
		}
	}
}

func TestCloneIsIndependent(t *testing.T) {
	bitmap, err := FromSorted([]uint64{1, 2, 3, 1 << 48})
	if err != nil {
		t.Fatal(err)
	}
	clone := bitmap.Clone()
	if !slices.Equal(clone.AppendTo(nil), bitmap.AppendTo(nil)) {
		t.Fatal("clone content differs")
	}
	if !slices.Equal(clone.data, bitmap.data) || &clone.data[0] == &bitmap.data[0] {
		t.Fatal("clone shares backing storage")
	}

	var empty Bitmap
	if c := empty.Clone(); !c.IsEmpty() {
		t.Fatal("clone of empty is not empty")
	}
	if data, _ := empty.MarshalBinary(); len(data) != HeaderSize {
		t.Fatalf("empty MarshalBinary = %d bytes", len(data))
	}
}

func TestUnmarshalBinary(t *testing.T) {
	original, _ := FromSorted([]uint64{5, 1 << 16})
	data, _ := original.MarshalBinary()
	var decoded Bitmap
	if err := decoded.UnmarshalBinary(data); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(decoded.AppendTo(nil), []uint64{5, 1 << 16}) {
		t.Fatalf("decoded = %v", decoded.AppendTo(nil))
	}
	if err := decoded.UnmarshalBinary([]byte{0xde, 0xad}); err == nil {
		t.Fatal("corrupt data accepted")
	}
}

func TestRankSelectMinMaxAgainstSroar(t *testing.T) {
	rng := rand.New(rand.NewPCG(2024, 7))
	pool := keyPool(rng, 32)
	ids := randomSetOver(rng, pool)
	ids = append(ids, denseSortedIDs(rng, 3)...)
	sortIDs(ids)

	bitmap, err := FromSorted(ids)
	if err != nil {
		t.Fatal(err)
	}
	reference := sroar.FromSortedList(ids)

	// roar's Rank is a prefix count (number of IDs <= x), unlike sroar's
	// member-rank which returns -1 for absent values. Verify against a
	// brute-force count over the sorted slice.
	for _, probe := range []uint64{0, 1, 1 << 16, ids[len(ids)-1] / 2, ids[len(ids)-1], ids[len(ids)-1] + 1<<16, ^uint64(0)} {
		want := uint64(countLE(ids, probe))
		if got := bitmap.Rank(probe); got != want {
			t.Fatalf("Rank(%d) = %d, want %d", probe, got, want)
		}
	}

	cardinality := int(bitmap.Cardinality())
	for index := 0; index < cardinality; index++ {
		got, err := bitmap.Select(uint64(index))
		if err != nil {
			t.Fatalf("Select(%d): %v", index, err)
		}
		want, _ := reference.Select(uint64(index))
		if got != want {
			t.Fatalf("Select(%d) = %d, want %d", index, got, want)
		}
	}
	if _, err := bitmap.Select(uint64(cardinality)); err != ErrSelectOutOfRange {
		t.Fatalf("Select(cardinality) error = %v", err)
	}

	if got, want := bitmap.Minimum(); got != ids[0] || !want {
		t.Fatalf("Minimum = %d, %v", got, want)
	}
	if got, want := bitmap.Maximum(); got != ids[len(ids)-1] || !want {
		t.Fatalf("Maximum = %d, %v", got, want)
	}

	empty, _ := FromSorted(nil)
	if _, ok := empty.Minimum(); ok {
		t.Fatal("empty Minimum reported ok")
	}
	if _, ok := empty.Maximum(); ok {
		t.Fatal("empty Maximum reported ok")
	}
}

func TestCursorContainsAndNextGeq(t *testing.T) {
	rng := rand.New(rand.NewPCG(99, 3))
	pool := keyPool(rng, 40)
	ids := randomSetOver(rng, pool)
	bitmap, _ := FromSorted(ids)

	cursor := bitmap.NewCursor()
	// NextGeq over the whole sorted list must reproduce the list exactly.
	probe := uint64(0)
	visited := make([]uint64, 0, len(ids))
	for {
		value, ok := cursor.NextGeq(probe)
		if !ok {
			break
		}
		visited = append(visited, value)
		probe = value + 1
	}
	if !slices.Equal(visited, ids) {
		t.Fatalf("NextGeq walk = %d elements, want %d", len(visited), len(ids))
	}

	// Contains matches for arbitrary (out-of-order) probes.
	reloaded := bitmap.NewCursor()
	queries := []uint64{0, 1, ids[len(ids)/2], maxKeyValue(ids), ^uint64(0)}
	for _, id := range queries {
		if reloaded.Contains(id) != bitmap.Contains(id) {
			t.Fatalf("Contains(%d) mismatch", id)
		}
	}

	// NextGeq lower-bound matches a plain scan for sampled probes.
	check := bitmap.NewCursor()
	for _, x := range []uint64{0, 1, 5, ids[len(ids)/3], ids[len(ids)-1]} {
		got, ok := check.NextGeq(x)
		want, wantOK := lowerBound(ids, x)
		if ok != wantOK || got != want {
			t.Fatalf("NextGeq(%d) = (%d,%v), want (%d,%v)", x, got, ok, want, wantOK)
		}
	}
}

func TestDirectSetOpsMatchInto(t *testing.T) {
	rng := rand.New(rand.NewPCG(5, 6))
	pool := keyPool(rng, 24)
	aIDs := randomSetOver(rng, pool)
	bIDs := randomSetOver(rng, pool)
	a, _ := FromSorted(aIDs)
	b, _ := FromSorted(bIDs)

	assertEq := func(name string, got Bitmap, want func() []uint64) {
		t.Helper()
		if !slices.Equal(got.AppendTo(nil), want()) {
			t.Fatalf("%s mismatch", name)
		}
	}
	assertEq("And", And(a, b), func() []uint64 {
		builder := NewBuilder(0)
		AndInto(builder, a, b)
		return OpenTrusted(builder.Compact()).AppendTo(nil)
	})
	assertEq("Or", Or(a, b), func() []uint64 {
		builder := NewBuilder(0)
		OrInto(builder, a, b)
		return OpenTrusted(builder.Compact()).AppendTo(nil)
	})
	assertEq("AndNot", AndNot(a, b), func() []uint64 {
		builder := NewBuilder(0)
		AndNotInto(builder, a, b)
		return OpenTrusted(builder.Compact()).AppendTo(nil)
	})
	assertEq("AndMany", AndMany(a, b, a), func() []uint64 {
		builder := NewBuilder(0)
		AndManyInto(builder, a, b, a)
		return OpenTrusted(builder.Compact()).AppendTo(nil)
	})
}

func TestConcurrentOpsMatchSequential(t *testing.T) {
	rng := rand.New(rand.NewPCG(8, 9))
	pool := keyPool(rng, 32)
	a, _ := FromSorted(randomSetOver(rng, pool))
	b, _ := FromSorted(randomSetOver(rng, pool))
	c, _ := FromSorted(randomSetOver(rng, pool))

	for _, concurrency := range []int{1, 2, 4, 8} {
		if got := AndConc(a, b, concurrency).AppendTo(nil); !slices.Equal(got, And(a, b).AppendTo(nil)) {
			t.Fatalf("AndConc(%d) mismatch", concurrency)
		}
		if got := OrConc(a, b, concurrency).AppendTo(nil); !slices.Equal(got, Or(a, b).AppendTo(nil)) {
			t.Fatalf("OrConc(%d) mismatch", concurrency)
		}
		if got := AndNotConc(a, b, concurrency).AppendTo(nil); !slices.Equal(got, AndNot(a, b).AppendTo(nil)) {
			t.Fatalf("AndNotConc(%d) mismatch", concurrency)
		}
		if got := FastParOr(concurrency, a, b, c).AppendTo(nil); !slices.Equal(got, Or(Or(a, b), c).AppendTo(nil)) {
			t.Fatalf("FastParOr(%d) mismatch", concurrency)
		}
	}
	if got := FastParOr(1, a, b).AppendTo(nil); !slices.Equal(got, Or(a, b).AppendTo(nil)) {
		t.Fatal("FastParOr(1) mismatch")
	}
}

func TestNewRangeIteratorsCoverBitmap(t *testing.T) {
	rng := rand.New(rand.NewPCG(11, 12))
	pool := keyPool(rng, 64)
	ids := randomSetOver(rng, pool)
	bitmap, _ := FromSorted(ids)

	var combined []uint64
	for _, iterator := range bitmap.NewRangeIterators(8) {
		for iterator.Next() {
			combined = append(combined, iterator.Value())
		}
	}
	if !slices.Equal(combined, ids) {
		t.Fatalf("range iterators covered %d of %d IDs", len(combined), len(ids))
	}

	empty, _ := FromSorted(nil)
	if got := empty.NewRangeIterators(4); len(got) == 0 || got[0].Next() {
		t.Fatal("empty range iterators misbehaved")
	}
}

func maxKeyValue(ids []uint64) uint64 {
	if len(ids) == 0 {
		return 0
	}
	return ids[len(ids)-1] | 0xffff
}

func countLE(ids []uint64, x uint64) int {
	index, _ := slices.BinarySearch(ids, x)
	if index < len(ids) && ids[index] == x {
		return index + 1
	}
	return index
}

func lowerBound(ids []uint64, x uint64) (uint64, bool) {
	index, _ := slices.BinarySearch(ids, x)
	if index < len(ids) {
		return ids[index], true
	}
	return 0, false
}
