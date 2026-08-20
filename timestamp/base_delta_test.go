package timestamp

import "testing"

func TestBaseRequiresDeterministicOrdering(t *testing.T) {
	if _, err := NewTimestampBase(
		[]Timemarker{1, 1},
		[]ImageID{2, 1},
	); err != ErrInvalidBase {
		t.Fatalf("error = %v, want %v", err, ErrInvalidBase)
	}
	base, err := NewTimestampBase(
		[]Timemarker{1, 1, 2},
		[]ImageID{1, 2, 1},
	)
	if err != nil {
		t.Fatal(err)
	}
	lo, hi := base.rangeIndexes(1, 2)
	if lo != 0 || hi != 2 {
		t.Fatalf("range = [%d,%d), want [0,2)", lo, hi)
	}
}

func TestDeltaIsFixedCapacityAndTracksLatestState(t *testing.T) {
	delta := newTimestampDelta(4)
	if !delta.appendAdd(1, 7) || !delta.appendDelete(7) || !delta.appendAdd(3, 7) {
		t.Fatal("append unexpectedly failed")
	}
	if delta.latestAdd(7, 0) {
		t.Fatal("stale addition is marked latest")
	}
	if !delta.latestAdd(7, 1) {
		t.Fatal("latest addition was not found")
	}
	if !delta.appendDelete(9) {
		t.Fatal("fourth append unexpectedly failed")
	}
	if delta.appendAdd(4, 10) {
		t.Fatal("delta grew beyond fixed capacity")
	}
	delta.reset()
	if delta.Len() != 0 || delta.remaining() != 4 || delta.hasState(7) {
		t.Fatal("reset delta retained published state")
	}
}

func TestCompactIsSortedAndAppliesLatestState(t *testing.T) {
	base, err := NewTimestampBase(
		[]Timemarker{1, 2, 3},
		[]ImageID{1, 2, 3},
	)
	if err != nil {
		t.Fatal(err)
	}
	delta := newTimestampDelta(8)
	delta.appendDelete(2)
	delta.appendAdd(5, 1)
	delta.appendAdd(4, 4)
	delta.appendAdd(6, 4)

	got := compact(base, delta)
	wantMarkers := []Timemarker{3, 5, 6}
	wantIDs := []ImageID{3, 1, 4}
	if !equalMarkers(got.Markers, wantMarkers) || !equalIDs(got.ImageIDs, wantIDs) {
		t.Fatalf("compact = (%v, %v), want (%v, %v)", got.Markers, got.ImageIDs, wantMarkers, wantIDs)
	}
}

func equalMarkers(a, b []Timemarker) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func equalIDs(a, b []ImageID) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
