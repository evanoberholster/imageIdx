package bitmapidx

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestPackedSegmentRoundTrip(t *testing.T) {
	config := Config{}
	a, _ := New(Spec{Key: Key{Type: IndexTypeTag, ValueID: 20}, IndexID: 2, IDs: []ImageID{7, 9}}, config)
	b, _ := New(Spec{Key: Key{Type: IndexTypeLocation, ValueID: 10}, IndexID: 1, IDs: []ImageID{1, 3, 5}}, config)
	_ = b.Apply(Mutation{Add: []ImageID{8}})
	path := filepath.Join(t.TempDir(), "packed.idx")
	if err := writeSegment(path, 12, []*Index{a, b}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size()%8 != 0 {
		t.Fatalf("segment size = %d, want 8-byte alignment", info.Size())
	}
	segment, err := openSegment(path, config)
	if err != nil {
		t.Fatal(err)
	}
	defer segment.close()
	if segment.generation != 12 || segment.directory.Len() != 2 {
		t.Fatalf("bad segment metadata: %#v", segment)
	}
	loaded, ok := segment.directory.Lookup(Key{Type: IndexTypeLocation, ValueID: 10})
	if !ok || !slices.Equal(loaded.AppendTo(nil), []ImageID{1, 3, 5, 8}) {
		t.Fatalf("loaded values = %v", loaded.AppendTo(nil))
	}
}

func TestPersistentStructureAlignment(t *testing.T) {
	for name, size := range map[string]int{
		"segment header": segmentHeaderSize,
		"segment entry":  segmentEntrySize,
		"manifest":       bitmapManifestSize,
		"WAL header":     bitmapWALHeaderSize,
	} {
		if size%8 != 0 {
			t.Fatalf("%s size = %d, want multiple of 8", name, size)
		}
	}
	if segmentAlignment%8 != 0 {
		t.Fatalf("segment alignment = %d, want multiple of 8", segmentAlignment)
	}
}

func TestPersistentStoreSharesReaderTracker(t *testing.T) {
	store, err := NewPersistent(t.TempDir(), []Spec{
		{Key: Key{Type: IndexTypeTag, ValueID: 1}, IndexID: 1},
		{Key: Key{Type: IndexTypeTag, ValueID: 2}, IndexID: 2},
	}, Config{}, DurabilityOS)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	indexes := store.Indexes()
	if len(indexes) != 2 {
		t.Fatalf("got %d indexes, want 2", len(indexes))
	}
	if store.readers == nil || indexes[0].readers != store.readers || indexes[1].readers != store.readers {
		t.Fatal("persistent indexes do not share their store's reader tracker")
	}
}

func TestWALRecoveryCheckpointAndVacuum(t *testing.T) {
	dir := t.TempDir()
	key := Key{Type: IndexTypeAlbum, ValueID: 44}
	store, err := NewPersistent(dir, []Spec{{Key: key, IndexID: 5, Flags: 7, IDs: []ImageID{1, 2}}}, Config{}, DurabilitySync)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.Apply(key, Mutation{Add: []ImageID{3, 4}, Remove: []ImageID{1}}); err != nil {
		t.Fatal(err)
	}
	if err = store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := OpenPersistent(dir, Config{}, DurabilityOS)
	if err != nil {
		t.Fatal(err)
	}
	index, ok := reopened.Lookup(key)
	if !ok || !slices.Equal(index.AppendTo(nil), []ImageID{2, 3, 4}) {
		t.Fatalf("recovered values = %v", index.AppendTo(nil))
	}
	if metadata := index.Metadata(); metadata.Key != key || metadata.IndexID != 5 || metadata.Flags != 7 {
		t.Fatalf("recovered metadata = %+v", metadata)
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
	index, _ = again.Lookup(key)
	if got := index.AppendTo(nil); !slices.Equal(got, []ImageID{2, 3, 4}) {
		t.Fatalf("checkpointed values = %v", got)
	}
}
