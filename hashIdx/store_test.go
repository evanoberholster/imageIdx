package hashidx

import (
	"errors"
	"os"
	"path/filepath"
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
