package timeidx

import (
	"os"
	"path/filepath"
	"testing"

	common "github.com/evanoberholster/imageIdx/common"
)

// Crash-window matrix: what each failure point leaves behind and where it
// is covered. Every row must stay green for V1.
//
//	| failure point                  | residue                  | recovery              | test
//	|--------------------------------|--------------------------|-----------------------|--------------------------------
//	| torn WAL tail                  | partial frame            | clean stop at replay  | TestWALIgnoresIncompleteTailRecord
//	| corrupt WAL frame              | bad CRC/counts/op        | ErrCorruptWAL on open | TestCorruptWALRejected (storage)
//	| corrupt manifest/generation    | bad CRC/order            | ErrCorruptManifest    | TestManifestChecksumRejects…,
//	|                                |                          |                       | TestCorruptGenerationHeader…
//	| unknown manifest version       | future bytes             | ErrUnsupported        | TestManifestVersionRejected
//	| mid-checkpoint crash           | *.tmp files              | ignored on open       | TestStrayTmpFilesIgnored
//	| WAL failure mid-write          | latched index            | close + reopen        | TestFailedWALRequiresRecovery
//	| repeated maintenance           | none                     | idempotent no-ops     | TestMaintenanceIdempotent
func TestStrayTmpFilesIgnored(t *testing.T) {
	const offset = uint64(1_000)
	directory := t.TempDir()
	index, err := NewStore(directory, Spec{Offset: offset, Entries: []Entry{
		{Timestamp: offset + testQuantum, ImageID: 1},
	}}, Config{BlockSize: 2, DeltaCapacity: 8}, DurabilityOS)
	if err != nil {
		t.Fatal(err)
	}
	if err := index.Close(); err != nil {
		t.Fatal(err)
	}
	// Plant crashed-checkpoint leftovers: generation temps plus a manifest
	// temp. Open follows only published names, so all of these are inert.
	markersPath, idsPath, blocksPath := generationPaths(directory, 2)
	for _, path := range []string{
		markersPath + ".tmp", idsPath + ".tmp", blocksPath + ".tmp",
		filepath.Join(directory, "manifest.tmp"),
	} {
		if err := os.WriteFile(path, []byte("torn"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	reopened, err := OpenStore(directory, Config{}, DurabilityOS)
	if err != nil {
		t.Fatalf("open with stray tmp files: %v", err)
	}
	defer func() { _ = reopened.Close() }()
	got := reopened.Search(nil, offset, offset+2*testQuantum)
	if !equalIDSet(got, []common.ValueID{1}) {
		t.Fatalf("reopened search = %v, want [1]", got)
	}
}

func TestMaintenanceIdempotent(t *testing.T) {
	const offset = uint64(1_000)
	directory := t.TempDir()
	index, err := NewStore(directory, Spec{Offset: offset}, Config{BlockSize: 2, DeltaCapacity: 8}, DurabilityOS)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = index.Close() }()
	batch := NewBatch(2)
	batch.Append(Insert(1, offset+testQuantum))
	batch.Append(Insert(2, offset+2*testQuantum))
	if err := index.Apply(batch); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if err := index.Checkpoint(); err != nil {
			t.Fatal(err)
		}
		if err := index.Vacuum(); err != nil {
			t.Fatal(err)
		}
	}
	got := index.Search(nil, offset, offset+3*testQuantum)
	if !equalIDSet(got, []common.ValueID{1, 2}) {
		t.Fatalf("search after repeated maintenance = %v, want [1 2]", got)
	}
}
