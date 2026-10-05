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
	for _, maxD := range []int{0, 1, 4, 8, 16, 32, 64} {
		got := st.SearchPHash(nil, query, maxD)
		want := oraclePHash(records, query, maxD)
		if !equalPHash(got, want) {
			t.Fatalf("SearchPHash maxD=%d: got %d, want %d", maxD, len(got), len(want))
		}
	}
}

func TestSearchPDQThresholds(t *testing.T) {
	st := openStore(t, testConfig())
	base := randRecords(12, 1)[0]
	rng := rand.New(rand.NewSource(12))
	ladder := []int{0, 1, 2, 4, 8, 16, 30, 31, 32, 40, 50}
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
	for _, maxD := range []uint16{0, 1, 4, 8, 16, 31, 32, 40, 50} {
		got := st.SearchPDQ(nil, base.PDQ, maxD)
		want := oraclePDQ(records, base.PDQ, maxD)
		if !equalPDQ(got, want) {
			t.Fatalf("SearchPDQ maxD=%d: got %+v, want %+v", maxD, got, want)
		}
	}
	// Threshold 31 explicitly: the distance-31 record must be included and the
	// distance-32 record excluded.
	got31 := st.SearchPDQ(nil, base.PDQ, 31)
	seen31, seen32 := false, false
	for _, m := range got31 {
		if m.ImageID == 9 && m.Distance == 31 {
			seen31 = true
		}
		if m.Distance == 32 {
			seen32 = true
		}
	}
	if !seen31 {
		t.Fatal("distance-31 record missing at threshold 31")
	}
	if seen32 {
		t.Fatal("distance-32 record present at threshold 31")
	}
}

func TestSearchCombined(t *testing.T) {
	st := openStore(t, testConfig())
	records := randRecords(13, 3000)
	if _, err := st.AppendBatch(records); err != nil {
		t.Fatalf("AppendBatch: %v", err)
	}
	rng := rand.New(rand.NewSource(13))
	phashQuery := flipPHash(rng, records[0].PHash, 7)
	pdqQuery := flipPDQ(rng, records[0].PDQ, 18)
	for _, order := range []Order{PHashFirst, PDQFirst} {
		for _, maxPH := range []int{0, 4, 8, 16, 32, 64} {
			for _, maxPDQ := range []uint16{0, 8, 16, 31} {
				got := st.SearchCombined(nil, phashQuery, maxPH, pdqQuery, maxPDQ, order)
				want := oracleCombined(records, phashQuery, maxPH, pdqQuery, maxPDQ)
				if !equalCombined(got, want) {
					t.Fatalf("combined order=%d maxPH=%d maxPDQ=%d: got %d, want %d",
						order, maxPH, maxPDQ, len(got), len(want))
				}
			}
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
	got := st.SearchPDQ(nil, q, 1)
	if len(got) != 1 || got[0].ImageID != 1 || got[0].Distance != 1 {
		t.Fatalf("SearchPDQ = %+v", got)
	}
	// Combined with maxPHash 0 must exclude it (pHash distance is 0 here, so
	// instead use a query pHash far away).
	gotC := st.SearchCombined(nil, ^uint64(0), 0, q, 1, PHashFirst)
	if len(gotC) != 0 {
		t.Fatalf("combined should exclude, got %+v", gotC)
	}
}

func TestSearchEmptyStore(t *testing.T) {
	st := openStore(t, testConfig())
	if got := st.SearchPHash(nil, 1, 8); len(got) != 0 {
		t.Fatalf("SearchPHash on empty = %+v", got)
	}
	if got := st.SearchPDQ(nil, PDQHash{}, 8); len(got) != 0 {
		t.Fatalf("SearchPDQ on empty = %+v", got)
	}
	if got := st.SearchCombined(nil, 1, 8, PDQHash{}, 8, PHashFirst); len(got) != 0 {
		t.Fatalf("SearchCombined on empty = %+v", got)
	}
}
