package ranking

import (
	"math"
	"testing"
)

func TestHigherIsBetter(t *testing.T) {
	if !(BestRank > MinRank && MinRank > NoRank) {
		t.Fatalf("ordering: best=%d min=%d none=%d", BestRank, MinRank, NoRank)
	}
	if EncodeDistance(0) != MaxRank {
		t.Fatalf("exact distance should be best, got %d", EncodeDistance(0))
	}
	if QuantizeCosine(1) != MaxRank || QuantizeCosine(-1) != MinRank {
		t.Fatalf("cosine boundaries wrong: +1=%d -1=%d", QuantizeCosine(1), QuantizeCosine(-1))
	}
	if QuantizeCosineDistance(0) != MaxRank || QuantizeCosineDistance(2) != MinRank {
		t.Fatalf("cosine-distance boundaries wrong: 0=%d 2=%d", QuantizeCosineDistance(0), QuantizeCosineDistance(2))
	}
	if QuantizeCosineDistance(float32(math.NaN())) != NoRank {
		t.Fatalf("NaN cosine distance should be NoRank")
	}
}

func TestTimeEncodingMonotonic(t *testing.T) {
	prev := EncodeDistance(0)
	for _, d := range []uint32{1, 2, 3, 10, 100, 1000, 1 << 20, 1 << 30, math.MaxUint32} {
		cur := EncodeDistance(d)
		if cur > prev {
			t.Fatalf("distance %d rank %d increased over prev %d", d, cur, prev)
		}
		prev = cur
	}
	// Property sweep.
	last := EncodeDistance(0)
	for d := uint32(1); d < 200000; d = d*2 + 1 {
		cur := EncodeDistance(d)
		if cur > last {
			t.Fatalf("monotonic violated at %d: %d > %d", d, cur, last)
		}
		last = cur
	}
}

func TestGeoEncodingMonotonic(t *testing.T) {
	prev := EncodeGeoDistance(0)
	for _, d := range []GeoDistance{1, 10, 1000, 100000, 20000000} {
		cur := EncodeGeoDistance(d)
		if cur > prev {
			t.Fatalf("geo %d rank %d > prev %d", d, cur, prev)
		}
		prev = cur
	}
}

func TestCosineMonotonic(t *testing.T) {
	sims := []float32{-1, -0.9, -0.5, 0, 0.5, 0.9, 0.99, 1}
	prev := QuantizeCosine(sims[0])
	for _, s := range sims[1:] {
		cur := QuantizeCosine(s)
		if cur < prev {
			t.Fatalf("sim %v rank %d decreased from %d", s, cur, prev)
		}
		prev = cur
	}
}

func TestExactAndMaxBoundaries(t *testing.T) {
	if got := EncodeDistance(0); got != 65535 {
		t.Fatalf("exact=%d want 65535", got)
	}
	if got := EncodeDistance(math.MaxUint32); got != MinRank {
		t.Fatalf("max distance=%d want %d", got, MinRank)
	}
	if got := EncodeTimeGap(1000, 1000); got != MaxRank {
		t.Fatalf("time exact=%d", got)
	}
	if got := QuantizeCosine(1); got != MaxRank {
		t.Fatalf("cos +1=%d", got)
	}
	if got := QuantizeCosine(-1); got != MinRank {
		t.Fatalf("cos -1=%d", got)
	}
	if got := QuantizeCosine(2); got != MaxRank {
		t.Fatalf("cos clamp high=%d", got)
	}
	if got := QuantizeCosine(-2); got != MinRank {
		t.Fatalf("cos clamp low=%d", got)
	}
}

func TestMissingValues(t *testing.T) {
	if QuantizeCosine(float32(math.NaN())) != NoRank {
		t.Fatal("NaN should be NoRank")
	}
	dst := make([]Rank, 3)
	RankTimeSignal(dst, []ImageID{1, 2, 3}, 1000, func(id ImageID) (uint64, bool) {
		if id == 2 {
			return 0, false
		}
		return 1000, true
	})
	if dst[1] != NoRank {
		t.Fatalf("missing timestamp should be NoRank, got %d", dst[1])
	}
	if dst[0] != MaxRank {
		t.Fatalf("exact should be MaxRank, got %d", dst[0])
	}
	// Missing never outranks ordinary.
	if !(NoRank < MinRank) {
		t.Fatal("NoRank must sort below MinRank")
	}
}

func TestSignalValidation(t *testing.T) {
	var s Signals
	s.ImageIDs = []ImageID{1, 2}
	s.Count = 1
	s.Values[0] = []Rank{10} // short
	if err := s.Validate(); err == nil {
		t.Fatal("short lane should fail validation")
	}
	s.Values[0] = []Rank{10, 20}
	if err := s.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	s.Count = 5
	if err := s.Validate(); err == nil {
		t.Fatal("count>max should fail")
	}
	s.Count = 1
	buf := make([]Rank, 2)
	if err := s.AttachContiguous(buf); err != nil {
		t.Fatalf("attach: %v", err)
	}
	if len(s.Values[0]) != 2 {
		t.Fatal("attach lane length wrong")
	}
	if _, err := SplitRanks(buf, 2, 5); err == nil {
		t.Fatal("split over max should fail")
	}
	if _, err := SplitRanks(buf[:1], 2, 1); err == nil {
		t.Fatal("short buf should fail")
	}
}

func TestFusionDeterminismAndTies(t *testing.T) {
	ids := []ImageID{3, 1, 2}
	mk := func() Signals {
		var s Signals
		s.ImageIDs = append([]ImageID(nil), ids...)
		s.Count = 2
		s.Kinds[0], s.Kinds[1] = KindTime, KindVector
		s.Values[0] = []Rank{100, 300, 200}
		s.Values[1] = []Rank{50, 50, 50} // tie lane
		return s
	}
	var f RRF
	s := mk()
	dst1 := make([]Rank, 3)
	dst2 := make([]Rank, 3)
	if err := f.Fuse(dst1, &s); err != nil {
		t.Fatal(err)
	}
	s2 := mk()
	if err := f.Fuse(dst2, &s2); err != nil {
		t.Fatal(err)
	}
	for i := range dst1 {
		if dst1[i] != dst2[i] {
			t.Fatalf("nondeterministic: %v vs %v", dst1, dst2)
		}
	}
	// Best time (id 1, rank 300) should fuse best.
	best := 0
	for i := 1; i < 3; i++ {
		if dst1[i] > dst1[best] {
			best = i
		}
	}
	if s.ImageIDs[best] != 1 {
		t.Fatalf("fused best=%d want id 1, ranks=%v", s.ImageIDs[best], dst1)
	}
}

func TestEqualRankTieSort(t *testing.T) {
	ids := []ImageID{30, 10, 20}
	ranks := []Rank{100, 100, 100}
	Sort(ids, ranks)
	want := []ImageID{10, 20, 30}
	for i := range want {
		if ids[i] != want[i] {
			t.Fatalf("tie sort=%v want %v", ids, want)
		}
	}
}

func TestEmptySingleMax(t *testing.T) {
	var f RRF
	empty := &Signals{}
	if err := f.Fuse(make([]Rank, 0), empty); err != nil {
		t.Fatalf("empty fuse: %v", err)
	}
	single := &Signals{ImageIDs: []ImageID{7}, Count: 1, Kinds: [MaxSignals]Kind{KindTime}}
	single.Values[0] = []Rank{EncodeDistance(5)}
	dst := make([]Rank, 1)
	if err := f.Fuse(dst, single); err != nil {
		t.Fatal(err)
	}
	if dst[0] == NoRank {
		t.Fatal("single candidate should rank")
	}
	// Maximum expected candidate count smoke (100k lanes stay valid).
	const n = 100000
	big := &Signals{ImageIDs: make([]ImageID, n), Count: 2}
	for i := range big.ImageIDs {
		big.ImageIDs[i] = ImageID(i + 1)
	}
	big.Values[0] = make([]Rank, n)
	big.Values[1] = make([]Rank, n)
	for i := range big.Values[0] {
		big.Values[0][i] = EncodeDistance(uint32(i))
		big.Values[1][i] = QuantizeCosine(0)
	}
	if err := big.Validate(); err != nil {
		t.Fatal(err)
	}
	out := make([]Rank, n)
	if err := f.Fuse(out, big); err != nil {
		t.Fatal(err)
	}
	if out[0] != MaxRank {
		t.Fatalf("best fused=%d want %d", out[0], MaxRank)
	}
}

func TestTopK(t *testing.T) {
	srcIDs := []ImageID{1, 2, 3, 4, 5}
	srcRanks := []Rank{10, 50, 30, 50, 5}
	var dstIDs []ImageID
	var dstRanks []Rank
	gotIDs, gotRanks := TopK(dstIDs, dstRanks, srcIDs, srcRanks, 2)
	if len(gotIDs) != 2 {
		t.Fatalf("topk len=%d", len(gotIDs))
	}
	// Both rank-50 ids, tie-break smaller id first.
	if !(gotIDs[0] == 2 && gotIDs[1] == 4) {
		t.Fatalf("topk=%v ranks=%v want [2 4]", gotIDs, gotRanks)
	}
	gotIDs, gotRanks = TopK(nil, nil, srcIDs, srcRanks, -1)
	if len(gotIDs) != 5 || gotRanks[0] != 50 || gotIDs[4] != 5 {
		t.Fatalf("topk all=%v %v", gotIDs, gotRanks)
	}
	beforeIDs := append([]ImageID(nil), srcIDs...)
	beforeRanks := append([]Rank(nil), srcRanks...)
	TopK(nil, nil, srcIDs, srcRanks, 2)
	for i := range srcIDs {
		if srcIDs[i] != beforeIDs[i] || srcRanks[i] != beforeRanks[i] {
			t.Fatal("TopK must not reorder src")
		}
	}
}

func TestZeroAllocHotPaths(t *testing.T) {
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
	if a := testing.AllocsPerRun(20, func() { EncodeDistances(dst, dist) }); a != 0 {
		t.Fatalf("EncodeDistances allocs=%v", a)
	}
	if a := testing.AllocsPerRun(20, func() { QuantizeCosines(dst, sims) }); a != 0 {
		t.Fatalf("QuantizeCosines allocs=%v", a)
	}
	s := &Signals{ImageIDs: ids, Count: 2}
	s.Values[0] = make([]Rank, n)
	s.Values[1] = make([]Rank, n)
	EncodeDistances(s.Values[0], dist)
	QuantizeCosines(s.Values[1], sims)
	fused := make([]Rank, n)
	var f RRF
	_ = f.Fuse(fused, s) // warm scratch
	if a := testing.AllocsPerRun(10, func() { _ = f.Fuse(fused, s) }); a != 0 {
		t.Fatalf("RRF Fuse allocs=%v", a)
	}
	sortedIDs := append([]ImageID(nil), ids...)
	sortedRanks := append([]Rank(nil), fused...)
	Sort(sortedIDs, sortedRanks) // warm
	if a := testing.AllocsPerRun(10, func() {
		cids := append(sortedIDs[:0:len(sortedIDs)], ids...)
		cranks := append(sortedRanks[:0:len(sortedRanks)], fused...)
		Sort(cids, cranks)
	}); a != 0 {
		t.Fatalf("Sort allocs=%v", a)
	}
}
