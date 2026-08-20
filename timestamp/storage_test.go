package timestamp

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestPersistentCheckpointAndMmapReopen(t *testing.T) {
	const offset = uint64(1_000)
	directory := t.TempDir()
	index, err := NewPersistent(directory, []Record{
		{Timestamp: offset + testQuantum, ImageID: 1},
		{Timestamp: offset + 2*testQuantum, ImageID: 2},
		{Timestamp: offset + 3*testQuantum, ImageID: 3},
	}, offset, Config{BlockSize: 2, DeltaCapacity: 8, Durability: DurabilitySync})
	if err != nil {
		t.Fatal(err)
	}

	batch := NewWriteBatch(4)
	batch.Append(TimestampWrite{Timestamp: offset + testQuantum, ImageID: 1, Op: WriteDelete})
	batch.Append(TimestampWrite{Timestamp: offset + 4*testQuantum, ImageID: 1, Op: WriteAdd})
	batch.Append(TimestampWrite{Timestamp: offset + 2*testQuantum, ImageID: 2, Op: WriteDelete})
	batch.Append(TimestampWrite{Timestamp: offset + 5*testQuantum, ImageID: 4, Op: WriteAdd})
	if err = index.Submit(batch); err != nil {
		t.Fatal(err)
	}
	if err = index.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	generation := index.Header.Generation
	if err = index.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := OpenPersistent(directory, Config{DeltaCapacity: 8, Durability: DurabilitySync})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	if reopened.Header.Generation != generation {
		t.Fatalf("generation = %d, want %d", reopened.Header.Generation, generation)
	}
	if len(reopened.mappings) != 2 {
		t.Fatalf("mapping count = %d, want 2", len(reopened.mappings))
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
	if reopened.Header.Generation != generation+1 {
		t.Fatalf("vacuum generation = %d, want %d", reopened.Header.Generation, generation+1)
	}
}

func TestWALRecoversWritesAfterLastCheckpoint(t *testing.T) {
	const offset = uint64(100)
	directory := t.TempDir()
	index, err := NewPersistent(directory, []Record{
		{Timestamp: offset + testQuantum, ImageID: 1},
	}, offset, Config{DeltaCapacity: 8, Durability: DurabilitySync})
	if err != nil {
		t.Fatal(err)
	}
	batch := NewWriteBatch(3)
	batch.Append(TimestampWrite{Timestamp: offset + testQuantum, ImageID: 1, Op: WriteDelete})
	batch.Append(TimestampWrite{Timestamp: offset + 3*testQuantum, ImageID: 1, Op: WriteAdd})
	batch.Append(TimestampWrite{Timestamp: offset + 2*testQuantum, ImageID: 2, Op: WriteAdd})
	if err = index.Submit(batch); err != nil {
		t.Fatal(err)
	}
	if err = index.Close(); err != nil {
		t.Fatal(err)
	}

	recovered, err := OpenPersistent(directory, Config{DeltaCapacity: 8, Durability: DurabilitySync})
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
	index, err := NewPersistent(directory, nil, 0, Config{DeltaCapacity: 8, Durability: DurabilitySync})
	if err != nil {
		t.Fatal(err)
	}
	batch := NewWriteBatch(1)
	batch.Append(TimestampWrite{Timestamp: testQuantum, ImageID: 7, Op: WriteAdd})
	if err = index.Submit(batch); err != nil {
		t.Fatal(err)
	}
	if err = index.Close(); err != nil {
		t.Fatal(err)
	}

	wal, err := os.OpenFile(filepath.Join(directory, walFilename), os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = wal.Write([]byte{1, 2, 3, 4, 5}); err != nil {
		t.Fatal(err)
	}
	if err = wal.Close(); err != nil {
		t.Fatal(err)
	}

	recovered, err := OpenPersistent(directory, Config{DeltaCapacity: 8})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = recovered.Close() })
	got := recovered.Search(make([]ImageID, 0, 1), 0, 2*testQuantum)
	if !equalIDSet(got, []ImageID{7}) {
		t.Fatalf("result = %v, want [7]", got)
	}
}

func TestManifestChecksumRejectsCorruption(t *testing.T) {
	directory := t.TempDir()
	index, err := NewPersistent(directory, nil, 0, Config{})
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
	_, err = OpenPersistent(directory, Config{})
	if !errors.Is(err, ErrCorruptManifest) {
		t.Fatalf("error = %v, want %v", err, ErrCorruptManifest)
	}
}

func TestOpenIgnoresUnpublishedManifestTemporary(t *testing.T) {
	directory := t.TempDir()
	index, err := NewPersistent(directory, []Record{{Timestamp: testQuantum, ImageID: 1}}, 0, Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err = index.Close(); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(directory, "manifest.tmp"), []byte("partial"), 0o644); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenPersistent(directory, Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	got := reopened.Search(make([]ImageID, 0, 1), 0, 2*testQuantum)
	if !equalIDSet(got, []ImageID{1}) {
		t.Fatalf("result = %v, want [1]", got)
	}
}
