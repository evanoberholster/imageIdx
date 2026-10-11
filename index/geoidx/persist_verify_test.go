package geoidx

import (
	"encoding/binary"
	"errors"
	"testing"

	"github.com/evanoberholster/imageIdx/internal/storage"
)

// TestDecodeDirectoryBounds exercises the directory and entry bound checks with
// hostile offsets and counts. Every invalid input must return ErrCorrupt, never
// panic.
func TestDecodeDirectoryBounds(t *testing.T) {
	arena := make([]byte, 8*int(storage.SuperblockSlotBytes))
	size := uint64(len(arena))
	// A directory that fits entirely (aligned, in range) with a single slot.
	lastAligned := (size - dirEntryBytes) / uint64(storage.SuperblockSlotBytes) * uint64(storage.SuperblockSlotBytes)

	cases := []struct {
		name  string
		off   uint64
		count uint32
	}{
		{"max-offset", ^uint64(0), 1},
		{"reserved-zero", 0, 1},
		{"reserved-slot1", uint64(storage.SuperblockSlotBytes), 1},
		{"unaligned", 2*uint64(storage.SuperblockSlotBytes) + 1, 1},
		{"at-eof", size, 1},
		{"before-eof-unaligned", size - dirEntryBytes, 1},
		{"past-eof", 2 * uint64(storage.SuperblockSlotBytes), uint32((size-2*uint64(storage.SuperblockSlotBytes))/dirEntryBytes) + 1},
		{"max-count", 2 * uint64(storage.SuperblockSlotBytes), ^uint32(0)},
	}
	for _, tc := range cases {
		if _, err := decodeDirectory(arena, tc.off, tc.count); !errors.Is(err, ErrCorrupt) {
			t.Fatalf("%s: err = %v, want ErrCorrupt", tc.name, err)
		}
	}
	// An aligned, in-range directory passes placement but the zeroed entry is
	// corrupt (count == 0).
	if _, err := decodeDirectory(arena, lastAligned, 1); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("zeroed entry: err = %v, want ErrCorrupt", err)
	}
	// Zero entries is the empty representation, not an error.
	if shards, err := decodeDirectory(arena, ^uint64(0), 0); err != nil || shards != nil {
		t.Fatalf("zero-entry directory = %v, %v; want nil, nil", shards, err)
	}

	entry := make([]byte, dirEntryBytes)
	binary.LittleEndian.PutUint64(entry[16:24], 2*uint64(storage.SuperblockSlotBytes))
	binary.LittleEndian.PutUint32(entry[24:28], ^uint32(0)) // count -> length > uint32
	if _, err := decodeDirEntry(arena, entry); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("oversized count: err = %v, want ErrCorrupt", err)
	}
	binary.LittleEndian.PutUint64(entry[16:24], ^uint64(0)) // offset
	binary.LittleEndian.PutUint32(entry[24:28], 1)
	if _, err := decodeDirEntry(arena, entry); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("oversized offset: err = %v, want ErrCorrupt", err)
	}
	binary.LittleEndian.PutUint64(entry[16:24], 2*uint64(storage.SuperblockSlotBytes))
	binary.LittleEndian.PutUint32(entry[24:28], 0)
	if _, err := decodeDirEntry(arena, entry); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("zero count: err = %v, want ErrCorrupt", err)
	}
}

// slotLive returns the live extent set referenced by one superblock slot.
func slotLive(index *Index, slot int) ([]extentRef, bool) {
	off := superblockOffset(slot)
	if off+superblockBytes > uint64(len(index.arena.data)) {
		return nil, false
	}
	_, payload, ok := storage.DecodeSuperblock(index.arena.data[off:off+superblockBytes], geoidxMagic, geoidxVersion)
	if !ok {
		return nil, false
	}
	shards, dirOffset, dirCount, err := decodeGeneration(index.arena.data, payload, uint64(len(index.arena.data)))
	if err != nil {
		return nil, false
	}
	return liveOf(shards, dirOffset, uint32(dirCount)*dirEntryBytes), true
}

// recoverableSet is the union of every live extent referenced by a valid slot.
func recoverableSet(index *Index) map[extentRef]bool {
	set := map[extentRef]bool{}
	for slot := 0; slot < superblockSlots; slot++ {
		if live, ok := slotLive(index, slot); ok {
			for _, e := range live {
				set[e] = true
			}
		}
	}
	return set
}

func extentSet(live []extentRef) map[extentRef]bool {
	set := make(map[extentRef]bool, len(live))
	for _, e := range live {
		set[e] = true
	}
	return set
}

// assertProtected requires every recoverable extent to be protected by index.live
// (and therefore marked used by the reconstructed allocator).
func assertProtected(t *testing.T, index *Index) {
	t.Helper()
	protected := extentSet(index.live)
	for e := range recoverableSet(index) {
		if !protected[e] {
			t.Fatalf("recoverable extent ref=%d size=%d is not protected", e.ref, e.size)
		}
	}
}

// TestUncertainExtentProtection proves uncertain candidate extents stay protected
// across repeated uncertain commits, that a durable retry releases only the
// now-unreachable candidates, and that recovery reconstructs the exact protected
// set. It runs with and without forced growth.
func TestUncertainExtentProtection(t *testing.T) {
	run := func(t *testing.T, cfg Config, seed, batch int) {
		dir := t.TempDir()
		index, err := New(dir, cfg)
		if err != nil {
			t.Fatal(err)
		}
		insertChunked(t, index, shardedMemoryEntries(seed))
		if err := index.Checkpoint(); err != nil {
			t.Fatal(err)
		}
		insertChunked(t, index, withNewIDs(shardedMemoryEntries(batch), 1_000_000))

		original := syncArenaFunc
		syncArenaFunc = func(*storage.MappedFile) error { return errors.New("sync") }
		if err := index.Compact(); err == nil {
			t.Fatal("attempt 1 should be uncertain")
		}
		assertProtected(t, index)
		if err := index.Compact(); err == nil {
			t.Fatal("attempt 2 should be uncertain")
		}
		assertProtected(t, index)
		syncArenaFunc = original

		if err := index.Compact(); err != nil {
			t.Fatalf("successful retry: %v", err)
		}
		assertProtected(t, index)
		// After a durable commit the protected set must equal exactly the two
		// recoverable generations (no stale uncertain candidates retained).
		if got, want := extentSet(index.live), recoverableSet(index); len(got) != len(want) {
			t.Fatalf("protected %d != recoverable %d after durable commit", len(got), len(want))
		}

		// Another commit overwrites the older slot; superseded extents drop out.
		key, err := KeyFromLatLng(testLat+0.2, testLng)
		if err != nil {
			t.Fatal(err)
		}
		if err := index.Insert(key, 1_000_000); err != nil {
			t.Fatal(err)
		}
		if err := index.Compact(); err != nil {
			t.Fatal(err)
		}
		assertProtected(t, index)
		if got, want := extentSet(index.live), recoverableSet(index); len(got) != len(want) {
			t.Fatalf("protected %d != recoverable %d after second commit", len(got), len(want))
		}

		expected := recoverableSet(index)
		if err := index.Close(); err != nil {
			t.Fatal(err)
		}
		reopened, err := Open(dir, cfg)
		if err != nil {
			t.Fatalf("reopen: %v", err)
		}
		defer reopened.Close()
		got := extentSet(reopened.live)
		if len(got) != len(expected) {
			t.Fatalf("recovered protected set %d != expected %d", len(got), len(expected))
		}
		for e := range expected {
			if !got[e] {
				t.Fatalf("recovered allocator missing protected extent ref=%d size=%d", e.ref, e.size)
			}
		}
	}

	t.Run("no-growth", func(t *testing.T) {
		cfg := tinyShardConfig()
		cfg.MaxFileBytes = 1 << 20
		run(t, cfg, 2000, 100)
	})
	t.Run("growth", func(t *testing.T) {
		run(t, growthConfig(), 30_000, 400_000)
	})
}
