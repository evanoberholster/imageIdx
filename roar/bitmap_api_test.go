package roar

import (
	"slices"
	"testing"
)

func TestFromSortedList(t *testing.T) {
	ids := []uint64{1, 1 << 48, 1<<48 + 7, ^uint64(0)}
	bitmap, err := FromSortedList(ids)
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

	// View remains source-compatible with callers of the original API.
	var view View = bitmap
	if view.Cardinality() != bitmap.Cardinality() {
		t.Fatal("View alias changed bitmap behavior")
	}
}

func TestFromSortedListEmpty(t *testing.T) {
	bitmap, err := FromSortedList(nil)
	if err != nil {
		t.Fatal(err)
	}
	if !bitmap.IsEmpty() || bitmap.Cardinality() != 0 {
		t.Fatal("empty input produced a non-empty Bitmap")
	}
	if _, err := Open(bitmap.data); err != nil {
		t.Fatalf("FromSortedList returned invalid encoding: %v", err)
	}
}

func TestFromSortedListErrors(t *testing.T) {
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
			bitmap, err := FromSortedList(test.ids)
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

	other, err := FromSortedList([]uint64{1, 1 << 16})
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
