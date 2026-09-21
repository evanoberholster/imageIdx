package timeidx

import (
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestPersistentCheckpointAndMmapReopen(t *testing.T) {
	const offset = uint64(1_000)
	directory := t.TempDir()
	index, err := NewPersistent(directory, Spec{IndexID: 9, Flags: 3, Offset: offset, Entries: []Entry{
		{Timestamp: offset + testQuantum, ImageID: 1},
		{Timestamp: offset + 2*testQuantum, ImageID: 2},
		{Timestamp: offset + 3*testQuantum, ImageID: 3},
	}}, Config{BlockSize: 2, DeltaCapacity: 8}, DurabilitySync)
	if err != nil {
		t.Fatal(err)
	}

	batch := NewBatch(3)
	batch.Append(Move(1, offset+testQuantum, offset+4*testQuantum))
	batch.Append(Delete(2, offset+2*testQuantum))
	batch.Append(Insert(4, offset+5*testQuantum))
	if err = index.Apply(batch); err != nil {
		t.Fatal(err)
	}
	if err = index.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	generation := index.Metadata().Generation
	markersPath, idsPath, blocksPath := generationPaths(directory, generation)
	for _, path := range []string{markersPath, idsPath, blocksPath, filepath.Join(directory, "manifest")} {
		info, statErr := os.Stat(path)
		if statErr != nil {
			t.Fatal(statErr)
		}
		if info.Size()%formatAlignment != 0 {
			t.Fatalf("%s size = %d, want 8-byte alignment", filepath.Base(path), info.Size())
		}
	}
	if err = index.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := OpenPersistent(directory, Config{DeltaCapacity: 8}, DurabilitySync)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	if reopened.Metadata().Generation != generation {
		t.Fatalf("generation = %d, want %d", reopened.Metadata().Generation, generation)
	}
	if reopened.Metadata().IndexID != 9 || reopened.Metadata().Flags != 3 {
		t.Fatalf("metadata = %+v", reopened.Metadata())
	}
	if len(reopened.persistence.mappings) != 2 {
		t.Fatalf("mapping count = %d, want 2", len(reopened.persistence.mappings))
	}
	got := reopened.Search(make([]ImageID, 0, 4), offset, offset+6*testQuantum)
	if !equalIDSet(got, []ImageID{1, 3, 4}) {
		t.Fatalf("reopened result = %v, want IDs 1, 3, and 4", got)
	}

	for _, entry := range reopened.directory.Load().entries {
		if entry.meta.MarkerOffset%storageAlignment != 0 || entry.meta.IDOffset%storageAlignment != 0 {
			t.Fatalf("unaligned offsets: marker=%d id=%d", entry.meta.MarkerOffset, entry.meta.IDOffset)
		}
	}
	if err = reopened.Vacuum(); err != nil {
		t.Fatal(err)
	}
	if reopened.Metadata().Generation != generation+1 {
		t.Fatalf("vacuum generation = %d, want %d", reopened.Metadata().Generation, generation+1)
	}
}

func TestPersistentStructureAlignment(t *testing.T) {
	for name, size := range map[string]int{
		"manifest":    manifestSize,
		"block entry": directoryEntrySize,
		"WAL header":  walHeaderSize,
		"WAL write":   walWriteSize,
		"image ID":    8,
	} {
		if size%formatAlignment != 0 {
			t.Fatalf("%s size = %d, want multiple of 8", name, size)
		}
	}
	if size := binary.Size(timemarker(0)); size != 4 {
		t.Fatalf("timemarker size = %d, want uint32 structure-of-arrays column", size)
	}
	if storageAlignment%formatAlignment != 0 {
		t.Fatalf("storage alignment = %d, want multiple of 8", storageAlignment)
	}
}

func TestWALRecoversWritesAfterLastCheckpoint(t *testing.T) {
	const offset = uint64(100)
	directory := t.TempDir()
	index, err := NewPersistent(directory, Spec{Offset: offset, Entries: []Entry{
		{Timestamp: offset + testQuantum, ImageID: 1},
	}}, Config{DeltaCapacity: 8}, DurabilitySync)
	if err != nil {
		t.Fatal(err)
	}
	batch := NewBatch(2)
	batch.Append(Move(1, offset+testQuantum, offset+3*testQuantum))
	batch.Append(Insert(2, offset+2*testQuantum))
	if err = index.Apply(batch); err != nil {
		t.Fatal(err)
	}
	if err = index.Close(); err != nil {
		t.Fatal(err)
	}

	recovered, err := OpenPersistent(directory, Config{DeltaCapacity: 8}, DurabilitySync)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = recovered.Close() })
	got := recovered.Search(make([]ImageID, 0, 3), offset, offset+4*testQuantum)
	if !equalIDSet(got, []ImageID{1, 2}) {
		t.Fatalf("recovered result = %v, want IDs 1 and 2", got)
	}
	old := recovered.Search(make([]ImageID, 0, 1), offset+testQuantum, offset+2*testQuantum)
	if len(old) != 0 {
		t.Fatalf("old timestamp survived WAL replay: %v", old)
	}
}

func TestWALIgnoresIncompleteTailRecord(t *testing.T) {
	directory := t.TempDir()
	index, err := NewPersistent(directory, Spec{}, Config{DeltaCapacity: 8}, DurabilitySync)
	if err != nil {
		t.Fatal(err)
	}
	batch := NewBatch(1)
	batch.Append(Insert(7, testQuantum))
	if err = index.Apply(batch); err != nil {
		t.Fatal(err)
	}
	if err = index.Close(); err != nil {
		t.Fatal(err)
	}

	wal, err := os.OpenFile(walPath(directory, 1), os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = wal.Write([]byte{1, 2, 3, 4, 5}); err != nil {
		t.Fatal(err)
	}
	if err = wal.Close(); err != nil {
		t.Fatal(err)
	}

	recovered, err := OpenPersistent(directory, Config{DeltaCapacity: 8}, DurabilityOS)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = recovered.Close() })
	got := recovered.Search(make([]ImageID, 0, 1), 0, 2*testQuantum)
	if !equalIDSet(got, []ImageID{7}) {
		t.Fatalf("result = %v, want [7]", got)
	}
}

// TestManifestVersionRejected proves unknown manifest versions fail open
// with ErrUnsupported instead of masquerading as corruption.
func TestManifestVersionRejected(t *testing.T) {
	directory := t.TempDir()
	index, err := NewPersistent(directory, Spec{Offset: 1_000}, Config{}, DurabilityOS)
	if err != nil {
		t.Fatal(err)
	}
	if err := index.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "manifest")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	data[4] ^= 0xff // version field; magic stays recognized
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenPersistent(directory, Config{}, DurabilityOS); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("versioned manifest error = %v, want ErrUnsupported", err)
	}
}

func TestManifestChecksumRejectsCorruption(t *testing.T) {
	directory := t.TempDir()
	index, err := NewPersistent(directory, Spec{}, Config{}, DurabilityOS)
	if err != nil {
		t.Fatal(err)
	}
	if err = index.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "manifest")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	data[16] ^= 0xff
	if err = os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = OpenPersistent(directory, Config{}, DurabilityOS)
	if !errors.Is(err, ErrCorruptManifest) {
		t.Fatalf("error = %v, want %v", err, ErrCorruptManifest)
	}
}

// TestVacuumRotatesWALAndRetainsLiveGeneration verifies that Vacuum rolls
// the WAL to a fresh epoch carrying the post-checkpoint tail, keeps exactly
// the published generation files, and leaves the index replayable.
func TestVacuumRotatesWALAndRetainsLiveGeneration(t *testing.T) {
	const offset = uint64(500)
	directory := t.TempDir()
	index, err := NewPersistent(directory, Spec{Offset: offset, Entries: []Entry{
		{Timestamp: offset + testQuantum, ImageID: 1},
	}}, Config{DeltaCapacity: 8}, DurabilitySync)
	if err != nil {
		t.Fatal(err)
	}
	// Two checkpoint generations plus WAL growth.
	for round := uint64(0); round < 2; round++ {
		batch := NewBatch(4)
		for k := uint64(0); k < 4; k++ {
			n := 10 + round*4 + k
			if !batch.Append(Insert(ImageID(n), offset+n*testQuantum)) {
				t.Fatal("batch append rejected")
			}
		}
		if err = index.Apply(batch); err != nil {
			t.Fatal(err)
		}
		if err = index.Checkpoint(); err != nil {
			t.Fatal(err)
		}
	}
	before := index.OnDiskBytes()
	if err = index.Vacuum(); err != nil {
		t.Fatal(err)
	}
	after := index.OnDiskBytes()
	if after >= before {
		t.Fatalf("OnDiskBytes before=%d after=%d, want vacuum to reclaim", before, after)
	}
	generation := index.Metadata().Generation
	for pattern, want := range map[string]int{
		"markers.*.base": 1, "ids.*.base": 1, "blocks.*.idx": 1, walGlob: 1,
	} {
		matches, globErr := filepath.Glob(filepath.Join(directory, pattern))
		if globErr != nil {
			t.Fatal(globErr)
		}
		if len(matches) != want {
			t.Fatalf("%s files after vacuum = %v, want %d", pattern, matches, want)
		}
	}
	markersPath, idsPath, blocksPath := generationPaths(directory, generation)
	for _, path := range []string{markersPath, idsPath, blocksPath} {
		if _, statErr := os.Stat(path); statErr != nil {
			t.Fatalf("live generation file missing: %s: %v", path, statErr)
		}
	}
	// Writes after the vacuum must survive close/reopen via the new epoch.
	batch := NewBatch(1)
	if !batch.Append(Insert(999, offset+999*testQuantum)) {
		t.Fatal("batch append rejected")
	}
	if err = index.Apply(batch); err != nil {
		t.Fatal(err)
	}
	if err = index.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenPersistent(directory, Config{DeltaCapacity: 8}, DurabilitySync)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	got := reopened.Search(make([]ImageID, 0, 16), offset, offset+1_000*testQuantum)
	if !equalIDSet(got, []ImageID{1, 10, 11, 12, 13, 14, 15, 16, 17, 999}) {
		t.Fatalf("reopened result = %v, want IDs 1, 10-17, and 999", got)
	}
	// A second vacuum must roll the epoch again without losing data.
	if err = reopened.Vacuum(); err != nil {
		t.Fatal(err)
	}
	matches, _ := filepath.Glob(filepath.Join(directory, walGlob))
	if len(matches) != 1 {
		t.Fatalf("WAL files after second vacuum = %v, want 1", matches)
	}
}

func TestOpenIgnoresUnpublishedManifestTemporary(t *testing.T) {
	directory := t.TempDir()
	index, err := NewPersistent(directory, Spec{Entries: []Entry{{Timestamp: testQuantum, ImageID: 1}}}, Config{}, DurabilityOS)
	if err != nil {
		t.Fatal(err)
	}
	if err = index.Close(); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(directory, "manifest.tmp"), []byte("partial"), 0o644); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenPersistent(directory, Config{}, DurabilityOS)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	got := reopened.Search(make([]ImageID, 0, 1), 0, 2*testQuantum)
	if !equalIDSet(got, []ImageID{1}) {
		t.Fatalf("result = %v, want [1]", got)
	}
}
