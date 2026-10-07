package simhash

import (
	"math/rand"
	"testing"
)

func TestSearchPHash(t *testing.T) {
	st := openStore(t, testConfig())
	records := randRecords(11, 3000)
	if _, err := st.AppendBatch(records); err != nil {
		t.Fatalf("AppendBatch: %v", err)
	}
	rng := rand.New(rand.NewSource(11))
	query := flipPHash(rng, records[0].PHash, 9)
	for _, maxD := range []uint8{0, 1, 4, 8, 16, 32, 64} {
		got := st.SearchPHash(nil, query, maxD)
		want := oraclePHash(records, query, maxD)
		if !equalResults(got, want) {
			t.Fatalf("SearchPHash maxD=%d: got %d, want %d", maxD, len(got), len(want))
		}
	}
}

// TestSearchPHashBoundaries pins the inclusive thresholds across the full pHash
// distance range, including 0, 63, and 64 (all bits flipped).
func TestSearchPHashBoundaries(t *testing.T) {
	st := openStore(t, testConfig())
	base := randRecords(20, 1)[0]
	rng := rand.New(rand.NewSource(20))
	ladder := []int{0, 1, 16, 31, 32, 63, 64}
	records := make([]Record, 0, len(ladder))
	for _, d := range ladder {
		records = append(records, Record{
			PHash:   flipPHash(rng, base.PHash, d),
			PDQ:     base.PDQ,
			Quality: uint8(d),
		})
	}
	if _, err := st.AppendBatch(records); err != nil {
		t.Fatalf("AppendBatch: %v", err)
	}
	for i, d := range ladder {
		if got := DistancePHash(base.PHash, records[i].PHash); got != uint8(d) {
			t.Fatalf("record %d: exact distance %d, want %d", i, got, d)
		}
	}
	for _, maxD := range []uint8{0, 1, 16, 31, 32, 63, 64} {
		got := st.SearchPHash(nil, base.PHash, maxD)
		want := oraclePHash(records, base.PHash, maxD)
		if !equalResults(got, want) {
			t.Fatalf("SearchPHash maxD=%d: got %+v, want %+v", maxD, got, want)
		}
	}
}

// TestDistancePDQMathematicalRange keeps the low-level primitive honest: it
// returns the exact uint16 distance over the complete 0..256 range, independently
// of the uint8-limited search representation.
func TestDistancePDQMathematicalRange(t *testing.T) {
	base := randRecords(59, 1)[0]
	rng := rand.New(rand.NewSource(59))
	for _, d := range []int{0, 1, 31, 32, 64, 127, 128, 129, 254, 255, 256} {
		h := flipPDQ(rng, base.PDQ, d)
		if got := DistancePDQ(&base.PDQ, &h); got != uint16(d) {
			t.Fatalf("DistancePDQ distance %d: got %d, want %d", d, got, d)
		}
	}
}

// TestSearchPDQThresholds covers the useful similarity range up to the supported
// radius of MaxPDQSearchDistance.
func TestSearchPDQThresholds(t *testing.T) {
	st := openStore(t, testConfig())
	base := randRecords(12, 1)[0]
	rng := rand.New(rand.NewSource(12))
	ladder := []int{0, 1, 8, 16, 31, 32, 40, 50, 64, 96, 127, 128}
	records := make([]Record, 0, len(ladder)+1)
	records = append(records, base)
	for _, d := range ladder {
		records = append(records, Record{
			PHash:   rng.Uint64(),
			PDQ:     flipPDQ(rng, base.PDQ, d),
			Quality: uint8(d),
		})
	}
	if _, err := st.AppendBatch(records); err != nil {
		t.Fatalf("AppendBatch: %v", err)
	}
	for i, d := range ladder {
		if got := DistancePDQ(&base.PDQ, &records[i+1].PDQ); got != uint16(d) {
			t.Fatalf("record %d: exact distance %d, want %d", i+1, got, d)
		}
	}
	for _, maxD := range []uint8{0, 1, 8, 16, 31, 32, 40, 50, 64, 96, 127, 128} {
		got := st.SearchPDQ(nil, &base.PDQ, maxD)
		want := oraclePDQ(records, base.PDQ, maxD)
		if !equalResults(got, want) {
			t.Fatalf("SearchPDQ maxD=%d: got %d, want %d", maxD, len(got), len(want))
		}
	}
}

// TestSearchPDQRadiusBoundary pins the top of the supported search radius:
// distance 127 is included at 127, distance 128 is included at 128, and distance
// 129 is excluded at 128.
func TestSearchPDQRadiusBoundary(t *testing.T) {
	st := openStore(t, testConfig())
	base := randRecords(60, 1)[0]
	rng := rand.New(rand.NewSource(60))
	records := []Record{
		base,
		{PHash: rng.Uint64(), PDQ: flipPDQ(rng, base.PDQ, 127), Quality: 1},
		{PHash: rng.Uint64(), PDQ: flipPDQ(rng, base.PDQ, 128), Quality: 2},
		{PHash: rng.Uint64(), PDQ: flipPDQ(rng, base.PDQ, 129), Quality: 3},
	}
	if _, err := st.AppendBatch(records); err != nil {
		t.Fatalf("AppendBatch: %v", err)
	}
	for i, want := range []uint16{0, 127, 128, 129} {
		if got := DistancePDQ(&base.PDQ, &records[i].PDQ); got != want {
			t.Fatalf("record %d: exact distance %d, want %d", i, got, want)
		}
	}

	got127 := st.SearchPDQ(nil, &base.PDQ, 127)
	if len(got127) != 2 || got127[0].ImageID != 1 || got127[0].Distance != 0 ||
		got127[1].ImageID != 2 || got127[1].Distance != 127 {
		t.Fatalf("maxDistance=127: got %+v, want [{1 0} {2 127}]", got127)
	}

	got128 := st.SearchPDQ(nil, &base.PDQ, 128)
	if len(got128) != 3 || got128[2].ImageID != 3 || got128[2].Distance != 128 {
		t.Fatalf("maxDistance=128: got %+v, want 3 results with {3 128} last", got128)
	}
	for _, m := range got128 {
		if m.ImageID == 4 {
			t.Fatalf("maxDistance=128: distance-129 record leaked: %+v", got128)
		}
	}

	if bulk := st.SearchPDQBulk(nil, &base.PDQ, 128); !equalResults(bulk, got128) {
		t.Fatalf("bulk maxDistance=128: got %+v, want %+v", bulk, got128)
	}
}

// TestSearchPDQClampThreshold verifies that any threshold above
// MaxPDQSearchDistance behaves exactly like MaxPDQSearchDistance, for both the
// concurrent-safe scalar scan and the bulk scan.
func TestSearchPDQClampThreshold(t *testing.T) {
	st := openStore(t, testConfig())
	records := randRecords(61, 2000)
	if _, err := st.AppendBatch(records); err != nil {
		t.Fatalf("AppendBatch: %v", err)
	}
	query := flipPDQ(rand.New(rand.NewSource(62)), records[0].PDQ, 100)
	atMaxScalar := st.SearchPDQ(nil, &query, MaxPDQSearchDistance)
	atMaxBulk := st.SearchPDQBulk(nil, &query, MaxPDQSearchDistance)
	for _, maxD := range []uint8{129, 200, 255} {
		if got := st.SearchPDQ(nil, &query, maxD); !equalResults(got, atMaxScalar) {
			t.Fatalf("SearchPDQ maxD=%d: got %d, want %d (clamped)", maxD, len(got), len(atMaxScalar))
		}
		if got := st.SearchPDQBulk(nil, &query, maxD); !equalResults(got, atMaxBulk) {
			t.Fatalf("SearchPDQBulk maxD=%d: got %d, want %d (clamped)", maxD, len(got), len(atMaxBulk))
		}
	}
}

func TestPDQIndependentOfPHash(t *testing.T) {
	// A PDQ-only query must evaluate PDQ independently: a record with a far
	// pHash but near PDQ must still be returned.
	st := openStore(t, testConfig())
	records := []Record{
		{PHash: 0, PDQ: PDQHash{}},
	}
	q := PDQHash{}
	q[0] = 1 // distance 1 from records[0]
	if _, err := st.AppendBatch(records); err != nil {
		t.Fatalf("AppendBatch: %v", err)
	}
	got := st.SearchPDQ(nil, &q, 1)
	if len(got) != 1 || got[0].ImageID != 1 || got[0].Distance != 1 {
		t.Fatalf("SearchPDQ = %+v", got)
	}
}

func TestSearchEmptyStore(t *testing.T) {
	st := openStore(t, testConfig())
	if got := st.SearchPHash(nil, 1, 8); len(got) != 0 {
		t.Fatalf("SearchPHash on empty = %+v", got)
	}
	var zero PDQHash
	if got := st.SearchPDQ(nil, &zero, 8); len(got) != 0 {
		t.Fatalf("SearchPDQ on empty = %+v", got)
	}
}

// TestSearchDifferential compares both searches against brute-force exact
// Hamming distance over deterministic random datasets, requiring identical
// ImageIDs, distances, and ordering. It also requires SearchPDQBulk to match
// SearchPDQ exactly on a quiescent store, including for clamped thresholds.
func TestSearchDifferential(t *testing.T) {
	st := openStore(t, testConfig())
	records := randRecords(700, 8000)
	if _, err := st.AppendBatch(records); err != nil {
		t.Fatalf("AppendBatch: %v", err)
	}
	rng := rand.New(rand.NewSource(701))

	phThresholds := []uint8{0, 1, 8, 16, 31, 32, 63, 64}
	for k := 0; k < 40; k++ {
		query := flipPHash(rng, records[k*7%len(records)].PHash, k%65)
		for _, maxD := range phThresholds {
			got := st.SearchPHash(nil, query, maxD)
			want := oraclePHash(records, query, maxD)
			if !equalResults(got, want) {
				t.Fatalf("pHash q=%d maxD=%d: got %d, want %d", k, maxD, len(got), len(want))
			}
			assertAscending(t, got)
		}
	}

	pdqThresholds := []uint8{0, 1, 8, 16, 31, 32, 40, 50, 64, 96, 127, 128, 129, 200, 255}
	for k := 0; k < 24; k++ {
		query := flipPDQ(rng, records[k*11%len(records)].PDQ, (k*37)%256)
		for _, maxD := range pdqThresholds {
			got := st.SearchPDQ(nil, &query, maxD)
			want := oraclePDQ(records, query, maxD)
			if !equalResults(got, want) {
				t.Fatalf("pDQ q=%d maxD=%d: got %d, want %d", k, maxD, len(got), len(want))
			}
			assertAscending(t, got)
			bulk := st.SearchPDQBulk(nil, &query, maxD)
			if !equalResults(bulk, got) {
				t.Fatalf("bulk q=%d maxD=%d mismatches scalar", k, maxD)
			}
		}
	}
}

// assertAscending requires strictly ascending ImageIDs.
func assertAscending(t *testing.T, got []Result) {
	t.Helper()
	for i := 1; i < len(got); i++ {
		if got[i-1].ImageID >= got[i].ImageID {
			t.Fatalf("results not strictly ascending at %d: %+v", i, got[i-1:i+1])
		}
	}
}

func TestSearchPDQBulkMatchesSearchPDQ(t *testing.T) {
	st := openStore(t, testConfig())
	records := randRecords(400, 5000)
	if _, err := st.AppendBatch(records); err != nil {
		t.Fatalf("AppendBatch: %v", err)
	}
	rng := rand.New(rand.NewSource(401))
	for k := 0; k < 40; k++ {
		q := flipPDQ(rng, records[k*7%len(records)].PDQ, k%25)
		for _, maxD := range []uint8{0, 4, 8, 16, 31, 32, 50, 96, 128, 200, 255} {
			bulk := st.SearchPDQBulk(nil, &q, maxD)
			scalar := st.SearchPDQ(nil, &q, maxD)
			if !equalResults(bulk, scalar) {
				t.Fatalf("bulk maxD=%d q=%d: got %d want %d", maxD, k, len(bulk), len(scalar))
			}
			want := oraclePDQ(records, q, maxD)
			if !equalResults(bulk, want) {
				t.Fatalf("bulk maxD=%d q=%d: got %d oracle %d", maxD, k, len(bulk), len(want))
			}
		}
	}
}

// TestSearchPDQBulkBoundaries checks the SIMD bulk scan at hash-page and shard
// boundaries, including the final partial block, against the scalar scan and the
// oracle.
func TestSearchPDQBulkBoundaries(t *testing.T) {
	check := func(t *testing.T, st *Store, records []Record, slots []int) {
		t.Helper()
		for _, slot := range slots {
			q := records[slot].PDQ
			for _, maxD := range []uint8{0, 1, 8, 16, 31, 32} {
				bulk := st.SearchPDQBulk(nil, &q, maxD)
				scalar := st.SearchPDQ(nil, &q, maxD)
				if !equalResults(bulk, scalar) {
					t.Fatalf("slot %d maxD %d: bulk %d scalar %d", slot, maxD, len(bulk), len(scalar))
				}
				if want := oraclePDQ(records, q, maxD); !equalResults(bulk, want) {
					t.Fatalf("slot %d maxD %d: bulk %d oracle %d", slot, maxD, len(bulk), len(want))
				}
			}
		}
	}

	t.Run("hash-page", func(t *testing.T) {
		st := openStore(t, testConfig())
		records := randRecords(500, slotsPerHashPage)
		if _, err := st.AppendBatch(records); err != nil {
			t.Fatalf("AppendBatch: %v", err)
		}
		check(t, st, records, []int{0, 1, 2, 63, 64, slotsPerHashPage - 2, slotsPerHashPage - 1})
	})

	t.Run("shard", func(t *testing.T) {
		dir := t.TempDir()
		cfg := Config{MaxShardBytes: shardBytes(1, defaultSeqBlockRecords)}
		st, err := Open(dir, cfg)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		defer st.Close()
		n := 2*slotsPerHashPage + 7
		records := randRecords(501, n)
		if _, err := st.AppendBatch(records); err != nil {
			t.Fatalf("AppendBatch: %v", err)
		}
		check(t, st, records, []int{slotsPerHashPage - 1, slotsPerHashPage, slotsPerHashPage + 1, n - 1})
	})
}

func TestSearchPDQBulkEmptyAndClosed(t *testing.T) {
	st := openStore(t, testConfig())
	var zero PDQHash
	if got := st.SearchPDQBulk(nil, &zero, 8); len(got) != 0 {
		t.Fatalf("empty bulk = %+v", got)
	}
	if _, err := st.AppendBatch(randRecords(402, 8)); err != nil {
		t.Fatalf("AppendBatch: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := st.SearchPDQBulk(nil, &zero, 8); len(got) != 0 {
		t.Fatalf("closed bulk = %+v", got)
	}
}
