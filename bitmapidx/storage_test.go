package bitmapidx

import (
	"path/filepath"
	"slices"
	"testing"
)

func TestPackedSegmentRoundTrip(t *testing.T) {
	epoch := NewEpoch()
	config := Config{Epoch: epoch}
	a, _ := New(BitmapKey{Type: IndexTypeTag, ValueID: 20}, 2, []uint64{7, 9}, config)
	b, _ := New(BitmapKey{Type: IndexTypeLocation, ValueID: 10}, 1, []uint64{1, 3, 5}, config)
	_ = b.Add([]uint64{8})
	path := filepath.Join(t.TempDir(), "packed.idx")
	if err := WriteSegment(path, 12, []*BitmapIndex{a, b}); err != nil {
		t.Fatal(err)
	}
	segment, err := OpenSegment(path, config)
	if err != nil {
		t.Fatal(err)
	}
	defer segment.Close()
	if segment.Generation != 12 || segment.Directory.Len() != 2 {
		t.Fatalf("bad segment metadata: %#v", segment)
	}
	loaded, ok := segment.Directory.Lookup(BitmapKey{Type: IndexTypeLocation, ValueID: 10})
	if !ok || !slices.Equal(loaded.Search(nil), []ImageID{1, 3, 5, 8}) {
		t.Fatalf("loaded values = %v", loaded.Search(nil))
	}
}

func TestWALRecoveryCheckpointAndVacuum(t *testing.T) {
	dir := t.TempDir()
	key := BitmapKey{Type: IndexTypeAlbum, ValueID: 44}
	store, err := NewPersistent(dir, []IndexSpec{{Key: key, IndexID: 5, ImageIDs: []uint64{1, 2}}}, Config{}, DurabilitySync)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.Write(key, BitmapMutation{Additions: []uint64{3, 4}, Removals: []uint64{1}}); err != nil {
		t.Fatal(err)
	}
	if err = store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := OpenPersistent(dir, Config{}, DurabilityOS)
	if err != nil {
		t.Fatal(err)
	}
	index, ok := reopened.Directory.Lookup(key)
	if !ok || !slices.Equal(index.Search(nil), []ImageID{2, 3, 4}) {
		t.Fatalf("recovered values = %v", index.Search(nil))
	}
	if err = reopened.Vacuum(); err != nil {
		t.Fatal(err)
	}
	if err = reopened.Close(); err != nil {
		t.Fatal(err)
	}

	again, err := OpenPersistent(dir, Config{}, DurabilityOS)
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	index, _ = again.Directory.Lookup(key)
	if got := index.Search(nil); !slices.Equal(got, []ImageID{2, 3, 4}) {
		t.Fatalf("checkpointed values = %v", got)
	}
}
