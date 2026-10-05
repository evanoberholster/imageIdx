package simhash

import (
	"runtime"
	"sync"
	"testing"
)

func TestClosedStoreReads(t *testing.T) {
	st, err := Open(t.TempDir(), testConfig())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := st.AppendBatch(randRecords(1, 8)); err != nil {
		t.Fatalf("AppendBatch: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, ok := st.Get(1); ok {
		t.Fatal("Get after Close returned a record")
	}
	if got := st.SearchPHash(nil, 0, 8); len(got) != 0 {
		t.Fatalf("SearchPHash after Close = %+v", got)
	}
	if got := st.SearchPDQ(nil, PDQHash{}, 8); len(got) != 0 {
		t.Fatalf("SearchPDQ after Close = %+v", got)
	}
	if got := st.SearchCombined(nil, 0, 8, PDQHash{}, 8, PHashFirst); len(got) != 0 {
		t.Fatalf("SearchCombined after Close = %+v", got)
	}
	if _, ok := st.Quality(1); ok {
		t.Fatal("Quality after Close returned a value")
	}
}

func TestConcurrentSearchClose(t *testing.T) {
	st, err := Open(t.TempDir(), testConfig())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := st.AppendBatch(randRecords(2, 1000)); err != nil {
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
				_ = st.SearchPHash(nil, 0x1234, 8)
				_ = st.SearchPDQ(nil, PDQHash{}, 8)
				_ = st.SearchCombined(nil, 0x1234, 8, PDQHash{}, 8, PHashFirst)
				_, _ = st.Get(1)
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

func TestOverflowRejected(t *testing.T) {
	st, err := Open(t.TempDir(), testConfig())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()
	rec := randRecords(3, 1)[0]
	if _, err := st.Append(rec); err != nil {
		t.Fatalf("Append: %v", err)
	}
	st.mu.Lock()
	st.generation = ^uint64(0)
	st.mu.Unlock()

	before := st.Len()
	if _, err := st.Append(rec); err != ErrOverflow {
		t.Fatalf("Append = %v, want ErrOverflow", err)
	}
	if err := st.Upsert(1, rec); err != ErrOverflow {
		t.Fatalf("Upsert = %v, want ErrOverflow", err)
	}
	if err := st.UpsertBatch([]Update{{ImageID: 1, Record: rec}}); err != ErrOverflow {
		t.Fatalf("UpsertBatch = %v, want ErrOverflow", err)
	}
	if st.Len() != before {
		t.Fatalf("committed changed after overflow: %d != %d", st.Len(), before)
	}
}

// TestAppendPreflightAllShards verifies a batch crossing a shard boundary
// creates all required shards before writing and yields a consistent store.
func TestAppendPreflightAllShards(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{MaxShardBytes: shardBytes(1, defaultSeqBlockRecords)}
	st, err := Open(dir, cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()
	n := 2*slotsPerHashPage + 7
	records := randRecords(4, n)
	if _, err := st.AppendBatch(records); err != nil {
		t.Fatalf("AppendBatch: %v", err)
	}
	if st.ShardCount() != 3 {
		t.Fatalf("ShardCount = %d, want 3", st.ShardCount())
	}
	for i := 0; i < n; i++ {
		if got, ok := st.Get(uint64(i + 1)); !ok || got != records[i] {
			t.Fatalf("Get(%d) mismatch", i+1)
		}
	}
}

func TestSequenceBlockShiftMatchesDivision(t *testing.T) {
	for _, seq := range []int{256, 512, 1024, 2048, 4096} {
		g := newGeometry(seq)
		for slot := 0; slot < slotsPerHashPage; slot += 997 {
			if g.blockOf(slot) != slot/seq {
				t.Fatalf("seq %d slot %d: blockOf=%d want %d", seq, slot, g.blockOf(slot), slot/seq)
			}
		}
	}
}
