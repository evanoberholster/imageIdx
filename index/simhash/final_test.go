package simhash

import (
	"os"
	"runtime"
	"sync"
	"testing"

	common "github.com/evanoberholster/imageIdx/common"
)

// TestAppendPreflightFailureLeavesStateUnchanged induces a shard-creation
// failure by making the directory read-only after one shard exists, then
// appending across a shard boundary. Logical state must be unchanged; any
// already-created shard is physical capacity only.
func TestAppendPreflightFailureLeavesStateUnchanged(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{MaxShardBytes: shardBytes(1, defaultSeqBlockRecords)}
	st, err := Open(dir, cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()
	if _, err := st.Append(randRecords(1, 1)[0]); err != nil {
		t.Fatalf("Append: %v", err)
	}
	beforeLen := st.Len()
	beforeGen := st.Generation()

	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	defer os.Chmod(dir, 0o755)
	// IDs 2..slotsPerHashPage+1 span past shard 0 (one page), so shard 1 must be
	// created; creation fails because the directory is read-only.
	recs := randRecords(2, slotsPerHashPage)
	if _, err := st.AppendBatch(recs); err == nil {
		t.Skip("could not induce a shard-creation failure (running privileged?)")
	}
	if st.Len() != beforeLen {
		t.Fatalf("Len changed after failed preflight: %d -> %d", beforeLen, st.Len())
	}
	if st.Generation() != beforeGen {
		t.Fatalf("generation changed after failed preflight: %d -> %d", beforeGen, st.Generation())
	}
}

// TestZeroAllocationOperations protects the allocation behavior of every hot
// operation with testing.AllocsPerRun.
func TestZeroAllocationOperations(t *testing.T) {
	st, err := Open(t.TempDir(), testConfig())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()
	records := randRecords(300, 20_000)
	if _, err := st.AppendBatch(records); err != nil {
		t.Fatalf("AppendBatch: %v", err)
	}
	phDst := make([]PHashMatch, 0, 8192)
	pdDst := make([]PDQMatch, 0, 8192)
	cbDst := make([]CombinedMatch, 0, 8192)
	batch := records[:64]
	single := records[0]
	updates := make([]Update, 64)
	for i := range updates {
		updates[i] = Update{ImageID: common.ImageID(1 + i), Record: records[i]}
	}

	check := func(name string, f func()) {
		t.Helper()
		if n := testing.AllocsPerRun(300, f); n != 0 {
			t.Fatalf("%s: %.2f allocs/op, want 0", name, n)
		}
	}
	check("Append", func() { _, _ = st.Append(single) })
	check("AppendBatch64", func() { _, _ = st.AppendBatch(batch) })
	check("Upsert", func() { _ = st.Upsert(1, single) })
	check("UpsertBatch64", func() { _ = st.UpsertBatch(updates) })
	check("Get", func() { _, _ = st.Get(1) })
	check("Quality", func() { _, _ = st.Quality(1) })
	check("SearchPHash", func() { phDst = st.SearchPHash(phDst[:0], single.PHash, 0) })
	check("SearchPDQ", func() { pdDst = st.SearchPDQ(pdDst[:0], single.PDQ, 0) })
	check("SearchCombined", func() {
		cbDst = st.SearchCombined(cbDst[:0], single.PHash, 0, single.PDQ, 0, PHashFirst)
	})
}

// TestUpsertBatchDuplicateThreeLastWins checks the documented last-wins rule.
func TestUpsertBatchDuplicateThreeLastWins(t *testing.T) {
	st := openStore(t, testConfig())
	if _, err := st.AppendBatch(randRecords(301, 4)); err != nil {
		t.Fatalf("AppendBatch: %v", err)
	}
	a, b, c := randRecords(302, 1)[0], randRecords(303, 1)[0], randRecords(304, 1)[0]
	if err := st.UpsertBatch([]Update{
		{ImageID: 2, Record: a},
		{ImageID: 2, Record: b},
		{ImageID: 2, Record: c},
	}); err != nil {
		t.Fatalf("UpsertBatch: %v", err)
	}
	if got, _ := st.Get(2); got != c {
		t.Fatalf("duplicate last-wins: got %+v, want %+v", got, c)
	}
}

// TestCloseRaceStress repeatedly races all read APIs with Close under -race.
func TestCloseRaceStress(t *testing.T) {
	for round := 0; round < 20; round++ {
		st, err := Open(t.TempDir(), testConfig())
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		if _, err := st.AppendBatch(randRecords(int64(round+1), 1000)); err != nil {
			t.Fatalf("AppendBatch: %v", err)
		}
		stop := make(chan struct{})
		var wg sync.WaitGroup
		for r := 0; r < 4; r++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for {
					select {
					case <-stop:
						return
					default:
					}
					_, _ = st.Get(1)
					_, _ = st.Quality(1)
					_ = st.SearchPHash(nil, 0x1234, 8)
					_ = st.SearchPDQ(nil, PDQHash{}, 8)
					_ = st.SearchCombined(nil, 0x1234, 8, PDQHash{}, 8, PHashFirst)
				}
			}()
		}
		runtime.Gosched()
		if err := st.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		close(stop)
		wg.Wait()
	}
}
