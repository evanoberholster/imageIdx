package simhash

import (
	common "github.com/evanoberholster/imageIdx/common"
	"runtime"
	"sync"
	"testing"
)

func testConfig() Config {
	return Config{MaxShardBytes: shardBytes(4, defaultSeqBlockRecords)}
}

func TestAppendGetQuality(t *testing.T) {
	st := openStore(t, testConfig())
	records := randRecords(1, 100)
	first, err := st.AppendBatch(records)
	if err != nil {
		t.Fatalf("AppendBatch: %v", err)
	}
	if first != 1 {
		t.Fatalf("first ID = %d, want 1", first)
	}
	if st.Len() != 100 {
		t.Fatalf("Len = %d, want 100", st.Len())
	}
	for i := range records {
		got, ok := st.Get(uint64(i + 1))
		if !ok || got != records[i] {
			t.Fatalf("Get(%d) = %+v, want %+v", i+1, got, records[i])
		}
		q, ok := st.Quality(uint64(i + 1))
		if !ok || q != records[i].Quality {
			t.Fatalf("Quality(%d) = %d, want %d", i+1, q, records[i].Quality)
		}
	}
	if _, ok := st.Get(0); ok {
		t.Fatal("Get(0) succeeded")
	}
	if _, ok := st.Get(101); ok {
		t.Fatal("Get(beyond high-water) succeeded")
	}
}

func TestAppendVisibility(t *testing.T) {
	st := openStore(t, testConfig())
	records := randRecords(2, 50)
	if _, err := st.AppendBatch(records); err != nil {
		t.Fatalf("AppendBatch: %v", err)
	}
	target := records[10]
	if matches := st.SearchPHash(nil, target.PHash, 0); len(matches) != 1 || matches[0].ImageID != 11 {
		t.Fatalf("before second append: matches = %+v", matches)
	}
	extra := randRecords(3, 10)
	first, err := st.AppendBatch(extra)
	if err != nil {
		t.Fatalf("AppendBatch: %v", err)
	}
	if first != 51 {
		t.Fatalf("first = %d, want 51", first)
	}
	if matches := st.SearchPHash(nil, extra[0].PHash, 0); len(matches) != 1 || matches[0].ImageID != 51 {
		t.Fatalf("after second append: matches = %+v", matches)
	}
}

func TestReopen(t *testing.T) {
	dir := t.TempDir()
	cfg := testConfig()
	st, err := Open(dir, cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	records := randRecords(4, 500)
	if _, err := st.AppendBatch(records); err != nil {
		t.Fatalf("AppendBatch: %v", err)
	}
	wantQuality, _ := st.Quality(7)
	gen := st.Generation()
	if err := st.Checkpoint(); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	reopened, err := Open(dir, cfg)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	if reopened.Len() != 500 {
		t.Fatalf("reopened Len = %d, want 500", reopened.Len())
	}
	if reopened.Generation() != gen {
		t.Fatalf("reopened Generation = %d, want %d", reopened.Generation(), gen)
	}
	for _, id := range []uint64{1, 7, 250, 500} {
		got, ok := reopened.Get(id)
		if !ok || got != records[id-1] {
			t.Fatalf("reopened Get(%d) = %+v, want %+v", id, got, records[id-1])
		}
	}
	if q, _ := reopened.Quality(7); q != wantQuality {
		t.Fatalf("reopened Quality(7) = %d, want %d", q, wantQuality)
	}
	got := reopened.SearchPDQ(nil, records[0].PDQ, 0)
	if len(got) != 1 || got[0].ImageID != 1 {
		t.Fatalf("reopened SearchPDQ = %+v", got)
	}
}

func TestGenerationSelection(t *testing.T) {
	dir := t.TempDir()
	cfg := testConfig()
	st, err := Open(dir, cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := st.AppendBatch(randRecords(5, 100)); err != nil {
		t.Fatalf("AppendBatch: %v", err)
	}
	if err := st.Checkpoint(); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	if _, err := st.AppendBatch(randRecords(6, 100)); err != nil {
		t.Fatalf("AppendBatch: %v", err)
	}
	if err := st.Checkpoint(); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	gen := st.Generation()
	liveness := st.Len()
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	reopened, err := Open(dir, cfg)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	if reopened.Len() != liveness || reopened.Generation() != gen {
		t.Fatalf("reopened Len/Gen = %d/%d, want %d/%d", reopened.Len(), reopened.Generation(), liveness, gen)
	}
}

func TestUpsertAndQuality(t *testing.T) {
	st := openStore(t, testConfig())
	records := randRecords(7, 64)
	if _, err := st.AppendBatch(records); err != nil {
		t.Fatalf("AppendBatch: %v", err)
	}
	updated := randRecords(8, 1)[0]
	if err := st.Upsert(20, updated); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	got, _ := st.Get(20)
	if got != updated {
		t.Fatalf("Get after Upsert = %+v, want %+v", got, updated)
	}
	if err := st.Upsert(0, updated); err != ErrInvalidID {
		t.Fatalf("Upsert(0) = %v, want ErrInvalidID", err)
	}
	if err := st.Upsert(1000, updated); err != ErrInvalidID {
		t.Fatalf("Upsert(beyond) = %v, want ErrInvalidID", err)
	}
}

func TestClosedStore(t *testing.T) {
	st, err := Open(t.TempDir(), testConfig())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := st.Append(randRecords(9, 1)[0]); err != ErrClosed {
		t.Fatalf("Append after Close = %v, want ErrClosed", err)
	}
	if err := st.Upsert(1, Record{}); err != ErrClosed {
		t.Fatalf("Upsert after Close = %v, want ErrClosed", err)
	}
	_ = common.ImageID(0)
}

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
