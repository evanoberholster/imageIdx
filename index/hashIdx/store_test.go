package hashidx

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func mustNewStore(t *testing.T, dir string, spec StoreSpec) *Store {
	t.Helper()
	store, err := NewStore(dir, spec, DurabilityOS)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func TestStoreRequiresDirectory(t *testing.T) {
	if _, err := NewStore("", StoreSpec{Exact: ExactSpec{Algorithm: AlgorithmSHA256}}, DurabilityOS); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("empty dir err = %v, want ErrInvalidConfig", err)
	}
}

func TestStorePersistentCheckpointReopenAndWAL(t *testing.T) {
	dir := t.TempDir()
	spec := StoreSpec{
		Perceptual:  Config{RecordsPerPage: 64},
		Exact:       ExactSpec{IndexID: 7, Algorithm: AlgorithmBLAKE3, Entries: entriesForRange(0, 10, 1)},
		ExactConfig: ExactConfig{BlockSize: 1024},
	}
	store, err := NewStore(dir, spec, DurabilityOS)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		filepath.Join(dir, "manifest"),
		filepath.Join(dir, perceptualDirName, "manifest"),
		filepath.Join(dir, exactDirName, "manifest"),
		filepath.Join(dir, storeLockName),
	} {
		if _, statErr := os.Stat(path); statErr != nil {
			t.Fatalf("store artifact %s: %v", path, statErr)
		}
	}
	// Writes without a checkpoint must replay from each index's WAL tail.
	if err := store.Perceptual().Apply(seedEntries(5)); err != nil {
		t.Fatal(err)
	}
	if err := store.Exact().Add(makeHash(1000), 500); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := OpenStore(dir, DurabilityOS)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	if reopened.Perceptual().readers != reopened.Exact().readers {
		t.Fatal("reopened store indexes do not share a reader tracker")
	}
	if md := reopened.Exact().Metadata(); md.IndexID != 7 || md.Algorithm != AlgorithmBLAKE3 {
		t.Fatalf("exact metadata = %+v", md)
	}
	if got := reopened.Perceptual().Metadata().RecordsPerPage; got != 64 {
		t.Fatalf("perceptual page size = %d, want 64", got)
	}
	if got := reopened.Exact().config.BlockSize; got != 1024 {
		t.Fatalf("exact block size = %d, want persisted 1024", got)
	}
	if got := reopened.Perceptual().Metadata().RecordCount; got != 5 {
		t.Fatalf("replayed perceptual records = %d, want 5", got)
	}
	if got := reopened.Exact().Lookup(nil, makeHash(1000)); len(got) != 1 || got[0] != 500 {
		t.Fatalf("replayed exact lookup = %v, want [500]", got)
	}
	if got := reopened.Exact().Lookup(nil, makeHash(0)); len(got) != 1 {
		t.Fatalf("checkpointed exact lookup = %v, want 1 id", got)
	}

	live := idSet{}
	for _, entry := range seedEntries(5)[:3] {
		live[entry.ImageID] = struct{}{}
	}
	if err := reopened.Vacuum(live); err != nil {
		t.Fatal(err)
	}
	if got := reopened.Perceptual().Metadata().RecordCount; got != 3 {
		t.Fatalf("perceptual records after vacuum = %d, want 3", got)
	}
	if got := reopened.Exact().Lookup(nil, makeHash(0)); len(got) != 1 {
		t.Fatalf("exact contents changed by vacuum: %v", got)
	}
}

func TestStoreDirectoryOwnership(t *testing.T) {
	dir := t.TempDir()
	spec := StoreSpec{Exact: ExactSpec{Algorithm: AlgorithmSHA256}}
	first := mustNewStore(t, dir, spec)
	if _, err := NewStore(dir, spec, DurabilityOS); !errors.Is(err, ErrStoreBusy) {
		t.Fatalf("second NewStore err = %v, want ErrStoreBusy", err)
	}
	if _, err := OpenStore(dir, DurabilityOS); !errors.Is(err, ErrStoreBusy) {
		t.Fatalf("OpenStore while locked err = %v, want ErrStoreBusy", err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := NewStore(dir, spec, DurabilityOS); !errors.Is(err, ErrStoreExists) {
		t.Fatalf("NewStore over store err = %v, want ErrStoreExists", err)
	}
	if _, err := OpenStore(t.TempDir(), DurabilityOS); !errors.Is(err, ErrNotStore) {
		t.Fatalf("OpenStore over plain dir err = %v, want ErrNotStore", err)
	}
}

// TestStoreCheckpointConvergenceAfterExactFailure hides the exact
// directory so its generation write fails mid-Checkpoint, then proves
// the store converges: perceptual advances alone, the joint anchor still
// names the last joint pair, and the next Checkpoint repairs both the
// exact generation and the anchor with data intact.
func TestStoreCheckpointConvergenceAfterExactFailure(t *testing.T) {
	// Renaming a directory out from under open mmap/WAL handles fails on
	// Windows; the fault this test injects is Unix-only.
	if runtime.GOOS == "windows" {
		t.Skip("rename-away fault injection needs Unix rename semantics")
	}
	dir := t.TempDir()
	spec := StoreSpec{
		Perceptual: Config{RecordsPerPage: 64},
		Exact:      ExactSpec{Algorithm: AlgorithmSHA256},
	}
	store := mustNewStore(t, dir, spec)
	if err := store.Perceptual().Apply(seedEntries(4)); err != nil {
		t.Fatal(err)
	}
	if err := store.Exact().Add(makeHash(4242), 4242); err != nil {
		t.Fatal(err)
	}
	subGen := func() (uint64, uint64) {
		t.Helper()
		p, err := readManifest(filepath.Join(dir, perceptualDirName, "manifest"))
		if err != nil {
			t.Fatal(err)
		}
		e, err := readDiskManifest(filepath.Join(dir, exactDirName, "manifest"))
		if err != nil {
			t.Fatal(err)
		}
		return p.generation, e.generation
	}
	// Fault: the exact generation file cannot be created.
	exactDir := filepath.Join(dir, exactDirName)
	hidden := exactDir + ".hidden"
	if err := os.Rename(exactDir, hidden); err != nil {
		t.Fatal(err)
	}
	failed := store.Checkpoint() == nil
	if err := os.Rename(hidden, exactDir); err != nil {
		t.Fatal(err)
	}
	if failed {
		t.Fatal("Checkpoint over a hidden exact dir succeeded")
	}
	pPersisted, ePersisted := subGen()
	if pPersisted == 1 {
		t.Fatal("perceptual checkpoint did not advance before the exact failure")
	}
	if ePersisted != 1 {
		t.Fatalf("exact generation advanced despite failure: %d", ePersisted)
	}
	if pGen, eGen, err := ReadStoreManifest(dir); err != nil || pGen != 1 || eGen != 1 {
		t.Fatalf("joint anchor = (%d, %d, %v), want (1, 1, nil)", pGen, eGen, err)
	}
	// Repair is another Checkpoint: it converges and re-anchors.
	if err := store.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	pGen, eGen, err := ReadStoreManifest(dir)
	if err != nil {
		t.Fatal(err)
	}
	pNow, eNow := subGen()
	if pGen != pNow || eGen != eNow {
		t.Fatalf("joint anchor = (%d, %d), sub-manifests = (%d, %d)", pGen, eGen, pNow, eNow)
	}
	if lp, le := store.Generations(); lp != pGen || le != eGen {
		t.Fatalf("live Generations = (%d, %d), anchor = (%d, %d)", lp, le, pGen, eGen)
	}
	if got := store.Perceptual().Metadata().RecordCount; got != 4 {
		t.Fatalf("perceptual records = %d, want 4", got)
	}
	if got := store.Exact().Lookup(nil, makeHash(4242)); len(got) != 1 || got[0] != 4242 {
		t.Fatalf("exact lookup = %v, want [4242]", got)
	}
}

func TestStoreCreationFailureCleansUp(t *testing.T) {
	dir := t.TempDir()
	if _, err := NewStore(dir, StoreSpec{Exact: ExactSpec{Algorithm: 0}}, DurabilityOS); !errors.Is(err, ErrInvalidAlgorithm) {
		t.Fatalf("bad algorithm err = %v, want ErrInvalidAlgorithm", err)
	}
	store, err := NewStore(dir, StoreSpec{Exact: ExactSpec{Algorithm: AlgorithmSHA256}}, DurabilityOS)
	if err != nil {
		t.Fatalf("store not reusable after failed create: %v", err)
	}
	_ = store.Close()
}
