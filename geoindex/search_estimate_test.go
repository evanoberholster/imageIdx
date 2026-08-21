package geoindex

import "testing"

func TestEstimateRangesMatchesPublishedCandidates(t *testing.T) {
	key, err := KeyFromLatLng(37, -122)
	if err != nil {
		t.Fatal(err)
	}
	index, err := NewFromCells(1, []GeoCellEntry{
		{Key: key, ImageID: 1},
		{Key: key, ImageID: 2},
		{Key: key, ImageID: 3},
	}, Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer index.Close()

	ranges := []S2KeyRange{{Start: key, End: key}}
	if estimate := index.EstimateRanges(ranges); estimate != uint64(len(index.SearchRanges(nil, ranges))) {
		t.Fatalf("EstimateRanges = %d", estimate)
	}
}
