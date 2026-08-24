package geoindex

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestGenerationRoundTrip(t *testing.T) {
	keyA := keyAt(t, 48.8566, 2.3522)
	keyB := keyAt(t, 40.7128, -74.0060)
	index := testIndexFromCells(t, 7, map[S2Key][]ImageID{
		keyA: {1},
		keyB: {2, 3, 4, 5, 6},
	})
	defer index.Close()
	path := filepath.Join(t.TempDir(), "geo.base")
	if err := writeGeoGeneration(path, 12, index.view.Load()); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(data)%8 != 0 {
		t.Fatalf("generation size = %d, want 8-byte alignment", len(data))
	}
	for _, offset := range []uint64{
		binary.LittleEndian.Uint64(data[56:64]),
		binary.LittleEndian.Uint64(data[64:72]),
		binary.LittleEndian.Uint64(data[72:80]),
		binary.LittleEndian.Uint64(data[80:88]),
		binary.LittleEndian.Uint64(data[88:96]),
	} {
		if offset%8 != 0 {
			t.Fatalf("generation offset = %d, want 8-byte alignment", offset)
		}
	}
	loaded, storageGeneration, err := readGeoGeneration(path, testConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer loaded.Close()
	if storageGeneration != 12 || loaded.Metadata().IndexID != 7 {
		t.Fatalf("metadata: storage=%d index=%d", storageGeneration, loaded.Metadata().IndexID)
	}
	got := loaded.AppendTo(nil)
	slices.Sort(got)
	if !slices.Equal(got, []ImageID{1, 2, 3, 4, 5, 6}) {
		t.Fatalf("round trip = %v", got)
	}
}

func TestPersistentStructureAlignment(t *testing.T) {
	for name, size := range map[string]int{
		"generation header": geoHeaderSize,
		"bitmap metadata":   geoBitmapMetaSize,
		"manifest":          geoManifestSize,
		"WAL header":        geoWALHeaderSize,
		"WAL mutation":      geoWALMutationSize,
	} {
		if size%8 != 0 {
			t.Fatalf("%s size = %d, want multiple of 8", name, size)
		}
	}
	if geoStorageAlignment%8 != 0 {
		t.Fatalf("storage alignment = %d, want multiple of 8", geoStorageAlignment)
	}
}

func TestWALRecoveryCheckpointAndVacuum(t *testing.T) {
	dir := t.TempDir()
	keySF := keyAt(t, 37.7749, -122.4194)
	keyLA := keyAt(t, 34.0522, -118.2437)
	keyNY := keyAt(t, 40.7128, -74.0060)
	index, err := NewPersistent(dir, Spec{IndexID: 42, Entries: []Entry{{1, 37.7749, -122.4194}}}, testConfig(), DurabilitySync)
	if err != nil {
		t.Fatal(err)
	}
	if err = index.Apply([]Mutation{{ImageID: 2, To: keyLA}}); err != nil {
		t.Fatal(err)
	}
	if err = index.Apply([]Mutation{{ImageID: 1, From: keySF, To: keyNY}}); err != nil {
		t.Fatal(err)
	}
	if err = index.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := OpenPersistent(dir, testConfig(), DurabilityOS)
	if err != nil {
		t.Fatal(err)
	}
	got := reopened.AppendTo(nil)
	slices.Sort(got)
	if !slices.Equal(got, []ImageID{1, 2}) {
		t.Fatalf("recovered = %v", got)
	}
	if err = reopened.Vacuum(); err != nil {
		t.Fatal(err)
	}
	if err = reopened.Close(); err != nil {
		t.Fatal(err)
	}

	again, err := OpenPersistent(dir, testConfig(), DurabilityOS)
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	got = again.AppendTo(nil)
	slices.Sort(got)
	if !slices.Equal(got, []ImageID{1, 2}) {
		t.Fatalf("checkpoint = %v", got)
	}
}
