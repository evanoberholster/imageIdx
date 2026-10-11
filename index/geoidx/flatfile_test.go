package geoidx

import (
	"os"
	"path/filepath"
	"testing"

	common "github.com/evanoberholster/imageIdx/common"
)

// flatEntries returns n deterministic entries whose ImageIDs start at base.
func flatEntries(n int, base uint64) []Entry {
	entries := shardedMemoryEntries(n)
	for i := range entries {
		entries[i].ImageID += common.ImageID(base)
	}
	return entries
}

// assertPersisted checks the reopened index holds exactly entries and nothing
// from the other index's disjoint ImageID range.
func assertPersisted(t *testing.T, index *Index, entries []Entry) {
	t.Helper()
	if got := index.Len(); got != uint64(len(entries)) {
		t.Fatalf("Len = %d, want %d", got, len(entries))
	}
	for _, e := range entries {
		if !index.Contains(e.Key, e.ImageID) {
			t.Fatalf("reopened index missing (key=%d,id=%d)", e.Key, e.ImageID)
		}
	}
}

// TestNewFileMultipleIndexesShareDirectory verifies that several geoidx indexes
// can coexist in one directory by choosing their data-file path with NewFile,
// and that OpenFile reopens exactly the requested file.
func TestNewFileMultipleIndexesShareDirectory(t *testing.T) {
	dir := t.TempDir()
	pathA := filepath.Join(dir, "geo-0003.idx")
	pathB := filepath.Join(dir, "geo-0004.idx")
	cfg := Config{}

	entriesA := flatEntries(100, 0)
	entriesB := flatEntries(200, 1000)

	idxA, err := NewFile(pathA, cfg)
	if err != nil {
		t.Fatalf("NewFile(%q): %v", pathA, err)
	}
	idxB, err := NewFile(pathB, cfg)
	if err != nil {
		t.Fatalf("NewFile(%q): %v", pathB, err)
	}

	// The lock file must be path+".lock" for NewFile.
	for _, path := range []string{pathA, pathB} {
		if _, err := os.Stat(path + ".lock"); err != nil {
			t.Fatalf("lock file %q missing: %v", path+".lock", err)
		}
	}

	if err := idxA.InsertBatch(entriesA); err != nil {
		t.Fatalf("InsertBatch A: %v", err)
	}
	if err := idxB.InsertBatch(entriesB); err != nil {
		t.Fatalf("InsertBatch B: %v", err)
	}
	if err := idxA.Checkpoint(); err != nil {
		t.Fatalf("Checkpoint A: %v", err)
	}
	if err := idxB.Checkpoint(); err != nil {
		t.Fatalf("Checkpoint B: %v", err)
	}
	if err := idxA.Close(); err != nil {
		t.Fatalf("Close A: %v", err)
	}
	if err := idxB.Close(); err != nil {
		t.Fatalf("Close B: %v", err)
	}

	// Both data files must coexist in the shared directory.
	for _, path := range []string{pathA, pathB} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("data file %q missing after close: %v", path, err)
		}
	}

	reopenedA, err := OpenFile(pathA, cfg)
	if err != nil {
		t.Fatalf("OpenFile(%q): %v", pathA, err)
	}
	defer reopenedA.Close()
	reopenedB, err := OpenFile(pathB, cfg)
	if err != nil {
		t.Fatalf("OpenFile(%q): %v", pathB, err)
	}
	defer reopenedB.Close()

	assertPersisted(t, reopenedA, entriesA)
	assertPersisted(t, reopenedB, entriesB)

	// Each file is independent: A must not contain B's ImageIDs and vice versa.
	for _, e := range entriesB {
		if reopenedA.Contains(e.Key, e.ImageID) {
			t.Fatalf("index A unexpectedly contains B entry (key=%d,id=%d)", e.Key, e.ImageID)
		}
	}
	for _, e := range entriesA {
		if reopenedB.Contains(e.Key, e.ImageID) {
			t.Fatalf("index B unexpectedly contains A entry (key=%d,id=%d)", e.Key, e.ImageID)
		}
	}
}

// TestNewOpenKeepDefaultFileName confirms New/Open still use <dir>/geoidx.dat and
// <dir>/geoidx.lock.
func TestNewOpenKeepDefaultFileName(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{}
	entries := shardedMemoryEntries(50)

	index, err := New(dir, cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := index.InsertBatch(entries); err != nil {
		t.Fatalf("InsertBatch: %v", err)
	}
	if err := index.Checkpoint(); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	if err := index.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if _, err := os.Stat(filepath.Join(dir, geoidxFile)); err != nil {
		t.Fatalf("%s missing: %v", geoidxFile, err)
	}
	if _, err := os.Stat(filepath.Join(dir, lockName)); err != nil {
		t.Fatalf("%s missing: %v", lockName, err)
	}

	reopened, err := Open(dir, cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer reopened.Close()
	assertPersisted(t, reopened, entries)
}
