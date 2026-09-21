package hashidx

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func mustVerifyStore(t *testing.T, dir string) {
	t.Helper()
	store, err := NewStore(dir, StoreSpec{Exact: ExactSpec{Algorithm: AlgorithmSHA256}}, DurabilityOS)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	if err := store.Perceptual().Apply(seedEntries(12)); err != nil {
		t.Fatal(err)
	}
	if err := store.Exact().Add(makeHash(4242), 4242); err != nil {
		t.Fatal(err)
	}
	if err := store.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestVerifyStoreHealthy(t *testing.T) {
	dir := t.TempDir()
	mustVerifyStore(t, dir)
	if err := VerifyStore(dir); err != nil {
		t.Fatalf("VerifyStore = %v, want nil", err)
	}
	// Verification is read-only: the store still opens with data intact.
	reopened, err := OpenStore(dir, DurabilityOS)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	if got := reopened.Perceptual().Metadata().RecordCount; got != 12 {
		t.Fatalf("records = %d, want 12", got)
	}
	if got := reopened.Exact().Lookup(nil, makeHash(4242)); len(got) != 1 {
		t.Fatalf("lookup = %v, want [4242]", got)
	}
}

func TestVerifyStoreMissingAnchor(t *testing.T) {
	dir := t.TempDir()
	mustVerifyStore(t, dir)
	// Pre-V1 stores have no joint anchor; that alone is not a defect.
	if err := os.Remove(filepath.Join(dir, "manifest")); err != nil {
		t.Fatal(err)
	}
	if err := VerifyStore(dir); err != nil {
		t.Fatalf("VerifyStore without anchor = %v, want nil", err)
	}
}

func TestVerifyStoreNotAStore(t *testing.T) {
	if err := VerifyStore(t.TempDir()); !errors.Is(err, ErrNotStore) {
		t.Fatalf("VerifyStore empty dir = %v, want ErrNotStore", err)
	}
}

func TestVerifyStoreCorruptPayload(t *testing.T) {
	dir := t.TempDir()
	mustVerifyStore(t, dir)
	md, err := readDiskManifest(filepath.Join(dir, exactDirName, "manifest"))
	if err != nil {
		t.Fatal(err)
	}
	genPath := exactGenerationPath(filepath.Join(dir, exactDirName), md.generation)
	data, err := os.ReadFile(genPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) <= exactGenHeaderSize {
		t.Fatalf("generation file too small: %d", len(data))
	}
	// Last byte sits in block payload, covered by the block CRC.
	data[len(data)-1] ^= 0xff
	if err := os.WriteFile(genPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := VerifyStore(dir); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("VerifyStore corrupt payload = %v, want ErrCorrupt", err)
	}
}

func TestVerifyStoreVersionMismatch(t *testing.T) {
	dir := t.TempDir()
	mustVerifyStore(t, dir)
	path := filepath.Join(dir, exactDirName, "manifest")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	data[4] ^= 0xff
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := VerifyStore(dir); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("VerifyStore versioned manifest = %v, want ErrUnsupported", err)
	}
}

func TestVerifyStoreMissingWAL(t *testing.T) {
	dir := t.TempDir()
	mustVerifyStore(t, dir)
	matches, _ := filepath.Glob(filepath.Join(dir, exactDirName, "exact-*.wal"))
	if len(matches) == 0 {
		t.Fatal("no exact WAL to remove")
	}
	if err := os.Remove(matches[0]); err != nil {
		t.Fatal(err)
	}
	if err := VerifyStore(dir); err == nil {
		t.Fatal("VerifyStore with amputated WAL = nil, want error")
	}
}
