package vectoridx

import (
	"context"
	"errors"
	"math/rand"
	"os"
	"path/filepath"
	"testing"
	"time"

	schema "github.com/evanoberholster/imageIdx/schema"
)

func tempPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "vec.idx")
}

func TestCreateCheckpointReopen(t *testing.T) {
	path := tempPath(t)
	idx, err := NewStore(path, testConfigFloat512(), DurabilitySync)
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
	reopened, err := OpenStore(path, DurabilitySync)
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
		if len(results) != 1 || results[0].ValueID != uint64(id) {
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
	idx, err := NewStore(path, testConfigFloat512(), DurabilitySync)
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
	reopened, err := OpenStore(path, DurabilitySync)
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
	if len(results) != 1 || results[0].ValueID != 2 {
		t.Fatalf("WAL-replayed search = %+v", results)
	}
	if got := reopened.TombstoneCount(); got != 1 {
		t.Fatalf("TombstoneCount after replay = %d, want 1", got)
	}
}

func TestCrashTornTailRecovery(t *testing.T) {
	path := tempPath(t)
	idx, err := NewStore(path, testConfigFloat512(), DurabilitySync)
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
	// Simulate a crash mid-append: tear the last byte of the active WAL
	// segment's tail.
	segs, err := listWALSegments(path)
	if err != nil || len(segs) == 0 {
		t.Fatalf("list WAL segments: %v (%d)", err, len(segs))
	}
	walPath := segs[len(segs)-1].path
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
	reopened, err := OpenStore(path, DurabilitySync)
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
	again, err := OpenStore(path, DurabilitySync)
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
	idx, err := NewStore(path, testConfigFloat512(), DurabilitySync)
	if err != nil {
		t.Fatal(err)
	}
	mustAddFloat(t, idx, 1, randFloatVec(rand.New(rand.NewSource(54)), Dim512))
	if err := idx.Vacuum(); err != nil {
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
	if _, err := OpenStore(path, DurabilitySync); !errors.Is(err, ErrCorruptStorage) {
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
	if _, err := OpenStore(path, DurabilitySync); !errors.Is(err, ErrCorruptStorage) {
		t.Fatalf("open bad-magic file = %v, want ErrCorruptStorage", err)
	}
}

func TestCompactPurgesTombstonesPreservesIDs(t *testing.T) {
	path := tempPath(t)
	idx, err := NewStore(path, testConfigFloat512(), DurabilitySync)
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
		if len(results) != 1 || results[0].ValueID != uint64(id) {
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
	reopened, err := OpenStore(path, DurabilitySync)
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
		if len(results) != 1 || results[0].ValueID != uint64(id) {
			t.Fatalf("node %d after reopen post-compact = %+v", id, results)
		}
	}
}

func TestOpenMissingFile(t *testing.T) {
	if _, err := OpenStore(filepath.Join(t.TempDir(), "nope.idx"), DurabilitySync); !os.IsNotExist(err) {
		t.Fatalf("open missing = %v, want not-exist", err)
	}
}

func TestCheckpointInMemoryFails(t *testing.T) {
	idx, err := NewMemory(testConfigFloat512())
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
	idx, err := NewStore(path, testConfigFloat512(), DurabilitySync)
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
	reopened, err := OpenStore(path, DurabilitySync)
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
	if len(results) != 1 || results[0].ValueID != 5 || results[0].Score != 0 {
		t.Fatalf("re-added node after reopen = %+v", results)
	}
}

func TestDurabilityValidation(t *testing.T) {
	path := tempPath(t)
	if _, err := NewStore(path, testConfigFloat512(), Durability(200)); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("Create invalid durability = %v, want ErrInvalidConfig", err)
	}
	if _, err := OpenStore(path, Durability(200)); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("Open invalid durability = %v, want ErrInvalidConfig", err)
	}
}

func TestOpenDurabilityIsRuntimePolicy(t *testing.T) {
	path := tempPath(t)
	idx, err := NewStore(path, testConfigFloat512(), DurabilitySync)
	if err != nil {
		t.Fatal(err)
	}
	mustAddFloat(t, idx, 1, randFloatVec(rand.New(rand.NewSource(44)), Dim512))
	if err := idx.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenStore(path, DurabilityOS)
	if err != nil {
		t.Fatal(err)
	}
	mustAddFloat(t, reopened, 2, randFloatVec(rand.New(rand.NewSource(45)), Dim512))
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
	again, err := OpenStore(path, DurabilitySync)
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
	first, err := NewStore(path, testConfigFloat512(), DurabilitySync)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := OpenStore(path, DurabilitySync); !errors.Is(err, ErrStoreBusy) {
		t.Fatalf("second Open = %v, want ErrStoreBusy", err)
	}
	if _, err := NewStore(path, testConfigFloat512(), DurabilitySync); !errors.Is(err, ErrStoreBusy) {
		t.Fatalf("second Create = %v, want ErrStoreBusy", err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := OpenStore(path, DurabilitySync)
	if err != nil {
		t.Fatalf("Open after Close: %v", err)
	}
	// Distinct index paths in the same directory take distinct locks.
	other, err := NewStore(filepath.Join(dir, "other.idx"), testConfigFloat512(), DurabilitySync)
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
	idx, err := NewStore(path, testConfigFloat512(), DurabilitySync)
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
	idx, err := NewStore(path, testConfigFloat512(), DurabilityOS, WithSyncInterval(5*time.Millisecond))
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
	reopened, err := OpenStore(path, DurabilityOS)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if got := reopened.Stats().LiveCount; got != 20 {
		t.Fatalf("LiveCount after reopen = %d, want 20", got)
	}
}

func TestSyncInMemoryReturnsNotPersistent(t *testing.T) {
	idx, err := NewMemory(testConfigFloat512())
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()
	if err := idx.Sync(); !errors.Is(err, ErrNotPersistent) {
		t.Fatalf("in-memory Sync = %v, want ErrNotPersistent", err)
	}
}

func TestCreateRejectsNegativeSyncInterval(t *testing.T) {
	if _, err := NewStore(tempPath(t), testConfigFloat512(), DurabilityOS, WithSyncInterval(-time.Second)); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("negative sync interval = %v, want ErrInvalidConfig", err)
	}
}

func TestWALSegmentRotationAndReplay(t *testing.T) {
	path := tempPath(t)
	// One 512-d float32 record frames to ~2KB, so 16 KiB segments rotate
	// every handful of writes.
	idx, err := NewStore(path, testConfigFloat512(), DurabilitySync, WithWALSegmentBytes(16<<10))
	if err != nil {
		t.Fatal(err)
	}
	rng := rand.New(rand.NewSource(63))
	vecs := make(map[NodeID][]float32, 60)
	for i := NodeID(1); i <= 60; i++ {
		vec := randFloatVec(rng, Dim512)
		vecs[i] = vec
		mustAddFloat(t, idx, i, vec)
	}
	st := idx.Stats()
	if st.WALSegments < 3 {
		t.Fatalf("WALSegments = %d, want >= 3", st.WALSegments)
	}
	if st.WALBytes == 0 || st.WALBytesWritten == 0 {
		t.Fatalf("WAL byte counters = %d/%d", st.WALBytes, st.WALBytesWritten)
	}
	if err := idx.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenStore(path, DurabilitySync, WithWALSegmentBytes(16<<10))
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if got := reopened.LiveCount(); got != 60 {
		t.Fatalf("LiveCount after segmented replay = %d, want 60", got)
	}
	for id, vec := range vecs {
		results, err := reopened.SearchFloat32(vec, nil, 1)
		if err != nil {
			t.Fatal(err)
		}
		if len(results) != 1 || results[0].ValueID != schema.ValueID(id) {
			t.Fatalf("node %d self-search after segmented replay = %+v", id, results)
		}
	}
}

func TestWALCheckpointRemovesOldSegments(t *testing.T) {
	path := tempPath(t)
	idx, err := NewStore(path, testConfigFloat512(), DurabilitySync, WithWALSegmentBytes(16<<10))
	if err != nil {
		t.Fatal(err)
	}
	rng := rand.New(rand.NewSource(64))
	for i := NodeID(1); i <= 60; i++ {
		mustAddFloat(t, idx, i, randFloatVec(rng, Dim512))
	}
	if st := idx.Stats(); st.WALSegments < 3 {
		t.Fatalf("WALSegments before checkpoint = %d, want >= 3", st.WALSegments)
	}
	if err := idx.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	st := idx.Stats()
	if st.WALSegments != 1 {
		t.Fatalf("WALSegments after checkpoint = %d, want 1", st.WALSegments)
	}
	if st.WALBytes != 0 {
		t.Fatalf("WALBytes after checkpoint = %d, want 0", st.WALBytes)
	}
	if err := idx.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenStore(path, DurabilitySync)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if got := reopened.LiveCount(); got != 60 {
		t.Fatalf("LiveCount after checkpoint cleanup = %d, want 60", got)
	}
}

func TestLegacyWALMigration(t *testing.T) {
	path := tempPath(t)
	idx, err := NewStore(path, testConfigFloat512(), DurabilitySync)
	if err != nil {
		t.Fatal(err)
	}
	rng := rand.New(rand.NewSource(65))
	for i := NodeID(1); i <= 3; i++ {
		mustAddFloat(t, idx, i, randFloatVec(rng, Dim512))
	}
	if err := idx.Close(); err != nil {
		t.Fatal(err)
	}
	segs, err := listWALSegments(path)
	if err != nil || len(segs) != 1 {
		t.Fatalf("segments = %v (%v), want 1", segs, err)
	}
	legacy := path + legacyWALSuffix
	if err := os.Rename(segs[0].path, legacy); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenStore(path, DurabilitySync)
	if err != nil {
		t.Fatalf("Open with legacy WAL: %v", err)
	}
	if got := reopened.LiveCount(); got != 3 {
		t.Fatalf("LiveCount after legacy migration = %d, want 3", got)
	}
	if reopened.Stats().WALSegments < 1 {
		t.Fatalf("no segmented WAL after migration")
	}
	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		t.Fatalf("legacy WAL still present: %v", err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestWALTornOlderSegmentIsCorruption(t *testing.T) {
	path := tempPath(t)
	idx, err := NewStore(path, testConfigFloat512(), DurabilityOS, WithWALSegmentBytes(16<<10))
	if err != nil {
		t.Fatal(err)
	}
	rng := rand.New(rand.NewSource(66))
	for i := NodeID(1); i <= 60; i++ {
		mustAddFloat(t, idx, i, randFloatVec(rng, Dim512))
	}
	if err := idx.Close(); err != nil {
		t.Fatal(err)
	}
	segs, err := listWALSegments(path)
	if err != nil || len(segs) < 3 {
		t.Fatalf("segments = %d (%v), want >= 3", len(segs), err)
	}
	first := segs[0]
	if err := os.Truncate(first.path, first.size-1); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenStore(path, DurabilityOS); !errors.Is(err, ErrCorruptWAL) {
		t.Fatalf("Open with torn older segment = %v, want ErrCorruptWAL", err)
	}
}

func TestCheckpointWritesDeltaWithoutBaseRewrite(t *testing.T) {
	path := tempPath(t)
	idx, err := NewStore(path, testConfigFloat512(), DurabilitySync)
	if err != nil {
		t.Fatal(err)
	}
	rng := rand.New(rand.NewSource(67))
	mustAddFloat(t, idx, 1, randFloatVec(rng, Dim512))
	baseBefore, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := idx.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	baseAfter, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if baseAfter.Size() != baseBefore.Size() {
		t.Fatalf("base rewritten by delta checkpoint: %d -> %d", baseBefore.Size(), baseAfter.Size())
	}
	if st := idx.Stats(); st.Deltas != 1 || st.DeltaRecords != 1 || st.DeltaBytes == 0 {
		t.Fatalf("delta stats after first checkpoint = %+v", st)
	}
	mustAddFloat(t, idx, 2, randFloatVec(rng, Dim512))
	if err := idx.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	if st := idx.Stats(); st.Deltas != 2 {
		t.Fatalf("Deltas = %d, want 2", st.Deltas)
	}
	if err := idx.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenStore(path, DurabilitySync)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if got := reopened.LiveCount(); got != 2 {
		t.Fatalf("LiveCount after delta reopen = %d, want 2", got)
	}
	if err := reopened.Verify(); err != nil {
		t.Fatalf("Verify after delta reopen: %v", err)
	}
}

func TestDeltaCheckpointCarriesDeletesAndUpdates(t *testing.T) {
	path := tempPath(t)
	idx, err := NewStore(path, testConfigFloat512(), DurabilitySync)
	if err != nil {
		t.Fatal(err)
	}
	rng := rand.New(rand.NewSource(68))
	for i := NodeID(1); i <= 20; i++ {
		mustAddFloat(t, idx, i, randFloatVec(rng, Dim512))
	}
	for i := NodeID(1); i <= 5; i++ {
		if err := idx.Delete(i); err != nil {
			t.Fatal(err)
		}
	}
	updated := randFloatVec(rng, Dim512)
	if err := idx.Delete(6); err != nil {
		t.Fatal(err)
	}
	mustAddFloat(t, idx, 6, updated)
	if err := idx.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	if st := idx.Stats(); st.DeltaTombstones != 5 || st.DeltaRecords != 15 {
		t.Fatalf("delta stats = %+v, want 15 puts / 5 tombstones", st)
	}
	mustAddFloat(t, idx, 21, randFloatVec(rng, Dim512))
	if err := idx.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenStore(path, DurabilitySync)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if got := reopened.LiveCount(); got != 16 {
		t.Fatalf("LiveCount = %d, want 16", got)
	}
	if err := reopened.Verify(); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	results, err := reopened.SearchFloat32(updated, nil, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].ValueID != 6 || results[0].Score != 0 {
		t.Fatalf("re-added node 6 after delta reopen = %+v", results)
	}
}

func TestVacuumMergesDeltas(t *testing.T) {
	path := tempPath(t)
	idx, err := NewStore(path, testConfigFloat512(), DurabilitySync)
	if err != nil {
		t.Fatal(err)
	}
	rng := rand.New(rand.NewSource(69))
	for i := NodeID(1); i <= 10; i++ {
		mustAddFloat(t, idx, i, randFloatVec(rng, Dim512))
	}
	if err := idx.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	if err := idx.Delete(1); err != nil {
		t.Fatal(err)
	}
	if err := idx.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	if st := idx.Stats(); st.Deltas != 2 {
		t.Fatalf("Deltas = %d, want 2", st.Deltas)
	}
	baseBefore, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := idx.Vacuum(); err != nil {
		t.Fatal(err)
	}
	st := idx.Stats()
	if st.Deltas != 0 || st.DeltaBytes != 0 {
		t.Fatalf("stats after vacuum = %+v", st)
	}
	baseAfter, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if baseAfter.Size() <= baseBefore.Size() {
		t.Fatalf("base did not grow on vacuum: %d -> %d", baseBefore.Size(), baseAfter.Size())
	}
	files, err := filepath.Glob(path + ".delta.*")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 0 {
		t.Fatalf("delta files remain after vacuum: %v", files)
	}
	if err := idx.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenStore(path, DurabilitySync)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if got := reopened.LiveCount(); got != 9 {
		t.Fatalf("LiveCount after vacuum = %d, want 9", got)
	}
}

func TestOrphanDeltaIgnored(t *testing.T) {
	path := tempPath(t)
	idx, err := NewStore(path, testConfigFloat512(), DurabilitySync)
	if err != nil {
		t.Fatal(err)
	}
	rng := rand.New(rand.NewSource(70))
	for i := NodeID(1); i <= 3; i++ {
		mustAddFloat(t, idx, i, randFloatVec(rng, Dim512))
	}
	if err := idx.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	// Write a delta that the manifest does not name: a crash between delta
	// publication and manifest update leaves this behind.
	orphan := deltaName(path, 99)
	raw := make([]byte, Dim512*4)
	if err := os.WriteFile(orphan, encodeDeltaFile(99, []deltaRecord{{rtype: deltaTypePut, id: 999, raw: raw}}), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := idx.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenStore(path, DurabilitySync)
	if err != nil {
		t.Fatal(err)
	}
	if got := reopened.LiveCount(); got != 3 {
		t.Fatalf("LiveCount with orphan delta = %d, want 3", got)
	}
	if err := reopened.Vacuum(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Fatalf("orphan delta survived vacuum: %v", err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestCheckpointNoopWhenClean(t *testing.T) {
	path := tempPath(t)
	idx, err := NewStore(path, testConfigFloat512(), DurabilitySync)
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()
	mustAddFloat(t, idx, 1, randFloatVec(rand.New(rand.NewSource(71)), Dim512))
	if err := idx.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	if err := idx.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	if st := idx.Stats(); st.Deltas != 1 {
		t.Fatalf("Deltas after clean checkpoint = %d, want 1", st.Deltas)
	}
}

func TestManifestCorruptionDetected(t *testing.T) {
	path := tempPath(t)
	idx, err := NewStore(path, testConfigFloat512(), DurabilitySync)
	if err != nil {
		t.Fatal(err)
	}
	mustAddFloat(t, idx, 1, randFloatVec(rand.New(rand.NewSource(72)), Dim512))
	if err := idx.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	if err := idx.Close(); err != nil {
		t.Fatal(err)
	}
	manifestPath := path + ".manifest"
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	data[len(data)/2] ^= 0xff
	if err := os.WriteFile(manifestPath, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenStore(path, DurabilitySync); !errors.Is(err, ErrCorruptStorage) {
		t.Fatalf("Open with corrupt manifest = %v, want ErrCorruptStorage", err)
	}
}

func TestAutoVacuumByDeltaCount(t *testing.T) {
	path := tempPath(t)
	idx, err := NewStore(path, testConfigFloat512(), DurabilitySync)
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()
	if err := idx.SetAutoVacuum(2, 0); err != nil {
		t.Fatal(err)
	}
	rng := rand.New(rand.NewSource(73))
	for i := NodeID(1); i <= 5; i++ {
		mustAddFloat(t, idx, i, randFloatVec(rng, Dim512))
	}
	if err := idx.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	mustAddFloat(t, idx, 6, randFloatVec(rng, Dim512))
	if err := idx.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	if st := idx.Stats(); st.Deltas != 2 {
		t.Fatalf("Deltas before trigger = %d, want 2", st.Deltas)
	}
	// The next write observes two deltas and enqueues a vacuum.
	mustAddFloat(t, idx, 7, randFloatVec(rng, Dim512))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := idx.WaitIdle(ctx); err != nil {
		t.Fatal(err)
	}
	if st := idx.Stats(); st.Deltas != 0 || st.DeltaBytes != 0 {
		t.Fatalf("stats after auto vacuum = %+v", st)
	}
	if got := idx.LiveCount(); got != 7 {
		t.Fatalf("LiveCount after auto vacuum = %d, want 7", got)
	}
	if err := idx.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenStore(path, DurabilitySync)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if got := reopened.LiveCount(); got != 7 {
		t.Fatalf("LiveCount after auto vacuum reopen = %d, want 7", got)
	}
	if err := reopened.Verify(); err != nil {
		t.Fatalf("Verify after auto vacuum: %v", err)
	}
}

func TestAutoVacuumByFragmentation(t *testing.T) {
	path := tempPath(t)
	idx, err := NewStore(path, testConfigFloat512(), DurabilitySync)
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()
	rng := rand.New(rand.NewSource(74))
	for i := NodeID(1); i <= 10; i++ {
		mustAddFloat(t, idx, i, randFloatVec(rng, Dim512))
	}
	if err := idx.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	for i := NodeID(1); i <= 5; i++ {
		if err := idx.Delete(i); err != nil {
			t.Fatal(err)
		}
	}
	if err := idx.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	if st := idx.Stats(); st.DeltaFragmentation < 0.3 {
		t.Fatalf("DeltaFragmentation = %v, want >= 0.3", st.DeltaFragmentation)
	}
	if err := idx.SetAutoVacuum(0, 0.3); err != nil {
		t.Fatal(err)
	}
	mustAddFloat(t, idx, 11, randFloatVec(rng, Dim512))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := idx.WaitIdle(ctx); err != nil {
		t.Fatal(err)
	}
	if st := idx.Stats(); st.Deltas != 0 {
		t.Fatalf("Deltas after fragmentation vacuum = %d, want 0", st.Deltas)
	}
	if got := idx.LiveCount(); got != 6 {
		t.Fatalf("LiveCount = %d, want 6", got)
	}
}

func TestSetAutoVacuumValidation(t *testing.T) {
	idx, err := NewMemory(testConfigFloat512())
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()
	if err := idx.SetAutoVacuum(-1, 0); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("negative deltas = %v", err)
	}
	if err := idx.SetAutoVacuum(0, 1.5); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("bad fragmentation = %v", err)
	}
}
