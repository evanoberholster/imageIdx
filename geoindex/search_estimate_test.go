package geoindex

import "testing"

func TestEstimateRangesMatchesPublishedCandidates(t *testing.T) {
	key, err := KeyFromLatLng(37, -122)
	if err != nil {
		t.Fatal(err)
	}
	index := testIndexFromCells(t, 1, map[S2Key][]ImageID{key: {1, 2, 3}})
	defer index.Close()

	ranges := []Range{{Start: key, End: key}}
	if estimate := index.EstimateRanges(ranges); estimate != uint64(len(index.SearchRanges(nil, ranges))) {
		t.Fatalf("EstimateRanges = %d", estimate)
	}
}
