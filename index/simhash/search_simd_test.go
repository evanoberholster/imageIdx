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

// TestSearchPDQBulkBoundaries checks the SIMD bulk scan at hash-page and shard
// boundaries, including the final partial block, against the scalar scan and the
// oracle.
func TestSearchPDQBulkBoundaries(t *testing.T) {
	check := func(t *testing.T, st *Store, records []Record, slots []int) {
		t.Helper()
		for _, slot := range slots {
			q := records[slot].PDQ
			for _, maxD := range []uint16{0, 1, 8, 16, 31} {
				bulk := st.SearchPDQBulk(nil, q, maxD)
				scalar := st.SearchPDQ(nil, q, maxD)
				if !equalPDQ(bulk, scalar) {
					t.Fatalf("slot %d maxD %d: bulk %d scalar %d", slot, maxD, len(bulk), len(scalar))
				}
				if want := oraclePDQ(records, q, maxD); !equalPDQ(bulk, want) {
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
