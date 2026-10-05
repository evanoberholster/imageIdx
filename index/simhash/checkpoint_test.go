package simhash

import (
	"os"
	"path/filepath"
	"testing"
)

func journalSize(dir string) (int64, error) {
	fi, err := os.Stat(filepath.Join(dir, journalFilename))
	if err != nil {
		return 0, err
	}
	return fi.Size(), nil
}

// TestCheckpointMetadataBeforeManifest simulates a crash after shard metadata is
// written and flushed but before the manifest is published. The manifest stays
// authoritative (the newer shard slot is ignored) and WAL replay completes the
// update.
func TestCheckpointMetadataBeforeManifest(t *testing.T) {
	dir := t.TempDir()
	cfg := testConfig()
	st, err := Open(dir, cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := st.AppendBatch(randRecords(100, 8)); err != nil {
		t.Fatalf("AppendBatch: %v", err)
	}
	if err := st.Checkpoint(); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	before := st.Generation()
	target := randRecords(101, 1)[0]
	if err := st.Upsert(2, target); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	// Crash after metadata write+flush, before manifest publication.
	for _, sh := range st.shards {
		sh.generation = st.generation
		if err := sh.writeMeta(); err != nil {
			t.Fatalf("writeMeta: %v", err)
		}
		if err := sh.mapped.Sync(); err != nil {
			t.Fatalf("sync: %v", err)
		}
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	reopened, err := Open(dir, cfg)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	if got, ok := reopened.Get(2); !ok || got != target {
		t.Fatalf("Get(2) = %+v, want %+v", got, target)
	}
	if reopened.Generation() != before+1 {
		t.Fatalf("generation = %d, want %d", reopened.Generation(), before+1)
	}
}

// TestCheckpointManifestBeforeWALReset simulates a crash after the manifest is
// published but before the WAL is truncated. Replay must skip the already
// committed batch (idempotent) and keep the manifest generation.
func TestCheckpointManifestBeforeWALReset(t *testing.T) {
	dir := t.TempDir()
	cfg := testConfig()
	st, err := Open(dir, cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := st.AppendBatch(randRecords(102, 8)); err != nil {
		t.Fatalf("AppendBatch: %v", err)
	}
	if err := st.Checkpoint(); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	before := st.Generation()
	target := randRecords(103, 1)[0]
	if err := st.Upsert(2, target); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	for _, sh := range st.shards {
		sh.generation = st.generation
		if err := sh.writeMeta(); err != nil {
			t.Fatalf("writeMeta: %v", err)
		}
		if err := sh.mapped.Sync(); err != nil {
			t.Fatalf("sync: %v", err)
		}
	}
	if err := st.writeManifestLocked(); err != nil {
		t.Fatalf("manifest: %v", err)
	}
	if err := st.Close(); err != nil { // WAL not reset
		t.Fatalf("Close: %v", err)
	}
	reopened, err := Open(dir, cfg)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	if got, ok := reopened.Get(2); !ok || got != target {
		t.Fatalf("Get(2) = %+v, want %+v", got, target)
	}
	if reopened.Generation() != before+1 {
		t.Fatalf("generation = %d, want %d", reopened.Generation(), before+1)
	}
}

func TestCheckpointResetsWAL(t *testing.T) {
	dir := t.TempDir()
	cfg := testConfig()
	st, err := Open(dir, cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := st.AppendBatch(randRecords(104, 8)); err != nil {
		t.Fatalf("AppendBatch: %v", err)
	}
	if err := st.Upsert(2, randRecords(105, 1)[0]); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if err := st.Checkpoint(); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	fi, err := os.Stat(filepath.Join(dir, journalFilename))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if fi.Size() != 0 {
		t.Fatalf("WAL size after checkpoint = %d, want 0", fi.Size())
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// TestRecoveryIdempotent reopens twice and checkpoints between, asserting the
// same visible state.
func TestRecoveryIdempotent(t *testing.T) {
	dir := t.TempDir()
	cfg := testConfig()
	st, err := Open(dir, cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := st.AppendBatch(randRecords(106, 16)); err != nil {
		t.Fatalf("AppendBatch: %v", err)
	}
	if err := st.Checkpoint(); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	target := randRecords(107, 1)[0]
	if err := st.UpsertBatch([]Update{{ImageID: 2, Record: target}, {ImageID: 9, Record: randRecords(108, 1)[0]}}); err != nil {
		t.Fatalf("UpsertBatch: %v", err)
	}
	if err := st.Close(); err != nil { // no checkpoint: WAL replays on open
		t.Fatalf("Close: %v", err)
	}
	first, err := Open(dir, cfg)
	if err != nil {
		t.Fatalf("reopen1: %v", err)
	}
	wantGen := first.Generation()
	want2, _ := first.Get(2)
	if err := first.Checkpoint(); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("close1: %v", err)
	}
	second, err := Open(dir, cfg)
	if err != nil {
		t.Fatalf("reopen2: %v", err)
	}
	defer second.Close()
	if second.Generation() != wantGen {
		t.Fatalf("generation drifted: %d != %d", second.Generation(), wantGen)
	}
	if got, _ := second.Get(2); got != want2 || got != target {
		t.Fatalf("Get(2) = %+v, want %+v", got, target)
	}
}
