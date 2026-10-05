package simhash

import (
	"os"
	"testing"

	"github.com/evanoberholster/imageIdx/internal/storage"
)

func buildCheckpointed(t *testing.T, dir string, cfg Config, n int) *Store {
	t.Helper()
	st, err := Open(dir, cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := st.AppendBatch(randRecords(int64(n), n)); err != nil {
		t.Fatalf("AppendBatch: %v", err)
	}
	if err := st.Checkpoint(); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	return st
}

// A shard superblock newer than the manifest generation is an interrupted
// commit: open must ignore it and select the committed slot.
func TestShardNewerThanManifestSkipped(t *testing.T) {
	dir := t.TempDir()
	cfg := testConfig()
	st := buildCheckpointed(t, dir, cfg, 100)
	sh := st.shards[0]
	sh.generation = st.generation + 1
	if err := sh.writeMeta(); err != nil {
		t.Fatalf("writeMeta: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	reopened, err := Open(dir, cfg)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	if reopened.Len() != 100 || reopened.Generation() != st.generation {
		t.Fatalf("reopened len/gen = %d/%d, want 100/%d", reopened.Len(), reopened.Generation(), st.generation)
	}
}

// If every shard slot is newer than the manifest, no committed metadata exists.
func TestShardNewerThanManifestBothSlotsCorrupt(t *testing.T) {
	dir := t.TempDir()
	cfg := testConfig()
	st := buildCheckpointed(t, dir, cfg, 100)
	sh := st.shards[0]
	for i := 0; i < 2; i++ {
		sh.generation = st.generation + 1
		if err := sh.writeMeta(); err != nil {
			t.Fatalf("writeMeta: %v", err)
		}
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := Open(dir, cfg); err == nil {
		t.Fatal("Open accepted shards newer than the manifest")
	}
}

// A manifest that claims more committed records than the shard contains is
// inconsistent.
func TestManifestNewerThanShardCorrupt(t *testing.T) {
	dir := t.TempDir()
	cfg := testConfig()
	st := buildCheckpointed(t, dir, cfg, 100)
	gen := st.generation
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	data := encodeManifest(manifest{
		pagesPerShard:   st.pagesPerShard,
		seqBlockRecords: uint64(cfg.SeqBlockRecords),
		committed:       100_000,
		generation:      gen + 1,
		shardCount:      1,
	})
	if err := storage.WriteFileAtomic(dir, "manifest.tmp", "manifest", data, 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	if _, err := Open(dir, cfg); err == nil {
		t.Fatal("Open accepted a manifest newer than shard data")
	}
}

// A single corrupt superblock slot must not prevent opening from the other.
func TestCorruptShardSuperblockSlot(t *testing.T) {
	dir := t.TempDir()
	cfg := testConfig()
	st := buildCheckpointed(t, dir, cfg, 100)
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	path := shardPath(dir, 0)
	file, err := os.OpenFile(path, os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open shard: %v", err)
	}
	if _, err := file.WriteAt(make([]byte, storage.SuperblockSlotBytes), 0); err != nil {
		t.Fatalf("zero slot: %v", err)
	}
	_ = file.Close()
	reopened, err := Open(dir, cfg)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	if reopened.Len() != 100 {
		t.Fatalf("reopened Len = %d, want 100", reopened.Len())
	}
}

// A shard file that is missing entirely (manifest references it) is corrupt.
func TestMissingShardCorrupt(t *testing.T) {
	dir := t.TempDir()
	cfg := testConfig()
	st := buildCheckpointed(t, dir, cfg, 100)
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := os.Remove(shardPath(dir, 0)); err != nil {
		t.Fatalf("remove shard: %v", err)
	}
	if _, err := Open(dir, cfg); err == nil {
		t.Fatal("Open accepted a missing shard")
	}
}
