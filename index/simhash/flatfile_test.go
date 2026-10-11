package simhash

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// TestOpenFilePrefixSharesDirectory verifies that two prefixed stores can live
// in the same directory, keep independent contents across a reopen, and leave
// both file sets coexisting on disk.
func TestOpenFilePrefixSharesDirectory(t *testing.T) {
	dir := t.TempDir()
	cfg := testConfig()

	prefix4 := filepath.Join(dir, "sim-0004")
	prefix5 := filepath.Join(dir, "sim-0005")

	store4, err := OpenFile(prefix4, cfg)
	if err != nil {
		t.Fatalf("OpenFile(%q): %v", prefix4, err)
	}
	store5, err := OpenFile(prefix5, cfg)
	if err != nil {
		t.Fatalf("OpenFile(%q): %v", prefix5, err)
	}

	// Store 4: one appended record then an in-place update of that record.
	rec4 := randRecords(10, 1)[0]
	if id, err := store4.Append(rec4); err != nil || id != 1 {
		t.Fatalf("store4 Append = %d, %v; want 1, nil", id, err)
	}
	updated4 := randRecords(11, 1)[0]
	if err := store4.Upsert(1, updated4); err != nil {
		t.Fatalf("store4 Upsert: %v", err)
	}

	// Store 5: two distinct appended records.
	rec5a := randRecords(12, 1)[0]
	rec5b := randRecords(13, 1)[0]
	if id, err := store5.Append(rec5a); err != nil || id != 1 {
		t.Fatalf("store5 Append first = %d, %v; want 1, nil", id, err)
	}
	if id, err := store5.Append(rec5b); err != nil || id != 2 {
		t.Fatalf("store5 Append second = %d, %v; want 2, nil", id, err)
	}

	for name, st := range map[string]*Store{"store4": store4, "store5": store5} {
		if err := st.Checkpoint(); err != nil {
			t.Fatalf("%s Checkpoint: %v", name, err)
		}
		if err := st.Close(); err != nil {
			t.Fatalf("%s Close: %v", name, err)
		}
	}

	// Both stores' file sets must coexist in the shared directory.
	wantFiles := []string{
		"sim-0004.manifest",
		"sim-0004-shard-000000.bin",
		"sim-0004.wal",
		"sim-0005.manifest",
		"sim-0005-shard-000000.bin",
		"sim-0005.wal",
	}
	for _, name := range wantFiles {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatalf("expected prefixed file %q: %v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, manifestFilename)); !os.IsNotExist(err) {
		t.Fatalf("legacy manifest unexpectedly present in prefix-only directory: %v", err)
	}

	// Reopen each store and confirm the contents stayed independent.
	reopened4, err := OpenFile(prefix4, cfg)
	if err != nil {
		t.Fatalf("reopen %q: %v", prefix4, err)
	}
	defer reopened4.Close()
	reopened5, err := OpenFile(prefix5, cfg)
	if err != nil {
		t.Fatalf("reopen %q: %v", prefix5, err)
	}
	defer reopened5.Close()

	if reopened4.Len() != 1 {
		t.Fatalf("store4 Len = %d, want 1", reopened4.Len())
	}
	if got, ok := reopened4.Get(1); !ok || got != updated4 {
		t.Fatalf("store4 Get(1) = %+v, %v; want %+v, true", got, ok, updated4)
	}
	if _, ok := reopened4.Get(2); ok {
		t.Fatal("store4 Get(2) succeeded; contents leaked from store5")
	}

	if reopened5.Len() != 2 {
		t.Fatalf("store5 Len = %d, want 2", reopened5.Len())
	}
	if got, ok := reopened5.Get(1); !ok || got != rec5a {
		t.Fatalf("store5 Get(1) = %+v, %v; want %+v, true", got, ok, rec5a)
	}
	if got, ok := reopened5.Get(2); !ok || got != rec5b {
		t.Fatalf("store5 Get(2) = %+v, %v; want %+v, true", got, ok, rec5b)
	}

	// An empty prefix is rejected rather than silently writing bare names.
	if _, err := OpenFile("", cfg); err == nil {
		t.Fatal("OpenFile(\"\") succeeded, want error")
	}
}

// TestOpenLegacyDirectoryLayout verifies that Open keeps using the historical
// bare filenames under dir and reopens them.
func TestOpenLegacyDirectoryLayout(t *testing.T) {
	dir := t.TempDir()
	cfg := testConfig()

	st, err := Open(dir, cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	rec := randRecords(20, 1)[0]
	if id, err := st.Append(rec); err != nil || id != 1 {
		t.Fatalf("Append = %d, %v; want 1, nil", id, err)
	}
	if err := st.Checkpoint(); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	for _, name := range []string{
		manifestFilename,
		fmt.Sprintf("shard-%06d.bin", 0),
		journalFilename,
	} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatalf("expected legacy file %q: %v", name, err)
		}
	}

	reopened, err := Open(dir, cfg)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	if reopened.Len() != 1 {
		t.Fatalf("reopened Len = %d, want 1", reopened.Len())
	}
	if got, ok := reopened.Get(1); !ok || got != rec {
		t.Fatalf("reopened Get(1) = %+v, %v; want %+v, true", got, ok, rec)
	}
}
