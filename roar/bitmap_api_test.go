package roar

import (
	"slices"
	"testing"
)

func TestFromSorted(t *testing.T) {
	ids := []uint64{1, 1 << 48, 1<<48 + 7, ^uint64(0)}
	bitmap, err := FromSorted(ids)
	if err != nil {
		t.Fatal(err)
	}

	if got := bitmap.AppendTo(nil); !slices.Equal(got, ids) {
		t.Fatalf("AppendTo = %v, want %v", got, ids)
	}
	if bitmap.Cardinality() != uint64(len(ids)) {
		t.Fatalf("Cardinality = %d, want %d", bitmap.Cardinality(), len(ids))
	}
	for _, id := range ids {
		if !bitmap.Contains(id) {
			t.Errorf("Contains(%d) = false", id)
		}
	}
}

func TestFromSortedEmpty(t *testing.T) {
	bitmap, err := FromSorted(nil)
	if err != nil {
		t.Fatal(err)
	}
	if !bitmap.IsEmpty() || bitmap.Cardinality() != 0 {
		t.Fatal("empty input produced a non-empty Bitmap")
	}
	if _, err := Open(bitmap.data); err != nil {
		t.Fatalf("FromSorted returned invalid encoding: %v", err)
	}
}

func TestFromSortedErrors(t *testing.T) {
	tests := []struct {
		name string
		ids  []uint64
		want error
	}{
		{name: "zero", ids: []uint64{0}, want: ErrInvalidImageID},
		{name: "duplicate", ids: []uint64{1, 1}, want: ErrUnsortedIDs},
		{name: "unsorted", ids: []uint64{2, 1}, want: ErrUnsortedIDs},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			bitmap, err := FromSorted(test.ids)
			if err != test.want {
				t.Fatalf("error = %v, want %v", err, test.want)
			}
			if !bitmap.IsEmpty() {
				t.Fatal("bitmap returned with error is not empty")
			}
		})
	}
}

func TestBitmapZeroValue(t *testing.T) {
	var bitmap Bitmap
	if !bitmap.IsEmpty() {
		t.Fatal("zero Bitmap is not empty")
	}
	if bitmap.Cardinality() != 0 || bitmap.KeyCount() != 0 {
		t.Fatal("zero Bitmap has content")
	}
	if bitmap.Contains(1) {
		t.Fatal("zero Bitmap contains an ID")
	}
	if iterator := bitmap.Iterator(); iterator.Next() {
		t.Fatal("zero Bitmap iterator produced an ID")
	}
	prefix := []uint64{99}
	if got := bitmap.AppendTo(prefix); !slices.Equal(got, prefix) {
		t.Fatalf("AppendTo changed destination: %v", got)
	}

	other, err := FromSorted([]uint64{1, 1 << 16})
	if err != nil {
		t.Fatal(err)
	}
	if Intersects(bitmap, other) || AndCardinality(bitmap, other) != 0 {
		t.Fatal("zero Bitmap intersects a populated bitmap")
	}
	if got := OrCardinality(bitmap, other); got != other.Cardinality() {
		t.Fatalf("OrCardinality = %d, want %d", got, other.Cardinality())
	}
	if got := AndNotCardinality(other, bitmap); got != other.Cardinality() {
		t.Fatalf("AndNotCardinality = %d, want %d", got, other.Cardinality())
	}
}

func TestEmptyOperandsShortCircuit(t *testing.T) {
	empty := Bitmap{}
	other, err := FromSorted([]uint64{1, 2, 3})
	if err != nil {
		t.Fatal(err)
	}
	if got := And(empty, other); !got.IsEmpty() {
		t.Fatal("And(empty, x) is not empty")
	}
	if got := And(other, empty); !got.IsEmpty() {
		t.Fatal("And(x, empty) is not empty")
	}
	if got := Or(empty, other); !slices.Equal(got.AppendTo(nil), []uint64{1, 2, 3}) {
		t.Fatalf("Or(empty, x) = %v", got.AppendTo(nil))
	}
	if got := Or(other, empty); !slices.Equal(got.AppendTo(nil), []uint64{1, 2, 3}) {
		t.Fatalf("Or(x, empty) = %v", got.AppendTo(nil))
	}
	if got := AndNot(empty, other); !got.IsEmpty() {
		t.Fatal("AndNot(empty, x) is not empty")
	}
	if got := AndNot(other, empty); !slices.Equal(got.AppendTo(nil), []uint64{1, 2, 3}) {
		t.Fatalf("AndNot(x, empty) = %v", got.AppendTo(nil))
	}
	if got := AndMany(other, empty); !got.IsEmpty() {
		t.Fatal("AndMany with empty is not empty")
	}

	m := NewDeltaBuilderFrom(other)
	m.And(empty)
	if !m.Freeze().IsEmpty() {
		t.Fatal("DeltaBuilder.And(empty) is not empty")
	}
	m = NewDeltaBuilderFrom(other)
	m.Or(empty)
	if got := m.Freeze().AppendTo(nil); !slices.Equal(got, []uint64{1, 2, 3}) {
		t.Fatalf("DeltaBuilder.Or(empty) = %v", got)
	}
}

func TestAndNotDisjointReturnsReceiver(t *testing.T) {
	a, err := FromSorted([]uint64{1, 2, 3})
	if err != nil {
		t.Fatal(err)
	}
	b, err := FromSorted([]uint64{1<<16 + 1, 1<<16 + 2})
	if err != nil {
		t.Fatal(err)
	}
	got := AndNot(a, b)
	if !slices.Equal(got.AppendTo(nil), []uint64{1, 2, 3}) {
		t.Fatalf("AndNot(disjoint) = %v", got.AppendTo(nil))
	}
	if len(got.Bytes()) == 0 || &got.Bytes()[0] != &a.Bytes()[0] {
		t.Fatal("AndNot(disjoint) copied instead of returning the receiver")
	}
}
