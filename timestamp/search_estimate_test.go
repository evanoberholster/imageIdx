package timestamp

import "testing"

func TestEstimateRangeBoundsCandidates(t *testing.T) {
	quantum := uint64(1) << TimestampShift
	index, err := New([]Record{
		{Timestamp: quantum, ImageID: 1},
		{Timestamp: 2 * quantum, ImageID: 2},
		{Timestamp: 3 * quantum, ImageID: 3},
	}, 0, Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer index.Close()

	candidates := index.Search(nil, 2*quantum, 4*quantum)
	if estimate := index.EstimateRange(2*quantum, 4*quantum); estimate < uint64(len(candidates)) {
		t.Fatalf("EstimateRange = %d, candidates = %d", estimate, len(candidates))
	}
}
