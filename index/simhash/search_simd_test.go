package simhash

import (
	"math/rand"
	"testing"
)

func TestSearchPDQBulkMatchesSearchPDQ(t *testing.T) {
	st := openStore(t, testConfig())
	records := randRecords(400, 5000)
	if _, err := st.AppendBatch(records); err != nil {
		t.Fatalf("AppendBatch: %v", err)
	}
	rng := rand.New(rand.NewSource(401))
	for k := 0; k < 40; k++ {
		q := flipPDQ(rng, records[k*7%len(records)].PDQ, k%25)
		for _, maxD := range []uint16{0, 4, 8, 16, 31, 32, 50} {
			bulk := st.SearchPDQBulk(nil, q, maxD)
			scalar := st.SearchPDQ(nil, q, maxD)
			if !equalPDQ(bulk, scalar) {
				t.Fatalf("bulk maxD=%d q=%d: got %d want %d", maxD, k, len(bulk), len(scalar))
			}
			want := oraclePDQ(records, q, maxD)
			if !equalPDQ(bulk, want) {
				t.Fatalf("bulk maxD=%d q=%d: got %d oracle %d", maxD, k, len(bulk), len(want))
			}
		}
	}
}

func TestSearchPDQBulkEmptyAndClosed(t *testing.T) {
	st := openStore(t, testConfig())
	if got := st.SearchPDQBulk(nil, PDQHash{}, 8); len(got) != 0 {
		t.Fatalf("empty bulk = %+v", got)
	}
	if _, err := st.AppendBatch(randRecords(402, 8)); err != nil {
		t.Fatalf("AppendBatch: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := st.SearchPDQBulk(nil, PDQHash{}, 8); len(got) != 0 {
		t.Fatalf("closed bulk = %+v", got)
	}
}
