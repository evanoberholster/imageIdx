package radixhash

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestMmapCheckpointReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "idx.bin")
	config := Config{}

	index, err := NewMmap(path, 4<<20, config)
	if err != nil {
		t.Fatalf("NewMmap: %v", err)
	}
	hash := hashWithPrefix([]byte{1, 2}, 3)
	var entries []Entry
	for id := ImageID(1); id <= 50; id++ {
		entries = append(entries, Entry{Hash: hash, ImageID: id})
	}
	other := hashWithPrefix([]byte{9, 9}, 9)
	entries = append(entries, Entry{Hash: other, ImageID: 51})
	if err := index.Apply(entries); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	before := index.Metadata()
	if err := index.Checkpoint(); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	if err := index.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	reopened, err := OpenMmap(path, config)
	if err != nil {
		t.Fatalf("OpenMmap: %v", err)
	}
	defer reopened.Close()
	meta := reopened.Metadata()
	if meta.Generation != before.Generation {
		t.Fatalf("generation = %d, want %d", meta.Generation, before.Generation)
	}
	if meta.Entries != before.Entries {
		t.Fatalf("entries = %d, want %d", meta.Entries, before.Entries)
	}
	if got := reopened.Lookup(nil, hash); !equalIDs(got, idsRange(1, 50)) {
		t.Fatalf("Lookup(hash) = %v", got)
	}
	if got := reopened.Lookup(nil, other); !equalIDs(got, []ImageID{51}) {
		t.Fatalf("Lookup(other) = %v", got)
	}

	// Apply again, checkpoint, and reopen a second time.
	if err := reopened.Apply([]Entry{{Hash: hash, ImageID: 100}}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if err := reopened.Checkpoint(); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	third, err := OpenMmap(path, config)
	if err != nil {
		t.Fatalf("OpenMmap third: %v", err)
	}
	defer third.Close()
	want := append(idsRange(1, 50), 100)
	if got := third.Lookup(nil, hash); !equalIDs(got, want) {
		t.Fatalf("Lookup after second reopen = %v, want %v", got, want)
	}
}

func TestMmapReopenReclaimsRetired(t *testing.T) {
	path := filepath.Join(t.TempDir(), "idx.bin")
	index, err := NewMmap(path, 4<<20, Config{})
	if err != nil {
		t.Fatalf("NewMmap: %v", err)
	}
	hash := hashWithPrefix([]byte{4, 4}, 4)
	// Two batches retire the first leaf and its L1 node.
	if err := index.Apply([]Entry{{Hash: hash, ImageID: 1}}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if err := index.Apply([]Entry{{Hash: hash, ImageID: 2}}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if index.Metadata().RetiredExtents == 0 {
		t.Fatalf("expected retired extents before checkpoint")
	}
	usedBefore := index.Metadata().ArenaBytes - index.Metadata().FreeBytes
	if err := index.Checkpoint(); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	if err := index.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	reopened, err := OpenMmap(path, Config{})
	if err != nil {
		t.Fatalf("OpenMmap: %v", err)
	}
	defer reopened.Close()
	if reopened.Metadata().RetiredExtents != 0 {
		t.Fatalf("retired after reopen = %d, want 0", reopened.Metadata().RetiredExtents)
	}
	usedAfter := reopened.Metadata().ArenaBytes - reopened.Metadata().FreeBytes
	if usedAfter >= usedBefore {
		t.Fatalf("used after reopen = %d, want < %d (retired space reclaimed)", usedAfter, usedBefore)
	}
	if got := reopened.Lookup(nil, hash); !equalIDs(got, []ImageID{1, 2}) {
		t.Fatalf("Lookup = %v", got)
	}
}

func TestMmapReopenPromotedTree(t *testing.T) {
	path := filepath.Join(t.TempDir(), "idx.bin")
	config := Config{PromotionBytes: 1 << 30, MaxLeafSlots: 32}
	index, err := NewMmap(path, 8<<20, config)
	if err != nil {
		t.Fatalf("NewMmap: %v", err)
	}
	var entries []Entry
	for i := 0; i < 300; i++ {
		entries = append(entries, Entry{Hash: hashWithPrefix([]byte{7, 9}, uint64(i)+1), ImageID: ImageID(i + 1)})
	}
	if err := index.Apply(entries); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	depth := index.Metadata().Depth
	if depth < 3 {
		t.Fatalf("depth = %d, want >= 3", depth)
	}
	if err := index.Checkpoint(); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	if err := index.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	reopened, err := OpenMmap(path, config)
	if err != nil {
		t.Fatalf("OpenMmap: %v", err)
	}
	defer reopened.Close()
	if got := reopened.Metadata().Depth; got != depth {
		t.Fatalf("depth after reopen = %d, want %d", got, depth)
	}
	for i := 0; i < 300; i++ {
		hash := hashWithPrefix([]byte{7, 9}, uint64(i)+1)
		if got := reopened.Lookup(nil, hash); !equalIDs(got, []ImageID{ImageID(i + 1)}) {
			t.Fatalf("Lookup(%d) after reopen = %v", i, got)
		}
	}
}

func TestMmapReopenEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "idx.bin")
	index, err := NewMmap(path, 1<<20, Config{})
	if err != nil {
		t.Fatalf("NewMmap: %v", err)
	}
	if err := index.Checkpoint(); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	if err := index.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	reopened, err := OpenMmap(path, Config{})
	if err != nil {
		t.Fatalf("OpenMmap: %v", err)
	}
	defer reopened.Close()
	if meta := reopened.Metadata(); meta.Entries != 0 || meta.Depth != 0 {
		t.Fatalf("empty reopen metadata = %+v", meta)
	}
}

func TestMmapReopenPreservesLargeImageIDs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "idx.bin")
	index, err := NewMmap(path, 1<<20, Config{})
	if err != nil {
		t.Fatalf("NewMmap: %v", err)
	}
	hash := hashWithPrefix([]byte{5, 5}, 5)
	big := ImageID(1) << 40
	if err := index.Apply([]Entry{{Hash: hash, ImageID: big}, {Hash: hash, ImageID: ^ImageID(0)}}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if err := index.Checkpoint(); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	if err := index.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	reopened, err := OpenMmap(path, Config{})
	if err != nil {
		t.Fatalf("OpenMmap: %v", err)
	}
	defer reopened.Close()
	if got := reopened.Lookup(nil, hash); !equalIDs(got, []ImageID{big, ^ImageID(0)}) {
		t.Fatalf("Lookup = %v, want [%d %d]", got, big, ^ImageID(0))
	}
}

func TestOpenMmapRejectsCorruption(t *testing.T) {
	path := filepath.Join(t.TempDir(), "idx.bin")
	index, err := NewMmap(path, 1<<20, Config{})
	if err != nil {
		t.Fatalf("NewMmap: %v", err)
	}
	if err := index.Apply([]Entry{{Hash: hashWithPrefix([]byte{1, 1}, 1), ImageID: 1}}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if err := index.Checkpoint(); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	if err := index.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	file, err := os.OpenFile(path, os.O_RDWR, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	// Corrupt both superblock slots.
	if _, err := file.WriteAt([]byte{0, 0, 0, 0}, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteAt([]byte{0, 0, 0, 0}, int64(superblockBytes)); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenMmap(path, Config{}); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("OpenMmap(corrupt) = %v, want ErrCorrupt", err)
	}
}

func TestOpenMmapRejectsForeignFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "foreign.bin")
	if err := os.WriteFile(path, make([]byte, 1<<20), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenMmap(path, Config{}); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("OpenMmap(foreign) = %v, want ErrCorrupt", err)
	}
}

// idsRange returns [first, last] as IDs.
func idsRange(first, last ImageID) []ImageID {
	out := make([]ImageID, 0, int(last-first)+1)
	for id := first; id <= last; id++ {
		out = append(out, id)
	}
	return out
}

// TestMmapReopenAfterSparsePromotion checkpoints a sparse Root->Leaf bucket,
// reopens it, grows it past the promotion threshold, and reopens the promoted
// Root->Radix->Leaf tree.
func TestMmapReopenAfterSparsePromotion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "idx.bin")
	config := Config{PromotionBytes: 2048}

	index, err := NewMmap(path, 8<<20, config)
	if err != nil {
		t.Fatalf("NewMmap: %v", err)
	}
	seed := func(from, to int) []Entry {
		var entries []Entry
		for i := from; i < to; i++ {
			entries = append(entries, Entry{Hash: hashWithPrefix([]byte{7, 9}, uint64(i+1)), ImageID: ImageID(i + 1)})
		}
		return entries
	}
	if err := index.Apply(seed(0, 10)); err != nil {
		t.Fatalf("seed Apply: %v", err)
	}
	if depth := index.Metadata().Depth; depth != 1 {
		t.Fatalf("depth = %d, want 1 (sparse)", depth)
	}
	if err := index.Checkpoint(); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	if err := index.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	reopened, err := OpenMmap(path, config)
	if err != nil {
		t.Fatalf("OpenMmap (sparse): %v", err)
	}
	if depth := reopened.Metadata().Depth; depth != 1 {
		t.Fatalf("reopened depth = %d, want 1", depth)
	}
	if err := reopened.Apply(seed(10, 600)); err != nil {
		t.Fatalf("promoting Apply: %v", err)
	}
	if depth := reopened.Metadata().Depth; depth < 2 {
		t.Fatalf("depth after promotion = %d, want >= 2", depth)
	}
	if err := reopened.Checkpoint(); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	promoted, err := OpenMmap(path, config)
	if err != nil {
		t.Fatalf("OpenMmap (promoted): %v", err)
	}
	defer promoted.Close()
	for i := 0; i < 600; i++ {
		h := hashWithPrefix([]byte{7, 9}, uint64(i+1))
		if got := promoted.Lookup(nil, h); !equalIDs(got, []ImageID{ImageID(i + 1)}) {
			t.Fatalf("Lookup(%d) after promotion reopen = %v", i, got)
		}
	}
}

// TestDecodeRadixSuperblockRejectsShortSlot verifies a truncated slot slice is
// rejected instead of being validated against the full 512-byte size.
func TestDecodeRadixSuperblockRejectsShortSlot(t *testing.T) {
	if _, ok := decodeRadixSuperblock(make([]byte, superblockBytes-1), nil); ok {
		t.Fatal("short superblock slot accepted")
	}
	if _, ok := decodeRadixSuperblock(nil, nil); ok {
		t.Fatal("nil superblock slot accepted")
	}
}

// TestMmapCorruptNewestSlotFallsBack corrupts the newest checkpoint's payload
// and verifies reopen selects the older valid generation instead of failing.
func TestMmapCorruptNewestSlotFallsBack(t *testing.T) {
	path := filepath.Join(t.TempDir(), "idx.bin")
	index, err := NewMmap(path, 1<<20, Config{})
	if err != nil {
		t.Fatalf("NewMmap: %v", err)
	}
	h := hashWithPrefix([]byte{1, 2}, 3)
	if err := index.Apply([]Entry{{Hash: h, ImageID: 1}}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if err := index.Checkpoint(); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	olderGen := index.Generation()
	if err := index.Apply([]Entry{{Hash: h, ImageID: 2}}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if err := index.Checkpoint(); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	newestGen := index.Generation()
	newestSlot := index.slot
	if newestGen == olderGen {
		t.Fatalf("checkpoints did not advance generation: %d", newestGen)
	}
	if err := index.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	file, err := os.OpenFile(path, os.O_RDWR, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	// Flip a payload byte in the newest slot so its CRC fails.
	if _, err := file.WriteAt([]byte{0xff}, int64(superblockBytes*newestSlot)+32); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := OpenMmap(path, Config{})
	if err != nil {
		t.Fatalf("OpenMmap(fallback): %v", err)
	}
	defer reopened.Close()
	if got := reopened.Generation(); got != olderGen {
		t.Fatalf("fallback generation = %d, want %d", got, olderGen)
	}
	if got := reopened.Lookup(nil, h); !equalIDs(got, []ImageID{1}) {
		t.Fatalf("fallback lookup = %v, want [1]", got)
	}
}
