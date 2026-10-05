package simhash

import (
	"testing"

	common "github.com/evanoberholster/imageIdx/common"
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
