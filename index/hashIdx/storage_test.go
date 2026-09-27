package hashidx

import (
	"encoding/binary"
	"errors"
	"hash/crc32"
	"math"
	"os"
	"path/filepath"
	"testing"
)

func mustNewPersistent(t *testing.T, dir string, cfg Config) *Index {
	t.Helper()
	idx, err := newIndex(dir, testPerceptualSpec(cfg), DurabilityOS)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = idx.Close() })
	return idx
}

func mustOpenPersistent(t *testing.T, dir string, cfg Config) *Index {
	t.Helper()
	idx, err := openIndex(dir, cfg, DurabilityOS)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = idx.Close() })
	return idx
}

func seedEntries(count int) []Entry {
	entries := make([]Entry, count)
	for k := range entries {
		entries[k] = entryFromSeed(uint64(k))
	}
	return entries
}

// walFiles returns the WAL files in an index directory.
func walFiles(t *testing.T, dir string) []string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, walGlob))
	if err != nil {
		t.Fatal(err)
	}
	return matches
}

// activeWAL returns the WAL file referenced by the current manifest.
func activeWAL(t *testing.T, dir string) string {
	t.Helper()
	manifest, err := readManifest(filepath.Join(dir, "manifest"))
	if err != nil {
		t.Fatal(err)
	}
	return walPath(dir, manifest.walEpoch)
}

func TestPersistentCheckpointReopen(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{RecordsPerPage: 64}
	idx := mustNewPersistent(t, dir, cfg)
	entries := seedEntries(200)
	if err := idx.Apply(entries[:100]); err != nil {
		t.Fatal(err)
	}
	if err := idx.Apply(entries[100:]); err != nil {
		t.Fatal(err)
	}
	if err := idx.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	if err := idx.Close(); err != nil {
		t.Fatal(err)
	}

	reopened := mustOpenPersistent(t, dir, cfg)
	metadata := reopened.Metadata()
	if metadata.RecordCount != 200 || metadata.RecordsPerPage != 64 {
		t.Fatalf("metadata = %+v", metadata)
	}
	if metadata.PageCount != 4 { // 3 sealed pages + 8-record active
		t.Fatalf("page count = %d, want 4", metadata.PageCount)
	}
	for k, want := range entries {
		got, ok := reopened.EntryAt(Ordinal(k))
		if !ok || got != want {
			t.Fatalf("ordinal %d = %+v (present %v), want %+v", k, got, ok, want)
		}
		query := want.PDQ
		dst := reopened.SearchPDQRadius(nil, query, 0, nil)
		if len(dst) == 0 || dst[0].ImageID != want.ImageID {
			t.Fatalf("exact PDQ search for id %d = %v", want.ImageID, dst)
		}
	}
	// A zero config adopts the persisted page size.
	adopted := mustOpenPersistent(t, dir, Config{})
	if got := adopted.Metadata().RecordsPerPage; got != 64 {
		t.Fatalf("adopted records per page = %d", got)
	}
	// A conflicting explicit page size is rejected.
	if _, err := openIndex(dir, Config{RecordsPerPage: 128}, DurabilityOS); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("mismatched page size error = %v", err)
	}
}

func TestWALReplayWithoutCheckpoint(t *testing.T) {
	dir := t.TempDir()
	idx := mustNewPersistent(t, dir, Config{RecordsPerPage: 64})
	entries := seedEntries(100)
	if err := idx.Apply(entries); err != nil {
		t.Fatal(err)
	}
	// Close without checkpoint: recovery must come from the WAL tail.
	if err := idx.Close(); err != nil {
		t.Fatal(err)
	}
	if len(walFiles(t, dir)) != 1 {
		t.Fatalf("wal files = %v", walFiles(t, dir))
	}
	reopened := mustOpenPersistent(t, dir, Config{})
	if got := reopened.Metadata().RecordCount; got != 100 {
		t.Fatalf("replayed record count = %d", got)
	}
	for k, want := range entries {
		got, ok := reopened.EntryAt(Ordinal(k))
		if !ok || got != want {
			t.Fatalf("replayed ordinal %d = %+v, want %+v", k, got, want)
		}
	}
}

func TestIncompleteTrailingWALBatch(t *testing.T) {
	dir := t.TempDir()
	idx := mustNewPersistent(t, dir, Config{RecordsPerPage: 64})
	first := seedEntries(40)
	if err := idx.Apply(first); err != nil {
		t.Fatal(err)
	}
	if err := idx.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	second := make([]Entry, 10)
	for k := range second {
		second[k] = entryFromSeed(uint64(1000 + k))
	}
	if err := idx.Apply(second); err != nil {
		t.Fatal(err)
	}
	wal := activeWAL(t, dir)
	info, err := os.Stat(wal)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() == 0 {
		t.Fatal("expected a non-empty wal before truncation")
	}
	// Cut the trailing frame in half: recovery must keep the checkpoint and
	// ignore the incomplete batch.
	if err := os.Truncate(wal, info.Size()-1); err != nil {
		t.Fatal(err)
	}
	if err := idx.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := mustOpenPersistent(t, dir, Config{})
	if got := reopened.Metadata().RecordCount; got != 40 {
		t.Fatalf("record count after truncated tail = %d, want 40", got)
	}
	if _, ok := reopened.EntryAt(40); ok {
		t.Fatal("truncated batch was partially replayed")
	}
}

func TestCorruptWALRejected(t *testing.T) {
	dir := t.TempDir()
	idx := mustNewPersistent(t, dir, Config{RecordsPerPage: 64})
	if err := idx.Apply(seedEntries(5)); err != nil {
		t.Fatal(err)
	}
	wal := activeWAL(t, dir)
	if err := idx.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(wal)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) < walHeaderSize+8 {
		t.Fatalf("wal too small: %d", len(data))
	}
	data[walHeaderSize] ^= 0xff
	if err := os.WriteFile(wal, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := openIndex(dir, Config{}, DurabilityOS); !errors.Is(err, ErrCorruptWAL) {
		t.Fatalf("corrupt wal error = %v, want ErrCorruptWAL", err)
	}
}

func TestCorruptManifestRejected(t *testing.T) {
	dir := t.TempDir()
	idx := mustNewPersistent(t, dir, Config{})
	if err := idx.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "manifest")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	data[10] ^= 0xff
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := openIndex(dir, Config{}, DurabilityOS); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("corrupt manifest error = %v, want ErrCorrupt", err)
	}
}

// TestManifestVersionRejected proves unknown manifest versions fail open
// with ErrUnsupported instead of masquerading as corruption.
func TestManifestVersionRejected(t *testing.T) {
	bumpVersion := func(t *testing.T, path string) {
		t.Helper()
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		data[4] ^= 0xff // version field; magic stays recognized
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	dir := t.TempDir()
	idx := mustNewPersistent(t, dir, Config{})
	if err := idx.Close(); err != nil {
		t.Fatal(err)
	}
	bumpVersion(t, filepath.Join(dir, "manifest"))
	if _, err := openIndex(dir, Config{}, DurabilityOS); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("versioned manifest error = %v, want ErrUnsupported", err)
	}

	exactDir := persistentDir(t)
	exactIdx, err := newExact(exactDir, ExactSpec{Algorithm: AlgorithmSHA256}, ExactConfig{}, DurabilityOS)
	if err != nil {
		t.Fatal(err)
	}
	_ = exactIdx.Close()
	bumpVersion(t, filepath.Join(exactDir, "manifest"))
	if _, err := openExact(exactDir, ExactConfig{}, DurabilityOS); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("versioned exact manifest error = %v, want ErrUnsupported", err)
	}

	storeDir := t.TempDir()
	mustNewStore(t, storeDir, testStoreSpec())
	if pGen, eGen, err := ReadStoreManifest(storeDir); err != nil || pGen != 1 || eGen != 1 {
		t.Fatalf("fresh joint anchor = (%d, %d, %v), want (1, 1, nil)", pGen, eGen, err)
	}
	bumpVersion(t, filepath.Join(storeDir, "manifest"))
	if _, _, err := ReadStoreManifest(storeDir); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("versioned joint manifest error = %v, want ErrUnsupported", err)
	}
	// Restore a well-formed anchor before the corruption stage: the
	// version bump above persists in the file.
	if err := writeStoreManifest(storeDir, 1, 1); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(storeDir, "manifest"))
	if err != nil {
		t.Fatal(err)
	}
	data[10] ^= 0xff
	if err := os.WriteFile(filepath.Join(storeDir, "manifest"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ReadStoreManifest(storeDir); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("corrupt joint manifest error = %v, want ErrCorrupt", err)
	}
	if _, _, err := ReadStoreManifest(filepath.Join(storeDir, "missing")); !os.IsNotExist(err) {
		t.Fatalf("missing joint manifest error = %v, want not-exist", err)
	}
}

func TestCorruptGenerationHeaderRejected(t *testing.T) {
	dir := t.TempDir()
	idx := mustNewPersistent(t, dir, Config{RecordsPerPage: 32})
	if err := idx.Apply(seedEntries(20)); err != nil {
		t.Fatal(err)
	}
	if err := idx.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	generation := idx.Metadata().Generation
	if err := idx.Close(); err != nil {
		t.Fatal(err)
	}
	path := generationPath(dir, generation)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	data[9] ^= 0xff // generation field, covered by the header CRC
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := openIndex(dir, Config{}, DurabilityOS); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("corrupt generation header error = %v, want ErrCorrupt", err)
	}
}

func TestCorruptPagePayloadRejected(t *testing.T) {
	dir := t.TempDir()
	idx := mustNewPersistent(t, dir, Config{RecordsPerPage: 32})
	if err := idx.Apply(seedEntries(20)); err != nil {
		t.Fatal(err)
	}
	if err := idx.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	generation := idx.Metadata().Generation
	if err := idx.Close(); err != nil {
		t.Fatal(err)
	}
	path := generationPath(dir, generation)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) < 256 {
		t.Fatalf("generation too small: %d", len(data))
	}
	data[len(data)-1] ^= 0xff // last plane byte, covered by the page CRC
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := openIndex(dir, Config{}, DurabilityOS); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("corrupt page error = %v, want ErrCorrupt", err)
	}
}

func TestCorruptDirectoryRejected(t *testing.T) {
	dir := t.TempDir()
	idx := mustNewPersistent(t, dir, Config{RecordsPerPage: 16})
	if err := idx.Apply(seedEntries(40)); err != nil {
		t.Fatal(err)
	}
	if err := idx.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	generation := idx.Metadata().Generation
	if err := idx.Close(); err != nil {
		t.Fatal(err)
	}
	path := generationPath(dir, generation)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// Directory starts after the 64-byte generation header.
	data[genHeaderSize+3] ^= 0xff
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := openIndex(dir, Config{}, DurabilityOS); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("corrupt directory error = %v, want ErrCorrupt", err)
	}
}

func TestMissingGenerationRejected(t *testing.T) {
	dir := t.TempDir()
	idx := mustNewPersistent(t, dir, Config{})
	if err := idx.Close(); err != nil {
		t.Fatal(err)
	}
	manifest, err := readManifest(filepath.Join(dir, "manifest"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(generationPath(dir, manifest.generation)); err != nil {
		t.Fatal(err)
	}
	if _, err := openIndex(dir, Config{}, DurabilityOS); err == nil {
		t.Fatal("expected an error for a missing generation file")
	}
}

func TestPartialPageContinuesAfterReopen(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{RecordsPerPage: 16}
	idx := mustNewPersistent(t, dir, cfg)
	first := seedEntries(10)
	if err := idx.Apply(first); err != nil {
		t.Fatal(err)
	}
	if err := idx.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	if err := idx.Close(); err != nil {
		t.Fatal(err)
	}

	reopened := mustOpenPersistent(t, dir, cfg)
	view := reopened.view.Load()
	if len(view.pages) != 0 || view.activeCount != 10 {
		t.Fatalf("partial final page not adopted: pages=%d active=%d", len(view.pages), view.activeCount)
	}
	second := make([]Entry, 10)
	for k := range second {
		second[k] = entryFromSeed(uint64(100 + k))
	}
	if err := reopened.Apply(second); err != nil {
		t.Fatal(err)
	}
	if got := reopened.Metadata().RecordCount; got != 20 {
		t.Fatalf("record count after continuing page = %d", got)
	}
	if got := reopened.Metadata().PageCount; got != 2 {
		t.Fatalf("page count after continuing page = %d, want 2", got)
	}
	all := append(append([]Entry(nil), first...), second...)
	for k, want := range all {
		got, ok := reopened.EntryAt(Ordinal(k))
		if !ok || got != want {
			t.Fatalf("ordinal %d = %+v, want %+v", k, got, want)
		}
	}
	// Reopen once more without a checkpoint: the generation plus the WAL tail
	// must reproduce the same 20 records.
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
	again := mustOpenPersistent(t, dir, cfg)
	if got := again.Metadata().RecordCount; got != 20 {
		t.Fatalf("record count after WAL replay = %d", got)
	}
	for k, want := range all {
		got, ok := again.EntryAt(Ordinal(k))
		if !ok || got != want {
			t.Fatalf("replayed ordinal %d = %+v, want %+v", k, got, want)
		}
	}
}

func TestVacuumRewritesLiveOnly(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{RecordsPerPage: 16}
	idx := mustNewPersistent(t, dir, cfg)
	entries := seedEntries(100)
	if err := idx.Apply(entries); err != nil {
		t.Fatal(err)
	}
	if err := idx.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	live := idSet{}
	for _, entry := range entries {
		if entry.ImageID%2 == 0 {
			live[entry.ImageID] = struct{}{}
		}
	}
	if err := idx.Vacuum(live); err != nil {
		t.Fatal(err)
	}
	if got := idx.Metadata().RecordCount; got != 50 {
		t.Fatalf("vacuum record count = %d, want 50", got)
	}
	want := make([]Entry, 0, 50)
	for _, entry := range entries {
		if _, ok := live[entry.ImageID]; ok {
			want = append(want, entry)
		}
	}
	for k, entry := range want {
		got, ok := idx.EntryAt(Ordinal(k))
		if !ok || got != entry {
			t.Fatalf("vacuum ordinal %d = %+v, want %+v", k, got, entry)
		}
	}
	if matches, _ := filepath.Glob(filepath.Join(dir, genGlob)); len(matches) != 1 {
		t.Fatalf("generation files after vacuum = %v", matches)
	}
	if matches := walFiles(t, dir); len(matches) != 1 {
		t.Fatalf("wal files after vacuum = %v", matches)
	}
	// Writes after vacuum append to the new WAL epoch and replay correctly.
	extra := make([]Entry, 5)
	for k := range extra {
		extra[k] = entryFromSeed(uint64(5000 + k))
	}
	if err := idx.Apply(extra); err != nil {
		t.Fatal(err)
	}
	if err := idx.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := mustOpenPersistent(t, dir, cfg)
	if got := reopened.Metadata().RecordCount; got != 55 {
		t.Fatalf("record count after post-vacuum replay = %d, want 55", got)
	}
	for k := range extra {
		got, ok := reopened.EntryAt(Ordinal(50 + k))
		if !ok || got != extra[k] {
			t.Fatalf("post-vacuum ordinal %d = %+v, want %+v", 50+k, got, extra[k])
		}
	}
}

func TestVacuumReclaimsWALStorage(t *testing.T) {
	dir := t.TempDir()
	idx := mustNewPersistent(t, dir, Config{RecordsPerPage: 32})
	for round := 0; round < 5; round++ {
		batch := make([]Entry, 400)
		for k := range batch {
			batch[k] = entryFromSeed(uint64(round*400 + k))
		}
		if err := idx.Apply(batch); err != nil {
			t.Fatal(err)
		}
	}
	if err := idx.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	beforeWAL := len(walFiles(t, dir))
	if beforeWAL != 1 {
		t.Fatalf("wal files before vacuum = %d", beforeWAL)
	}
	if err := idx.Vacuum(nil); err != nil {
		t.Fatal(err)
	}
	if got := idx.Metadata().RecordCount; got != 2000 {
		t.Fatalf("vacuum with nil live count = %d", got)
	}
	if matches := walFiles(t, dir); len(matches) != 1 {
		t.Fatalf("wal files after vacuum = %v", matches)
	}
	if err := idx.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := mustOpenPersistent(t, dir, Config{})
	if got := reopened.Metadata().RecordCount; got != 2000 {
		t.Fatalf("reopened count after vacuum = %d", got)
	}
}

func TestPersistentDurabilitySync(t *testing.T) {
	dir := t.TempDir()
	idx, err := newIndex(dir, testPerceptualSpec(Config{RecordsPerPage: 16}), DurabilitySync)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = idx.Close() })
	if err := idx.Apply(seedEntries(7)); err != nil {
		t.Fatal(err)
	}
	if err := idx.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := mustOpenPersistent(t, dir, Config{})
	if got := reopened.Metadata().RecordCount; got != 7 {
		t.Fatalf("synced record count = %d", got)
	}
}

func persistentDir(t *testing.T) string {
	t.Helper()
	return t.TempDir()
}

func TestCreateOpenCheckpoint(t *testing.T) {
	dir := persistentDir(t)
	entries := entriesForRange(0, 30, 1)
	idx, err := newExact(dir, ExactSpec{Namespace: testExactNamespace, Algorithm: AlgorithmSHA256, Entries: entries}, ExactConfig{BlockSize: 1024}, DurabilityOS)
	if err != nil {
		t.Fatal(err)
	}
	for i := uint64(0); i < 30; i++ {
		if got := idx.Lookup(nil, makeHash(i)); len(got) != 1 {
			t.Fatalf("pre-checkpoint lookup %d = %v", i, got)
		}
	}
	if err := idx.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	if err := idx.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := openExact(dir, ExactConfig{BlockSize: 1024}, DurabilityOS)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	for i := uint64(0); i < 30; i++ {
		got := reopened.Lookup(nil, makeHash(i))
		if len(got) != 1 || got[0] != ImageID(i+1) {
			t.Fatalf("post-open lookup %d = %v", i, got)
		}
	}
	if md := reopened.Metadata(); md.Namespace != testExactNamespace || md.Algorithm != AlgorithmSHA256 || md.BaseCount != 30 {
		t.Fatalf("metadata = %+v", md)
	}
}

func TestExactWALReplayWithoutCheckpoint(t *testing.T) {
	dir := persistentDir(t)
	idx, err := newExact(dir, ExactSpec{Algorithm: AlgorithmSHA256}, ExactConfig{}, DurabilityOS)
	if err != nil {
		t.Fatal(err)
	}
	var adds []ExactEntry
	for i := uint64(0); i < 25; i++ {
		adds = append(adds, ExactEntry{Hash: makeHash(i), ImageID: ImageID(i + 1)})
	}
	if err := idx.Apply(Mutation{Add: adds}); err != nil {
		t.Fatal(err)
	}
	// Close without checkpoint: WAL tail must carry the batch.
	if err := idx.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := openExact(dir, ExactConfig{}, DurabilityOS)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	for i := uint64(0); i < 25; i++ {
		if got := reopened.Lookup(nil, makeHash(i)); len(got) != 1 {
			t.Fatalf("replayed lookup %d = %v", i, got)
		}
	}
}

func TestExactIncompleteTrailingWALBatch(t *testing.T) {
	dir := persistentDir(t)
	idx, err := newExact(dir, ExactSpec{Algorithm: AlgorithmSHA256}, ExactConfig{}, DurabilityOS)
	if err != nil {
		t.Fatal(err)
	}
	if err := idx.Apply(Mutation{Add: []ExactEntry{{Hash: makeHash(1), ImageID: 1}}}); err != nil {
		t.Fatal(err)
	}
	if err := idx.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	if err := idx.Apply(Mutation{Add: []ExactEntry{{Hash: makeHash(2), ImageID: 2}}}); err != nil {
		t.Fatal(err)
	}
	walPath := exactWALPath(dir, 1)
	st, err := os.Stat(walPath)
	if err != nil {
		t.Fatal(err)
	}
	// Truncate the last byte: the trailing batch becomes an incomplete tail
	// and must be ignored, preserving the first checkpointed entry.
	if err := os.Truncate(walPath, st.Size()-1); err != nil {
		t.Fatal(err)
	}
	_ = idx.Close()
	reopened, err := openExact(dir, ExactConfig{}, DurabilityOS)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	// ExactEntry 1 was checkpointed; entry 2's truncated batch is dropped.
	if got := reopened.Lookup(nil, makeHash(1)); len(got) != 1 {
		t.Fatalf("checkpointed entry lost: %v", got)
	}
}

func TestReplacementWithoutRootPublication(t *testing.T) {
	dir := persistentDir(t)
	idx, err := newExact(dir, ExactSpec{Algorithm: AlgorithmSHA256, Entries: entriesForRange(0, 10, 1)}, ExactConfig{}, DurabilityOS)
	if err != nil {
		t.Fatal(err)
	}
	// In-memory COW replacements without checkpoint: simulate a crash by
	// closing without checkpointing, then recovering purely from WAL.
	if err := idx.Apply(Mutation{Add: []ExactEntry{{Hash: makeHash(100), ImageID: 100}}}); err != nil {
		t.Fatal(err)
	}
	if err := idx.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := openExact(dir, ExactConfig{}, DurabilityOS)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	if got := reopened.Lookup(nil, makeHash(100)); len(got) != 1 {
		t.Fatalf("uncheckpointed replacement lost: %v", got)
	}
}

func TestExactCorruptManifestRejected(t *testing.T) {
	dir := persistentDir(t)
	idx, err := newExact(dir, ExactSpec{Algorithm: AlgorithmSHA256}, ExactConfig{}, DurabilityOS)
	if err != nil {
		t.Fatal(err)
	}
	_ = idx.Close()
	manifestPath := filepath.Join(dir, "manifest")
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	data[10] ^= 0xff
	if err := os.WriteFile(manifestPath, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := openExact(dir, ExactConfig{}, DurabilityOS); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("manifest corruption err = %v, want ErrCorrupt", err)
	}
}

func TestExactOversizedNamespaceRejected(t *testing.T) {
	manifest := exactManifest{
		generation: 3, namespace: 7, liveCount: 11, blockCount: 2,
		walOffset: 128, flags: 0, blockSize: DefaultBlockSize,
		algorithm: AlgorithmSHA256, targetFillPct: DefaultTargetFillPct,
		walEpoch: 1,
	}
	oversized := uint64(math.MaxUint16) + 1
	data := encodeManifest(manifest)
	binary.LittleEndian.PutUint64(data[16:24], oversized)
	binary.LittleEndian.PutUint32(data[60:64], crc32.ChecksumIEEE(data[:60]))
	if _, err := decodeManifest(data); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("oversized manifest namespace err = %v, want ErrCorrupt", err)
	}

	header := make([]byte, exactGenHeaderSize)
	encodeExactGenHeader(header, manifest)
	binary.LittleEndian.PutUint64(header[16:24], oversized)
	binary.LittleEndian.PutUint32(header[124:128], crc32.ChecksumIEEE(header[:124]))
	if _, err := decodeExactGenHeader(header); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("oversized generation namespace err = %v, want ErrCorrupt", err)
	}
}

func TestCorruptBlockPayloadRejected(t *testing.T) {
	dir := persistentDir(t)
	idx, err := newExact(dir, ExactSpec{Algorithm: AlgorithmSHA256, Entries: entriesForRange(0, 20, 1)}, ExactConfig{BlockSize: 1024}, DurabilityOS)
	if err != nil {
		t.Fatal(err)
	}
	gen := idx.Metadata().Generation
	_ = idx.Close()
	genPath := exactGenerationPath(dir, gen)
	data, err := os.ReadFile(genPath)
	if err != nil {
		t.Fatal(err)
	}
	// Flip a payload byte past the header+directory.
	if len(data) < 300 {
		t.Fatalf("generation file too small: %d", len(data))
	}
	data[len(data)-1] ^= 0xff
	if err := os.WriteFile(genPath, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := openExact(dir, ExactConfig{}, DurabilityOS); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("block corruption err = %v, want ErrCorrupt", err)
	}
}

func TestExactCorruptWALRejected(t *testing.T) {
	dir := persistentDir(t)
	idx, err := newExact(dir, ExactSpec{Algorithm: AlgorithmSHA256}, ExactConfig{}, DurabilityOS)
	if err != nil {
		t.Fatal(err)
	}
	if err := idx.Apply(Mutation{Add: []ExactEntry{{Hash: makeHash(1), ImageID: 1}}}); err != nil {
		t.Fatal(err)
	}
	_ = idx.Close()
	walPath := exactWALPath(dir, 1)
	data, err := os.ReadFile(walPath)
	if err != nil {
		t.Fatal(err)
	}
	// Corrupt the payload of the (only) WAL batch without truncating: flip a
	// byte in the first op's hash so CRC fails on a complete record.
	if len(data) < exactWALHeaderSize+10 {
		t.Fatalf("wal too small: %d", len(data))
	}
	data[exactWALHeaderSize] ^= 0xff
	if err := os.WriteFile(walPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := openExact(dir, ExactConfig{}, DurabilityOS); !errors.Is(err, ErrCorruptWAL) {
		t.Fatalf("wal corruption err = %v, want ErrCorruptWAL", err)
	}
}

func TestCorruptDirectoryOrderingRejected(t *testing.T) {
	dir := persistentDir(t)
	entries := entriesForRange(0, 40, 1)
	idx, err := newExact(dir, ExactSpec{Algorithm: AlgorithmSHA256, Entries: entries}, ExactConfig{BlockSize: 512}, DurabilityOS)
	if err != nil {
		t.Fatal(err)
	}
	if idx.Metadata().BlockCount < 2 {
		t.Skip("need multiple blocks for directory test")
	}
	gen := idx.Metadata().Generation
	_ = idx.Close()
	genPath := exactGenerationPath(dir, gen)
	data, err := os.ReadFile(genPath)
	if err != nil {
		t.Fatal(err)
	}
	// Swap the FirstHash of the first two directory entries.
	if len(data) < exactGenHeaderSize+2*dirEntrySize {
		t.Fatal("generation too small")
	}
	first := make([]byte, dirEntrySize)
	copy(first, data[exactGenHeaderSize:exactGenHeaderSize+dirEntrySize])
	copy(data[exactGenHeaderSize:exactGenHeaderSize+dirEntrySize], data[exactGenHeaderSize+dirEntrySize:exactGenHeaderSize+2*dirEntrySize])
	copy(data[exactGenHeaderSize+dirEntrySize:exactGenHeaderSize+2*dirEntrySize], first)
	// Directory is not CRC-protected, so no checksum fix-up is needed; the
	// loader must reject the misordering plus FirstHash mismatches.
	if err := os.WriteFile(genPath, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := openExact(dir, ExactConfig{}, DurabilityOS); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("directory corruption err = %v, want ErrCorrupt", err)
	}
}

func TestUnsortedBlockRecordsRejected(t *testing.T) {
	dir := persistentDir(t)
	entries := entriesForRange(0, 10, 1)
	idx, err := newExact(dir, ExactSpec{Algorithm: AlgorithmSHA256, Entries: entries}, ExactConfig{BlockSize: 4096}, DurabilityOS)
	if err != nil {
		t.Fatal(err)
	}
	gen := idx.Metadata().Generation
	_ = idx.Close()
	genPath := exactGenerationPath(dir, gen)
	data, err := os.ReadFile(genPath)
	if err != nil {
		t.Fatal(err)
	}
	// Single block: swap the first two 40-byte records and fix the block CRC
	// so the loader reaches the sort check rather than failing on checksum.
	payloadOff := exactGenHeaderSize + dirEntrySize + blockHeaderSize
	if len(data) < payloadOff+2*RecordSize {
		t.Fatal("generation too small for swap")
	}
	a := make([]byte, RecordSize)
	copy(a, data[payloadOff:payloadOff+RecordSize])
	copy(data[payloadOff:payloadOff+RecordSize], data[payloadOff+RecordSize:payloadOff+2*RecordSize])
	copy(data[payloadOff+RecordSize:payloadOff+2*RecordSize], a)
	fixBlockCRC(data, payloadOff)
	if err := os.WriteFile(genPath, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := openExact(dir, ExactConfig{}, DurabilityOS); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("unsorted records err = %v, want ErrCorrupt", err)
	}
}

func fixBlockCRC(file []byte, payloadOff int) {
	// Recompute the single block's CRC after test surgery. The block header
	// immediately precedes the payload.
	headerOff := payloadOff - blockHeaderSize
	count := int(uint32(file[headerOff+8]) | uint32(file[headerOff+9])<<8 | uint32(file[headerOff+10])<<16 | uint32(file[headerOff+11])<<24)
	payload := file[payloadOff : payloadOff+count*RecordSize]
	crc := crc32.ChecksumIEEE(payload)
	file[headerOff+16] = byte(crc)
	file[headerOff+17] = byte(crc >> 8)
	file[headerOff+18] = byte(crc >> 16)
	file[headerOff+19] = byte(crc >> 24)
	// Also repair the generation header CRC (unchanged here) — no-op.
}

func TestVacuumAndReopen(t *testing.T) {
	dir := persistentDir(t)
	idx, err := newExact(dir, ExactSpec{Algorithm: AlgorithmSHA256}, ExactConfig{BlockSize: 512}, DurabilityOS)
	if err != nil {
		t.Fatal(err)
	}
	// Sustained random inserts fragment the file across checkpoints.
	for round := 0; round < 10; round++ {
		var adds []ExactEntry
		for k := 0; k < 20; k++ {
			adds = append(adds, ExactEntry{Hash: makeHash(uint64(round*20 + k)), ImageID: ImageID(round*20 + k + 1)})
		}
		if err := idx.Apply(Mutation{Add: adds}); err != nil {
			t.Fatal(err)
		}
		if err := idx.Checkpoint(); err != nil {
			t.Fatal(err)
		}
	}
	before := idx.OnDiskBytes()
	// Delete half to create sparse blocks, then vacuum.
	var removes []ExactEntry
	for k := 0; k < 100; k++ {
		removes = append(removes, ExactEntry{Hash: makeHash(uint64(k)), ImageID: ImageID(k + 1)})
	}
	if err := idx.Apply(Mutation{Remove: removes}); err != nil {
		t.Fatal(err)
	}
	// Effective count: removes stage into the overlay until vacuum folds.
	wantLive := idx.LiveCount()
	if err := idx.Vacuum(); err != nil {
		t.Fatal(err)
	}
	after := idx.OnDiskBytes()
	if after > before {
		t.Logf("vacuum bytes before=%d after=%d (dead storage reclaimed across generations)", before, after)
	}
	if got := idx.LiveCount(); got != wantLive {
		t.Fatalf("vacuum changed live count: %d -> %d", wantLive, got)
	}
	// Vacuum folds the overlay: base count now equals the live count and
	// no staged entries remain.
	if md := idx.Metadata(); md.BaseCount != wantLive || md.OverlayCount != 0 {
		t.Fatalf("post-vacuum metadata = %+v, want base %d overlay 0", md, wantLive)
	}
	if err := checkViewInvariants(t, idx); err != nil {
		t.Fatal(err)
	}
	gen := idx.Metadata().Generation
	_ = idx.Close()
	reopened, err := openExact(dir, ExactConfig{BlockSize: 512}, DurabilityOS)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	if reopened.LiveCount() != wantLive {
		t.Fatalf("reopen count = %d, want %d", reopened.LiveCount(), wantLive)
	}
	_ = gen
	// Only the current generation file should remain.
	matches, _ := filepath.Glob(filepath.Join(dir, "exact-*.idx"))
	if len(matches) != 1 {
		t.Fatalf("generations after vacuum = %v, want 1", matches)
	}
}

func TestAlgorithmPersistedAuthoritatively(t *testing.T) {
	dir := t.TempDir()
	spec := ExactSpec{Namespace: testExactNamespace, Algorithm: AlgorithmBLAKE3, Entries: []ExactEntry{{Hash: SumBLAKE3([]byte("x")), ImageID: 1}}}
	idx, err := newExact(dir, spec, ExactConfig{}, DurabilityOS)
	if err != nil {
		t.Fatal(err)
	}
	_ = idx.Close()
	reopened, err := openExact(dir, ExactConfig{}, DurabilityOS)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	if reopened.Metadata().Algorithm != AlgorithmBLAKE3 {
		t.Fatalf("algorithm = %v, want blake3", reopened.Metadata().Algorithm)
	}
}

// TestVacuumWithoutRepackKeepsViewReadable is a regression test: a vacuum
// whose repack leaves the block layout unchanged used to unmap the generation
// mapping still referenced by the published view.
func TestVacuumWithoutRepackKeepsViewReadable(t *testing.T) {
	dir := persistentDir(t)
	entries := entriesForRange(0, 20, 1)
	idx, err := newExact(dir, ExactSpec{Algorithm: AlgorithmSHA256, Entries: entries}, ExactConfig{}, DurabilityOS)
	if err != nil {
		t.Fatal(err)
	}
	if err := idx.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := openExact(dir, ExactConfig{}, DurabilityOS)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	if err := reopened.Vacuum(); err != nil {
		t.Fatal(err)
	}
	for i := uint64(0); i < 20; i++ {
		if got := reopened.Lookup(nil, makeHash(i)); len(got) != 1 || got[0] != ImageID(i+1) {
			t.Fatalf("post-vacuum lookup %d = %v", i, got)
		}
	}
	if err := reopened.Checkpoint(); err != nil {
		t.Fatal(err)
	}
}

// TestVacuumReclaimsExactWAL verifies that Vacuum rolls the WAL to a fresh
// epoch, removes superseded WAL files, and keeps the index replayable.
func TestVacuumReclaimsExactWAL(t *testing.T) {
	dir := persistentDir(t)
	idx, err := newExact(dir, ExactSpec{Algorithm: AlgorithmSHA256}, ExactConfig{}, DurabilityOS)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = idx.Close() }()
	for round := 0; round < 10; round++ {
		var adds []ExactEntry
		for k := 0; k < 100; k++ {
			n := round*100 + k
			adds = append(adds, ExactEntry{Hash: makeHash(uint64(n)), ImageID: ImageID(n + 1)})
		}
		if err := idx.Apply(Mutation{Add: adds}); err != nil {
			t.Fatal(err)
		}
	}
	if err := idx.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	if err := idx.Vacuum(); err != nil {
		t.Fatal(err)
	}
	files, err := filepath.Glob(filepath.Join(dir, exactWALGlob))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 {
		t.Fatalf("WAL files after vacuum = %v, want 1", files)
	}
	if st, statErr := os.Stat(files[0]); statErr != nil || st.Size() != 0 {
		t.Fatalf("WAL after vacuum = %v (err %v), want an empty file", st, statErr)
	}
	if err := idx.Apply(Mutation{Add: []ExactEntry{{Hash: makeHash(5_000), ImageID: 5_000}}}); err != nil {
		t.Fatal(err)
	}
	if err := idx.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := openExact(dir, ExactConfig{}, DurabilityOS)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	for i := uint64(0); i < 1_000; i++ {
		if got := reopened.Lookup(nil, makeHash(i)); len(got) != 1 {
			t.Fatalf("post-vacuum lookup %d = %v", i, got)
		}
	}
	if got := reopened.Lookup(nil, makeHash(5_000)); len(got) != 1 || got[0] != 5_000 {
		t.Fatalf("rotated-WAL lookup = %v, want [5000]", got)
	}
}
