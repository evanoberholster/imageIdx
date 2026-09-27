package ranking

import (
	"errors"
	"math"
	"testing"

	searchmetrics "github.com/evanoberholster/imageIdx/search/metrics"
)

func TestWithMetricsRejectNil(t *testing.T) {
	var f RRF
	s := &Signals{ImageIDs: []ImageID{1}, Count: 1}
	s.Values[0] = []Rank{10}
	dst := make([]Rank, 1)
	if err := RankTimeSignalWithMetrics(dst, s.ImageIDs, 0, nil, nil); !errors.Is(err, ErrInvalidMetrics) {
		t.Fatal(err)
	}
	if err := EncodeDistancesWithMetrics(dst, []uint32{1}, nil); !errors.Is(err, ErrInvalidMetrics) {
		t.Fatal(err)
	}
	if err := QuantizeCosinesWithMetrics(dst, []float32{0}, nil); !errors.Is(err, ErrInvalidMetrics) {
		t.Fatal(err)
	}
	if err := f.FuseWithMetrics(dst, s, nil); !errors.Is(err, ErrInvalidMetrics) {
		t.Fatal(err)
	}
	if err := SortWithMetrics(s.ImageIDs, dst, nil); !errors.Is(err, ErrInvalidMetrics) {
		t.Fatal(err)
	}
	if _, _, err := TopKWithMetrics(nil, nil, s.ImageIDs, dst, 1, nil); !errors.Is(err, ErrInvalidMetrics) {
		t.Fatal(err)
	}
}

func TestEncodeMetricsCounts(t *testing.T) {
	var m searchmetrics.RankMetrics
	ids := []ImageID{1, 2, 3}
	dst := make([]Rank, 3)
	lookup := func(id ImageID) (uint64, bool) {
		if id == 2 {
			return 0, false
		}
		return 1000, true
	}
	if err := RankTimeSignalWithMetrics(dst, ids, 1000, lookup, &m); err != nil {
		t.Fatal(err)
	}
	if m.Candidates != 3 || m.Scored != 2 || m.Skipped != 1 {
		t.Fatalf("signal metrics=%+v", m)
	}
	if m.ScoreDuration <= 0 {
		t.Fatal("signal should record ScoreDuration")
	}

	m = searchmetrics.RankMetrics{}
	if err := EncodeDistancesWithMetrics(dst, []uint32{0, 5, 9}, &m); err != nil {
		t.Fatal(err)
	}
	if m.Candidates != 3 || m.Scored != 3 || m.Skipped != 0 {
		t.Fatalf("encode metrics=%+v", m)
	}

	m = searchmetrics.RankMetrics{}
	nan := float32(math.NaN())
	if err := QuantizeCosinesWithMetrics(dst, []float32{1, nan, -1}, &m); err != nil {
		t.Fatal(err)
	}
	if m.Candidates != 3 || m.Scored != 2 || m.Skipped != 1 || dst[1] != NoRank {
		t.Fatalf("cosine metrics=%+v dst=%v", m, dst)
	}
}

func TestFuseWithMetricsCounts(t *testing.T) {
	s := &Signals{ImageIDs: []ImageID{1, 2, 3}, Count: 2}
	s.Values[0] = []Rank{300, NoRank, 100}
	s.Values[1] = []Rank{50, 60, 70}
	var m searchmetrics.RankMetrics
	var f RRF
	dst := make([]Rank, 3)
	if err := f.FuseWithMetrics(dst, s, &m); err != nil {
		t.Fatal(err)
	}
	plain := make([]Rank, 3)
	var g RRF
	if err := g.Fuse(plain, s); err != nil {
		t.Fatal(err)
	}
	for i := range dst {
		if dst[i] != plain[i] {
			t.Fatalf("measured fuse differs: %v vs %v", dst, plain)
		}
	}
	if m.Candidates != 3 || m.Scored != 3 || m.Skipped != 0 {
		t.Fatalf("fuse metrics=%+v", m)
	}
	if m.K != 0 {
		t.Fatalf("fuse must not overwrite K, got %d", m.K)
	}
	if m.ScoreDuration <= 0 {
		t.Fatal("fuse should record ScoreDuration")
	}
}

func TestSortTopKMetrics(t *testing.T) {
	ids := []ImageID{3, 1, 2}
	ranks := []Rank{10, 30, 20}
	var m searchmetrics.RankMetrics
	if err := SortWithMetrics(ids, ranks, &m); err != nil {
		t.Fatal(err)
	}
	if !equalIDs(ids, []ImageID{1, 2, 3}) || m.SortDuration <= 0 {
		t.Fatalf("sort ids=%v metrics=%+v", ids, m)
	}
	if err := SortWithMetrics(ids[:2], ranks, &m); !errors.Is(err, ErrLengthMismatch) {
		t.Fatalf("mismatch should fail, got %v", err)
	}

	srcIDs := []ImageID{1, 2, 3, 4}
	srcRanks := []Rank{10, 50, 30, 40}
	m = searchmetrics.RankMetrics{}
	gotIDs, gotRanks, err := TopKWithMetrics(nil, nil, srcIDs, srcRanks, 2, &m)
	if err != nil {
		t.Fatal(err)
	}
	if !equalIDs(gotIDs, []ImageID{2, 4}) || len(gotRanks) != 2 {
		t.Fatalf("topk ids=%v ranks=%v", gotIDs, gotRanks)
	}
	if m.K != 2 || m.Candidates != 4 || m.Scored != 2 || m.Skipped != 2 {
		t.Fatalf("topk metrics=%+v", m)
	}
	if m.SortDuration <= 0 {
		t.Fatal("topk should record SortDuration")
	}
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

func TestMetricsPipelineReuse(t *testing.T) {
	ids := []ImageID{1, 2, 3, 4}
	var m searchmetrics.RankMetrics
	timeRanks := make([]Rank, len(ids))
	vecRanks := make([]Rank, len(ids))
	lookup := func(id ImageID) (uint64, bool) { return uint64(id * 10), true }
	if err := RankTimeSignalWithMetrics(timeRanks, ids, 20, lookup, &m); err != nil {
		t.Fatal(err)
	}
	if err := QuantizeCosinesWithMetrics(vecRanks, []float32{0.9, 0.1, -0.5, 0.2}, &m); err != nil {
		t.Fatal(err)
	}
	s := &Signals{ImageIDs: ids, Count: 2}
	s.Values[0], s.Values[1] = timeRanks, vecRanks
	fused := make([]Rank, len(ids))
	var f RRF
	if err := f.FuseWithMetrics(fused, s, &m); err != nil {
		t.Fatal(err)
	}
	if _, _, err := TopKWithMetrics(nil, nil, ids, fused, 2, &m); err != nil {
		t.Fatal(err)
	}
	// 4 signal + 4 cosine + 4 fuse candidates, topk adds 4 more.
	if m.Candidates != 16 || m.Scored == 0 {
		t.Fatalf("pipeline metrics=%+v", m)
	}
	if m.ScoreDuration <= 0 || m.SortDuration <= 0 {
		t.Fatalf("pipeline durations=%+v", m)
	}
}

func TestMeasuredPathsNoAlloc(t *testing.T) {
	n := 1000
	ids := make([]ImageID, n)
	dist := make([]uint32, n)
	sims := make([]float32, n)
	for i := range ids {
		ids[i] = ImageID(i + 1)
		dist[i] = uint32(i)
		sims[i] = float32(i)/float32(n)*2 - 1
	}
	dst := make([]Rank, n)
	var m searchmetrics.RankMetrics
	if a := testing.AllocsPerRun(20, func() {
		m = searchmetrics.RankMetrics{}
		_ = EncodeDistancesWithMetrics(dst, dist, &m)
	}); a != 0 {
		t.Fatalf("EncodeDistancesWithMetrics allocs=%v", a)
	}
	if a := testing.AllocsPerRun(20, func() {
		m = searchmetrics.RankMetrics{}
		_ = QuantizeCosinesWithMetrics(dst, sims, &m)
	}); a != 0 {
		t.Fatalf("QuantizeCosinesWithMetrics allocs=%v", a)
	}
	s := &Signals{ImageIDs: ids, Count: 2}
	s.Values[0] = make([]Rank, n)
	s.Values[1] = make([]Rank, n)
	EncodeDistances(s.Values[0], dist)
	QuantizeCosines(s.Values[1], sims)
	fused := make([]Rank, n)
	var f RRF
	_ = f.Fuse(fused, s)
	if a := testing.AllocsPerRun(10, func() {
		m = searchmetrics.RankMetrics{}
		_ = f.FuseWithMetrics(fused, s, &m)
	}); a != 0 {
		t.Fatalf("FuseWithMetrics allocs=%v", a)
	}
	cids := make([]ImageID, n)
	cranks := make([]Rank, n)
	if a := testing.AllocsPerRun(10, func() {
		m = searchmetrics.RankMetrics{}
		copy(cids, ids)
		copy(cranks, fused)
		_ = SortWithMetrics(cids, cranks, &m)
	}); a != 0 {
		t.Fatalf("SortWithMetrics allocs=%v", a)
	}
	outIDs := make([]ImageID, 0, 10)
	outRanks := make([]Rank, 0, 10)
	if a := testing.AllocsPerRun(10, func() {
		m = searchmetrics.RankMetrics{}
		_, _, _ = TopKWithMetrics(outIDs[:0], outRanks[:0], ids, fused, 10, &m)
	}); a != 0 {
		t.Fatalf("TopKWithMetrics allocs=%v", a)
	}
}
