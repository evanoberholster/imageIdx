package vectoridx

import (
	"context"
	"errors"
	"math/rand"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func tempPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "vec.idx")
}

func TestCreateCheckpointReopen(t *testing.T) {
	path := tempPath(t)
	idx, err := Create(path, testConfigFloat512(), DurabilitySync)
	if err != nil {
		t.Fatal(err)
	}
	rng := rand.New(rand.NewSource(51))
	vecs := make(map[NodeID][]float32)
	for i := NodeID(1); i <= 30; i++ {
		vec := randFloatVec(rng, Dim512)
		vecs[i] = vec
		mustAddFloat(t, idx, i, vec)
	}
	if err := idx.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	meta := idx.Metadata()
	if err := idx.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path, DurabilitySync)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if got := reopened.Metadata(); got.LiveCount != meta.LiveCount || got.Dimensions != meta.Dimensions {
		t.Fatalf("metadata after reopen = %+v, want %+v", got, meta)
	}
	for id, vec := range vecs {
		results, err := reopened.SearchFloat32(vec, nil, 1)
		if err != nil {
			t.Fatal(err)
		}
		if len(results) != 1 || results[0].ImageID != uint64(id) {
			t.Fatalf("node %d self-search after reopen = %+v", id, results)
		}
	}
	if err := reopened.Verify(); err != nil {
		t.Fatalf("Verify after reopen: %v", err)
	}
	if err := VerifyFile(path); err != nil {
		t.Fatalf("VerifyFile: %v", err)
	}
}

func TestWALReplayWithoutCheckpoint(t *testing.T) {
	path := tempPath(t)
	idx, err := Create(path, testConfigFloat512(), DurabilitySync)
	if err != nil {
		t.Fatal(err)
	}
	rng := rand.New(rand.NewSource(52))
	mustAddFloat(t, idx, 1, randFloatVec(rng, Dim512))
	if err := idx.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	// These two mutations live only in the WAL.
	vec2 := randFloatVec(rng, Dim512)
	mustAddFloat(t, idx, 2, vec2)
	if err := idx.Delete(1); err != nil {
		t.Fatal(err)
	}
	if err := idx.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path, DurabilitySync)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if got := reopened.LiveCount(); got != 1 {
		t.Fatalf("LiveCount after WAL replay = %d, want 1", got)
	}
	results, err := reopened.SearchFloat32(vec2, nil, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].ImageID != 2 {
		t.Fatalf("WAL-replayed search = %+v", results)
	}
	if got := reopened.TombstoneCount(); got != 1 {
		t.Fatalf("TombstoneCount after replay = %d, want 1", got)
	}
}

func TestCrashTornTailRecovery(t *testing.T) {
	path := tempPath(t)
	idx, err := Create(path, testConfigFloat512(), DurabilitySync)
	if err != nil {
		t.Fatal(err)
	}
	rng := rand.New(rand.NewSource(53))
	mustAddFloat(t, idx, 1, randFloatVec(rng, Dim512))
	if err := idx.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	vec2 := randFloatVec(rng, Dim512)
	mustAddFloat(t, idx, 2, vec2)
	if err := idx.Close(); err != nil {
		t.Fatal(err)
	}
	// Simulate a crash mid-append: tear the last byte of the WAL tail.
	walPath := path + ".wal"
	info, err := os.Stat(walPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() == 0 {
		t.Fatalf("expected WAL bytes after un-checkpointed add")
	}
	if err := os.Truncate(walPath, info.Size()-1); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path, DurabilitySync)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	// Prefix recovery: node 1 (checkpointed) must exist; index must verify.
	if err := reopened.Verify(); err != nil {
		t.Fatalf("Verify after torn-tail recovery: %v", err)
	}
	if got := reopened.LiveCount(); got < 1 || got > 2 {
		t.Fatalf("LiveCount after torn-tail recovery = %d, want 1 or 2", got)
	}
	// A second open must be deterministic (tail was truncated, not re-torn).
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
	again, err := Open(path, DurabilitySync)
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	if got, want := again.LiveCount(), reopened.LiveCount(); got != want {
		t.Fatalf("second reopen LiveCount = %d, want %d", got, want)
	}
}

func TestCorruptFileDetected(t *testing.T) {
	path := tempPath(t)
	idx, err := Create(path, testConfigFloat512(), DurabilitySync)
	if err != nil {
		t.Fatal(err)
	}
	mustAddFloat(t, idx, 1, randFloatVec(rand.New(rand.NewSource(54)), Dim512))
	if err := idx.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	if err := idx.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// Corrupt a vector payload byte (past header + directory).
	off := vectorHeaderSize + 4*regionEntrySize + 10
	if off >= len(data) {
		t.Fatalf("file too small to corrupt at %d (len %d)", off, len(data))
	}
	data[off] ^= 0xff
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path, DurabilitySync); !errors.Is(err, ErrCorruptStorage) {
		t.Fatalf("open corrupt file = %v, want ErrCorruptStorage", err)
	}
	if err := VerifyFile(path); !errors.Is(err, ErrCorruptStorage) {
		t.Fatalf("VerifyFile corrupt = %v, want ErrCorruptStorage", err)
	}
	// Bad magic is also corruption.
	copy(data[:8], []byte("NOTAVEC!"))
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path, DurabilitySync); !errors.Is(err, ErrCorruptStorage) {
		t.Fatalf("open bad-magic file = %v, want ErrCorruptStorage", err)
	}
}

func TestCompactPurgesTombstonesPreservesIDs(t *testing.T) {
	path := tempPath(t)
	idx, err := Create(path, testConfigFloat512(), DurabilitySync)
	if err != nil {
		t.Fatal(err)
	}
	rng := rand.New(rand.NewSource(55))
	live := make(map[NodeID][]float32)
	for i := NodeID(1); i <= 40; i++ {
		vec := randFloatVec(rng, Dim512)
		mustAddFloat(t, idx, i, vec)
		if i%2 == 1 {
			live[i] = vec
		}
	}
	for i := NodeID(2); i <= 40; i += 2 {
		if err := idx.Delete(i); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := idx.Compact(); err != nil {
		t.Fatal(err)
	}
	if got := idx.TombstoneCount(); got != 0 {
		t.Fatalf("TombstoneCount after compact = %d, want 0", got)
	}
	if got := idx.LiveCount(); got != 20 {
		t.Fatalf("LiveCount after compact = %d, want 20", got)
	}
	// NodeIDs are preserved exactly (critical invariant 4).
	for id, vec := range live {
		results, err := idx.SearchFloat32(vec, nil, 1)
		if err != nil {
			t.Fatal(err)
		}
		if len(results) != 1 || results[0].ImageID != uint64(id) {
			t.Fatalf("live node %d after compact = %+v", id, results)
		}
	}
	if err := idx.Verify(); err != nil {
		t.Fatalf("Verify after compact: %v", err)
	}
	genBefore := idx.Generation()
	if err := idx.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path, DurabilitySync)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if got := reopened.LiveCount(); got != 20 {
		t.Fatalf("LiveCount after reopen post-compact = %d, want 20", got)
	}
	if reopened.Generation() < genBefore {
		t.Fatalf("generation regressed across reopen: %d < %d", reopened.Generation(), genBefore)
	}
	for id, vec := range live {
		results, err := reopened.SearchFloat32(vec, nil, 1)
		if err != nil {
			t.Fatal(err)
		}
		if len(results) != 1 || results[0].ImageID != uint64(id) {
			t.Fatalf("node %d after reopen post-compact = %+v", id, results)
		}
	}
}

func TestOpenMissingFile(t *testing.T) {
	if _, err := Open(filepath.Join(t.TempDir(), "nope.idx"), DurabilitySync); !os.IsNotExist(err) {
		t.Fatalf("open missing = %v, want not-exist", err)
	}
}

func TestCheckpointInMemoryFails(t *testing.T) {
	idx, err := New(testConfigFloat512())
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()
	if err := idx.Checkpoint(); !errors.Is(err, ErrNotPersistent) {
		t.Fatalf("in-memory Checkpoint = %v, want ErrNotPersistent", err)
	}
}

func TestReAddAfterDeleteReopen(t *testing.T) {
	path := tempPath(t)
	idx, err := Create(path, testConfigFloat512(), DurabilitySync)
	if err != nil {
		t.Fatal(err)
	}
	rng := rand.New(rand.NewSource(43))
	for i := NodeID(1); i <= 24; i++ {
		mustAddFloat(t, idx, i, randFloatVec(rng, Dim512))
	}
	if err := idx.Delete(5); err != nil {
		t.Fatal(err)
	}
	updated := randFloatVec(rng, Dim512)
	mustAddFloat(t, idx, 5, updated)
	if err := idx.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path, DurabilitySync)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if got := reopened.LiveCount(); got != 24 {
		t.Fatalf("LiveCount after reopen = %d, want 24", got)
	}
	if err := reopened.Verify(); err != nil {
		t.Fatalf("Verify after reopen: %v", err)
	}
	results, err := reopened.SearchFloat32(updated, nil, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].ImageID != 5 || results[0].Score != 0 {
		t.Fatalf("re-added node after reopen = %+v", results)
	}
}

func TestDurabilityValidation(t *testing.T) {
	path := tempPath(t)
	if _, err := Create(path, testConfigFloat512(), Durability(200)); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("Create invalid durability = %v, want ErrInvalidConfig", err)
	}
	if _, err := Open(path, Durability(200)); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("Open invalid durability = %v, want ErrInvalidConfig", err)
	}
}

func TestOpenDurabilityIsRuntimePolicy(t *testing.T) {
	path := tempPath(t)
	idx, err := Create(path, testConfigFloat512(), DurabilitySync)
	if err != nil {
		t.Fatal(err)
	}
	mustAddFloat(t, idx, 1, randFloatVec(rand.New(rand.NewSource(44)), Dim512))
	if err := idx.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path, DurabilityOS)
	if err != nil {
		t.Fatal(err)
	}
	mustAddFloat(t, reopened, 2, randFloatVec(rand.New(rand.NewSource(45)), Dim512))
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
	again, err := Open(path, DurabilitySync)
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	if got := again.LiveCount(); got != 2 {
		t.Fatalf("LiveCount after OS-durability reopen = %d, want 2", got)
	}
}

func TestStoreLock(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "vec.idx")
	first, err := Create(path, testConfigFloat512(), DurabilitySync)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path, DurabilitySync); !errors.Is(err, ErrStoreBusy) {
		t.Fatalf("second Open = %v, want ErrStoreBusy", err)
	}
	if _, err := Create(path, testConfigFloat512(), DurabilitySync); !errors.Is(err, ErrStoreBusy) {
		t.Fatalf("second Create = %v, want ErrStoreBusy", err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := Open(path, DurabilitySync)
	if err != nil {
		t.Fatalf("Open after Close: %v", err)
	}
	// Distinct index paths in the same directory take distinct locks.
	other, err := Create(filepath.Join(dir, "other.idx"), testConfigFloat512(), DurabilitySync)
	if err != nil {
		t.Fatalf("Create sibling index: %v", err)
	}
	if err := other.Close(); err != nil {
		t.Fatal(err)
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestStatsAndFileSize(t *testing.T) {
	path := tempPath(t)
	idx, err := Create(path, testConfigFloat512(), DurabilitySync)
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()
	rng := rand.New(rand.NewSource(61))
	for i := NodeID(1); i <= 10; i++ {
		mustAddFloat(t, idx, i, randFloatVec(rng, Dim512))
	}
	st := idx.Stats()
	if st.LiveCount != 10 || st.TombstoneCount != 0 {
		t.Fatalf("counts = %d/%d, want 10/0", st.LiveCount, st.TombstoneCount)
	}
	if st.FileBytes == 0 || st.WALBytes == 0 {
		t.Fatalf("file sizes = %d/%d, want both > 0", st.FileBytes, st.WALBytes)
	}
	if st.TotalBytes != st.FileBytes+st.WALBytes {
		t.Fatalf("TotalBytes = %d, want %d", st.TotalBytes, st.FileBytes+st.WALBytes)
	}
	if st.WALRecords != 10 || st.WALBytesWritten == 0 {
		t.Fatalf("WAL counters = %d/%d", st.WALRecords, st.WALBytesWritten)
	}
	if st.WALSegments != 1 {
		t.Fatalf("WALSegments = %d, want 1", st.WALSegments)
	}
	if st.Generation == 0 {
		t.Fatal("Generation is zero")
	}
	if st.SyncInterval != 0 {
		t.Fatalf("SyncInterval = %s, want 0", st.SyncInterval)
	}
	size, err := idx.FileSize()
	if err != nil {
		t.Fatal(err)
	}
	if uint64(size) != st.TotalBytes {
		t.Fatalf("FileSize = %d, Stats.TotalBytes = %d", size, st.TotalBytes)
	}
}

func TestSyncIntervalGroupCommit(t *testing.T) {
	path := tempPath(t)
	idx, err := Create(path, testConfigFloat512(), DurabilityOS, WithSyncInterval(5*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	rng := rand.New(rand.NewSource(62))
	for i := NodeID(1); i <= 20; i++ {
		idx.AddAsync(i, randFloatVec(rng, Dim512))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := idx.WaitIdle(ctx); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for idx.Stats().WALSyncs == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if st := idx.Stats(); st.WALSyncs == 0 {
		t.Fatalf("group commit did not fsync: %+v", st)
	}
	if err := idx.Sync(); err != nil {
		t.Fatalf("explicit Sync: %v", err)
	}
	if err := idx.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path, DurabilityOS)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if got := reopened.Stats().LiveCount; got != 20 {
		t.Fatalf("LiveCount after reopen = %d, want 20", got)
	}
}

func TestSyncInMemoryReturnsNotPersistent(t *testing.T) {
	idx, err := New(testConfigFloat512())
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()
	if err := idx.Sync(); !errors.Is(err, ErrNotPersistent) {
		t.Fatalf("in-memory Sync = %v, want ErrNotPersistent", err)
	}
}

func TestCreateRejectsNegativeSyncInterval(t *testing.T) {
	if _, err := Create(tempPath(t), testConfigFloat512(), DurabilityOS, WithSyncInterval(-time.Second)); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("negative sync interval = %v, want ErrInvalidConfig", err)
	}
}
