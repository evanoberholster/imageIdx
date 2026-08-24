package bitmapidx

import (
	"slices"
	"testing"
)

func TestFilterCandidatesSupportsInPlaceCandidates(t *testing.T) {
	first, err := New(Spec{Key: Key{Type: IndexTypeTag, ValueID: 1}, IndexID: 1, IDs: []ImageID{1, 2, 3}}, Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := New(Spec{Key: Key{Type: IndexTypeTag, ValueID: 2}, IndexID: 2, IDs: []ImageID{2, 3, 4}}, Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()

	candidates := []ImageID{1, 2, 3, 4}
	got := FilterCandidates(candidates[:0], candidates, first, second)
	if !slices.Equal(got, []ImageID{2, 3}) {
		t.Fatalf("FilterCandidates = %v", got)
	}
}

func TestAppendToWithDeltasReusesDestination(t *testing.T) {
	index, err := New(Spec{
		Key: Key{Type: IndexTypeTag, ValueID: 1}, IndexID: 1, IDs: []ImageID{1, 2, 3},
	}, Config{ActiveTarget: 10, HardDeltaLimit: 20})
	if err != nil {
		t.Fatal(err)
	}
	defer index.Close()
	if err = index.Apply(Mutation{Add: []ImageID{4, 5}, Remove: []ImageID{2}}); err != nil {
		t.Fatal(err)
	}
	buffer := make([]ImageID, 0, 8)
	if got := index.AppendTo(buffer[:0]); !slices.Equal(got, []ImageID{1, 3, 4, 5}) {
		t.Fatalf("AppendTo = %v", got)
	}
	if allocations := testing.AllocsPerRun(100, func() {
		buffer = index.AppendTo(buffer[:0])
	}); allocations != 0 {
		t.Fatalf("AppendTo allocations = %v, want 0", allocations)
	}
}
