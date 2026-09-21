package hashidx

import (
	"os"
	"path/filepath"
	"testing"
)

// Crash-window matrix: what each failure point leaves behind and where it
// is covered. Every row must stay green for V1.
//
//	| failure point                  | residue                  | recovery              | test
//	|--------------------------------|--------------------------|-----------------------|--------------------------------
//	| torn WAL tail                  | partial frame            | clean stop at replay  | TestIncompleteTrailingWALBatch,
//	|                                |                          |                       | TestExactIncompleteTrailingWALBatch
//	| corrupt WAL frame              | bad CRC/counts           | ErrCorruptWAL on open | TestCorruptWALRejected,
//	|                                |                          |                       | TestExactCorruptWALRejected
//	| corrupt manifest/generation    | bad CRC/order            | ErrCorrupt on open    | TestCorrupt*Rejected
//	| unknown artifact version       | future bytes             | ErrUnsupported        | TestManifestVersionRejected
//	| mid-checkpoint crash           | *.tmp files              | ignored on open       | TestStrayTmpFilesIgnored
//	| WAL failure mid-write          | latched index            | close + reopen        | recovery_test.go
//	| checkpoint between sub-indexes | diverged generations     | next Checkpoint       | TestStoreCheckpointConvergence…
//	| repeated maintenance           | none                     | idempotent no-ops     | TestMaintenanceIdempotent
func TestStrayTmpFilesIgnored(t *testing.T) {
	dir := t.TempDir()
	store := mustNewStore(t, dir, StoreSpec{Exact: ExactSpec{Algorithm: AlgorithmSHA256}})
	if err := store.Perceptual().Apply(seedEntries(8)); err != nil {
		t.Fatal(err)
	}
	if err := store.Exact().Add(makeHash(4242), 4242); err != nil {
		t.Fatal(err)
	}
	if err := store.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	// Plant crashed-checkpoint leftovers: generation tmp files plus
	// manifest tmp files, which WriteManifest itself uses as staging.
	strays := []string{
		filepath.Join(dir, perceptualDirName, "hashidx-00000000000000000002.data.tmp"),
		filepath.Join(dir, exactDirName, "exact-00000000000000000002.idx.tmp"),
		filepath.Join(dir, perceptualDirName, "manifest.tmp"),
		filepath.Join(dir, exactDirName, "manifest.tmp"),
		filepath.Join(dir, "manifest.tmp"),
	}
	for _, path := range strays {
		if err := os.WriteFile(path, []byte("torn"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenStore(dir, DurabilityOS)
	if err != nil {
		t.Fatalf("open with stray tmp files: %v", err)
	}
	defer func() { _ = reopened.Close() }()
	if got := reopened.Perceptual().Metadata().RecordCount; got != 8 {
		t.Fatalf("records = %d, want 8", got)
	}
	if got := reopened.Exact().Lookup(nil, makeHash(4242)); len(got) != 1 {
		t.Fatalf("lookup = %v, want [4242]", got)
	}
}

func TestMaintenanceIdempotent(t *testing.T) {
	dir := t.TempDir()
	store := mustNewStore(t, dir, StoreSpec{Exact: ExactSpec{Algorithm: AlgorithmSHA256}})
	if err := store.Perceptual().Apply(seedEntries(8)); err != nil {
		t.Fatal(err)
	}
	if err := store.Exact().Add(makeHash(4242), 4242); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if err := store.Checkpoint(); err != nil {
			t.Fatal(err)
		}
		if err := store.Vacuum(nil); err != nil {
			t.Fatal(err)
		}
	}
	if got := store.Perceptual().Metadata().RecordCount; got != 8 {
		t.Fatalf("records = %d, want 8", got)
	}
	if got := store.Exact().LiveCount(); got != 1 {
		t.Fatalf("exact live = %d, want 1", got)
	}
	pGen, eGen, err := ReadStoreManifest(dir)
	if err != nil {
		t.Fatal(err)
	}
	if pGen != store.Perceptual().Metadata().Generation || eGen != store.Exact().Metadata().Generation {
		t.Fatalf("anchor (%d, %d) disagrees with live (%d, %d)", pGen, eGen,
			store.Perceptual().Metadata().Generation, store.Exact().Metadata().Generation)
	}
}
