package timeidx

import (
	"testing"

	common "github.com/evanoberholster/imageIdx/common"
)

func TestBaseRequiresDeterministicOrdering(t *testing.T) {
	if _, err := newBase(
		[]timemarker{1, 1},
		[]common.ValueID{2, 1},
	); err != ErrInvalidBase {
		t.Fatalf("error = %v, want %v", err, ErrInvalidBase)
	}
	base, err := newBase(
		[]timemarker{1, 1, 2},
		[]common.ValueID{1, 2, 1},
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
	delta := newDelta(4)
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
	if delta.len() != 0 || delta.remaining() != 4 || delta.hasState(7) {
		t.Fatal("reset delta retained published state")
	}
}

func TestCompactIsSortedAndAppliesLatestState(t *testing.T) {
	base, err := newBase(
		[]timemarker{1, 2, 3},
		[]common.ValueID{1, 2, 3},
	)
	if err != nil {
		t.Fatal(err)
	}
	delta := newDelta(8)
	delta.appendDelete(2)
	delta.appendAdd(5, 1)
	delta.appendAdd(4, 4)
	delta.appendAdd(6, 4)

	got := compact(base, delta)
	wantMarkers := []timemarker{3, 5, 6}
	wantIDs := []common.ValueID{3, 1, 4}
	if !equalMarkers(got.markers, wantMarkers) || !equalIDs(got.imageIDs, wantIDs) {
		t.Fatalf("compact = (%v, %v), want (%v, %v)", got.markers, got.imageIDs, wantMarkers, wantIDs)
	}
}

func equalMarkers(a, b []timemarker) bool {
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

func equalIDs(a, b []common.ValueID) bool {
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
