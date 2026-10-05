package simhash

import "testing"

func TestShardBoundary(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{MaxShardBytes: shardBytes(1, defaultSeqBlockRecords)}
	st, err := Open(dir, cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	n := 2*slotsPerPage + 5
	records := randRecords(21, n)
	if _, err := st.AppendBatch(records); err != nil {
		t.Fatalf("AppendBatch: %v", err)
	}
	if st.Len() != uint64(n) {
		t.Fatalf("Len = %d, want %d", st.Len(), n)
	}
	if st.ShardCount() != 3 {
		t.Fatalf("ShardCount = %d, want 3", st.ShardCount())
	}
	for _, id := range []uint64{1, slotsPerPage - 1, slotsPerPage, slotsPerPage + 1, 2 * slotsPerPage, uint64(n)} {
		got, ok := st.Get(id)
		if !ok || got != records[id-1] {
			t.Fatalf("Get(%d) = %+v, want %+v", id, got, records[id-1])
		}
	}
	target := records[n-1]
	matches := st.SearchPHash(nil, target.PHash, 0)
	found := false
	for _, m := range matches {
		if m.ImageID == uint64(n) {
			found = true
		}
	}
	if !found {
		t.Fatalf("last-shard record not found: %+v", matches)
	}
}

func TestShardBoundaryReopen(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{MaxShardBytes: shardBytes(1, defaultSeqBlockRecords)}
	st, err := Open(dir, cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	n := slotsPerPage + 7
	records := randRecords(22, n)
	if _, err := st.AppendBatch(records); err != nil {
		t.Fatalf("AppendBatch: %v", err)
	}
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
	if reopened.ShardCount() != 2 || reopened.Len() != uint64(n) {
		t.Fatalf("reopened shards/len = %d/%d, want 2/%d", reopened.ShardCount(), reopened.Len(), n)
	}
	got, ok := reopened.Get(uint64(n))
	if !ok || got != records[n-1] {
		t.Fatalf("reopened Get(%d) = %+v", n, got)
	}
}

func TestMaxShardSize(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{} // default 4 GiB
	st, err := Open(dir, cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()
	geo := newGeometry(defaultSeqBlockRecords)
	size := uint64(headerBytes) + st.PagesPerShard()*geo.pageStride
	if size > defaultMaxShardBytes {
		t.Fatalf("shard size %d exceeds max %d", size, defaultMaxShardBytes)
	}
	if st.PagesPerShard() == 0 {
		t.Fatal("PagesPerShard = 0")
	}
	if st.ShardCount() != 0 {
		t.Fatalf("empty store has %d shards", st.ShardCount())
	}
}
