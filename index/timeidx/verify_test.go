package timeidx

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	common "github.com/evanoberholster/imageIdx/common"
)

func mustVerifyStore(t *testing.T, dir string) {
	t.Helper()
	index, err := NewStore(dir, Spec{Offset: 1_000}, Config{BlockSize: 2, DeltaCapacity: 8}, DurabilityOS)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = index.Close() }()
	batch := NewBatch(4)
	batch.Append(Insert(1, 1_010))
	batch.Append(Insert(2, 1_020))
	if err := index.Apply(batch); err != nil {
		t.Fatal(err)
	}
	if err := index.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	if err := index.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestVerifyHealthy(t *testing.T) {
	dir := t.TempDir()
	mustVerifyStore(t, dir)
	if err := Verify(dir); err != nil {
		t.Fatalf("Verify = %v, want nil", err)
	}
	// Verification is read-only: the store still opens with data intact.
	reopened, err := OpenStore(dir, Config{}, DurabilityOS)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	if got := reopened.Search(nil, 1_000, 2_000); !equalIDSet(got, []common.ValueID{1, 2}) {
		t.Fatalf("search after verify = %v, want [1 2]", got)
	}
}

func TestVerifyNotAStore(t *testing.T) {
	if err := Verify(t.TempDir()); err == nil {
		t.Fatal("Verify empty dir = nil, want error")
	}
}

func TestVerifyCorruptManifest(t *testing.T) {
	dir := t.TempDir()
	mustVerifyStore(t, dir)
	path := filepath.Join(dir, "manifest")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	data[10] ^= 0xff
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Verify(dir); !errors.Is(err, ErrCorruptManifest) {
		t.Fatalf("Verify corrupt manifest = %v, want ErrCorruptManifest", err)
	}
}

func TestVerifyCorruptPayload(t *testing.T) {
	dir := t.TempDir()
	mustVerifyStore(t, dir)
	matches, _ := filepath.Glob(filepath.Join(dir, "markers.*.base"))
	if len(matches) == 0 {
		t.Fatal("no markers generation to corrupt")
	}
	markers := matches[len(matches)-1]
	data, err := os.ReadFile(markers)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) < 8 {
		t.Fatalf("markers file too small: %d", len(data))
	}
	// Flip the first marker above the second (test data shares one
	// marker, so the tail cannot break ordering); sortedness validation
	// must reject it.
	data[3] ^= 0xff
	if err := os.WriteFile(markers, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Verify(dir); !errors.Is(err, ErrInvalidBase) {
		t.Fatalf("Verify corrupt payload = %v, want ErrInvalidBase", err)
	}
}

func TestVerifyMissingWAL(t *testing.T) {
	dir := t.TempDir()
	mustVerifyStore(t, dir)
	matches, _ := filepath.Glob(filepath.Join(dir, "timestamp-*.wal"))
	if len(matches) == 0 {
		t.Fatal("no WAL to remove")
	}
	if err := os.Remove(matches[0]); err != nil {
		t.Fatal(err)
	}
	if err := Verify(dir); err == nil {
		t.Fatal("Verify with amputated WAL = nil, want error")
	}
}
