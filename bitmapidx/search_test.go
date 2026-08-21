package bitmapidx

import (
	"slices"
	"testing"
)

func TestFilterIntoSupportsInPlaceCandidates(t *testing.T) {
	epoch := NewEpoch()
	first, err := New(BitmapKey{Type: IndexTypeTag, ValueID: 1}, 1, []uint64{1, 2, 3}, Config{Epoch: epoch})
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := New(BitmapKey{Type: IndexTypeTag, ValueID: 2}, 2, []uint64{2, 3, 4}, Config{Epoch: epoch})
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()

	candidates := []ImageID{1, 2, 3, 4}
	got := FilterInto(candidates[:0], candidates, first, second)
	if !slices.Equal(got, []ImageID{2, 3}) {
		t.Fatalf("FilterInto = %v", got)
	}
}
