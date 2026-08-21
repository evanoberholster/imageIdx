package geoindex

import (
	"path/filepath"
	"slices"
	"testing"
)

func TestGenerationRoundTrip(t *testing.T) {
	keyA := keyAt(t, 48.8566, 2.3522)
	keyB := keyAt(t, 40.7128, -74.0060)
	entries := []GeoCellEntry{{keyA, 1}, {keyB, 2}, {keyB, 3}, {keyB, 4}, {keyB, 5}, {keyB, 6}}
	index, err := NewFromCells(7, entries, testConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer index.Close()
	path := filepath.Join(t.TempDir(), "geo.base")
	if err = writeGeoGeneration(path, 12, index.LoadView()); err != nil {
		t.Fatal(err)
	}
	loaded, storageGeneration, err := readGeoGeneration(path, testConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer loaded.Close()
	if storageGeneration != 12 || loaded.IndexID() != 7 {
		t.Fatalf("metadata: storage=%d index=%d", storageGeneration, loaded.IndexID())
	}
	got := loaded.All(nil)
	slices.Sort(got)
	if !slices.Equal(got, []ImageID{1, 2, 3, 4, 5, 6}) {
		t.Fatalf("round trip = %v", got)
	}
}

func TestWALRecoveryCheckpointAndVacuum(t *testing.T) {
	dir := t.TempDir()
	index, err := NewPersistent(dir, 42, []GeoEntry{{1, 37.7749, -122.4194}}, testConfig(), DurabilitySync)
	if err != nil {
		t.Fatal(err)
	}
	if err = index.AddBatch([]GeoEntry{{2, 34.0522, -118.2437}}); err != nil {
		t.Fatal(err)
	}
	if err = index.UpdateBatch([]GeoUpdate{{1, 40.7128, -74.0060}}); err != nil {
		t.Fatal(err)
	}
	if err = index.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := OpenPersistent(dir, testConfig(), DurabilityOS)
	if err != nil {
		t.Fatal(err)
	}
	got := reopened.All(nil)
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
	got = again.All(nil)
	slices.Sort(got)
	if !slices.Equal(got, []ImageID{1, 2}) {
		t.Fatalf("checkpoint = %v", got)
	}
}
