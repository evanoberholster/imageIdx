package geoidx

import (
	"errors"
	"os"
	"slices"
	"testing"

	common "github.com/evanoberholster/imageIdx/common"
	"github.com/evanoberholster/imageIdx/internal/storage"
	"github.com/evanoberholster/imageIdx/roar"
	"github.com/golang/geo/s2"
)

// refRangeIDs is the reference result for a range query: ascending, unique
// ImageIDs whose key is in ranges.
func refRangeIDs(entries []Entry, ranges []Range) []common.ImageID {
	out := make([]common.ImageID, 0)
	for _, e := range entries {
		if keyInRanges(ranges, e.Key) {
			out = append(out, e.ImageID)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// assertAscendingUniqueNoDuplicates checks an initial-search result and the
// live shard records for ordering and uniqueness.
func assertAscendingUniqueNoDuplicates(t *testing.T, index *Index, got []common.ImageID) {
	t.Helper()
	if !slices.IsSorted(got) {
		t.Fatal("results not ascending")
	}
	for i := 1; i < len(got); i++ {
		if got[i] == got[i-1] {
			t.Fatalf("duplicate result %d", got[i])
		}
	}
	seen := map[common.ImageID]struct{}{}
	for _, s := range index.view.Load().shards {
		for _, id := range s.ids {
			if _, dup := seen[id]; dup {
				t.Fatalf("duplicate live ImageID %d", id)
			}
			seen[id] = struct{}{}
		}
	}
	if len(seen) != len(got) {
		t.Fatalf("result cardinality %d != live records %d", len(got), len(seen))
	}
}

// Scenario A: repeated synchronization failures across retries.
func TestRepeatedUncertainCommits(t *testing.T) {
	dir := t.TempDir()
	cfg := tinyShardConfig()
	cfg.MaxFileBytes = 1 << 20
	index, err := New(dir, cfg)
	if err != nil {
		t.Fatal(err)
	}
	insertChunked(t, index, shardedMemoryEntries(200))
	if err := index.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	committed := index.Generation()
	insertChunked(t, index, withNewIDs(shardedMemoryEntries(100), 1_000_000))

	failures := 0
	original := syncArenaFunc
	syncArenaFunc = func(mf *storage.MappedFile) error {
		failures++
		if failures <= 2 {
			return errors.New("sync failure")
		}
		return original(mf)
	}
	if err := index.Compact(); err == nil {
		t.Fatal("attempt 1 should be an uncertain commit")
	}
	if index.Generation() != committed {
		t.Fatal("uncertain commit advanced the durable generation")
	}
	if err := index.Compact(); err == nil {
		t.Fatal("attempt 2 should be an uncertain commit")
	}
	if index.Generation() != committed {
		t.Fatal("uncertain commit advanced the durable generation")
	}
	if err := index.Compact(); err != nil {
		t.Fatalf("attempt 3: %v", err)
	}
	syncArenaFunc = original
	if index.Generation() == committed {
		t.Fatal("successful commit did not advance the generation")
	}

	all := []Range{{Start: 0, End: S2Key(^uint64(0))}}
	got := index.SearchRanges(nil, all)
	if len(got) != 300 {
		t.Fatalf("Len after retries = %d, want 300", len(got))
	}
	assertAscendingUniqueNoDuplicates(t, index, got)

	// Abandoned (uncertain) extents become reusable: a further small update must
	// not grow the arena.
	arenaBefore := index.StorageStats().ArenaBytes
	moveKey, err := KeyFromLatLng(testLat+0.2, testLng)
	if err != nil {
		t.Fatal(err)
	}
	if err := index.Insert(moveKey, 1_000_000); err != nil {
		t.Fatal(err)
	}
	if err := index.Compact(); err != nil {
		t.Fatal(err)
	}
	if grown := index.StorageStats().ArenaBytes; grown > arenaBefore {
		t.Fatalf("arena grew after reuse: %d -> %d", arenaBefore, grown)
	}

	if err := index.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(dir, cfg)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	if !validateDirectory(reopened.view.Load().shards) {
		t.Fatal("reopened directory invalid")
	}
	if got := reopened.Len(); got != 300 {
		t.Fatalf("reopened Len = %d, want 300", got)
	}
}

// Scenario B: partial and full-then-error superblock writes across successive
// retries, then success.
func TestSuccessivePartialWrites(t *testing.T) {
	dir := t.TempDir()
	cfg := tinyShardConfig()
	cfg.MaxFileBytes = 1 << 20
	index, err := New(dir, cfg)
	if err != nil {
		t.Fatal(err)
	}
	insertChunked(t, index, shardedMemoryEntries(200))
	if err := index.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	committed := index.Generation()
	insertChunked(t, index, withNewIDs(shardedMemoryEntries(100), 1_000_000))

	attempt := 0
	original := writeSuperblockSlotFunc
	writeSuperblockSlotFunc = func(arena []byte, offset uint64, slot []byte) error {
		attempt++
		switch attempt {
		case 1: // modifies only part of the slot
			copy(arena[offset:offset+32], slot[:32])
			return errors.New("partial")
		case 2: // writes the whole slot, then reports failure
			copy(arena[offset:offset+storage.SuperblockSlotBytes], slot)
			return errors.New("post-write failure")
		case 3: // fails without modifying bytes
			return errors.New("no write")
		default:
			return original(arena, offset, slot)
		}
	}
	if err := index.Compact(); err == nil {
		t.Fatal("partial write should be uncertain")
	}
	if err := index.Compact(); err == nil {
		t.Fatal("full-then-error write should be uncertain")
	}
	if err := index.Compact(); err == nil {
		t.Fatal("no-write failure should abort")
	}
	if index.Generation() != committed {
		t.Fatal("failed writes advanced the durable generation")
	}
	if err := index.Compact(); err != nil {
		t.Fatalf("final attempt: %v", err)
	}
	writeSuperblockSlotFunc = original

	all := []Range{{Start: 0, End: S2Key(^uint64(0))}}
	if got := index.SearchRanges(nil, all); len(got) != 300 {
		t.Fatalf("Len = %d, want 300", len(got))
	}
	if err := index.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(dir, cfg)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	if got := reopened.Len(); got != 300 {
		t.Fatalf("reopened Len = %d, want 300", got)
	}
}

// Scenario D: an uncertain commit combined with forced file growth.
func TestUncertainThenGrowth(t *testing.T) {
	dir := t.TempDir()
	cfg := growthConfig()
	index, err := New(dir, cfg)
	if err != nil {
		t.Fatal(err)
	}
	insertChunked(t, index, shardedMemoryEntries(30_000))
	if err := index.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	committed := index.Generation()
	// A batch larger than the arena forces growth during the commit attempt.
	more := withNewIDs(shardedMemoryEntries(400_000), 1_000_000)
	if err := index.InsertBatch(more); err != nil {
		t.Fatal(err)
	}

	original := syncArenaFunc
	syncArenaFunc = func(mf *storage.MappedFile) error { return errors.New("sync failure") }
	if err := index.Compact(); err == nil {
		t.Fatal("expected an uncertain commit")
	}
	if index.Generation() != committed {
		t.Fatal("uncertain commit advanced the generation")
	}
	syncArenaFunc = original

	if err := index.Compact(); err != nil {
		t.Fatalf("retry after uncertain growth: %v", err)
	}
	all := []Range{{Start: 0, End: S2Key(^uint64(0))}}
	got := index.SearchRanges(nil, all)
	if len(got) < 30_000 {
		t.Fatalf("committed generation lost: Len = %d", len(got))
	}
	assertAscendingUniqueNoDuplicates(t, index, got)

	if err := index.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(dir, cfg)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	if !validateDirectory(reopened.view.Load().shards) {
		t.Fatal("reopened directory invalid")
	}
}

// TestSearchDifferentialAcrossShards compares every query mode against a
// reference, then repeats after compaction and reopen to confirm results are
// stable across shard replacement and recovery.
func TestSearchDifferentialAcrossShards(t *testing.T) {
	dir := t.TempDir()
	cfg := tinyShardConfig()
	cfg.MaxFileBytes = 1 << 20
	entries := shardedMemoryEntries(5000)
	index := persistentShardedIndex(t, dir, cfg, entries)

	exactKey := entries[123].Key
	exact := []Range{{Start: exactKey, End: exactKey}}
	cover := testCover(3000)
	rect := s2.RectFromCenterSize(s2.LatLngFromDegrees(testLat, testLng), s2.LatLngFromDegrees(0.02, 0.02))
	rectRanges := CoverRect(nil, rect)
	radius := 2000.0
	center := s2.LatLngFromDegrees(testLat, testLng)

	check := func(t *testing.T, stage string) {
		t.Helper()
		// Exact cell and ranges.
		if got, want := index.SearchRanges(nil, exact), refRangeIDs(entries, exact); !slices.Equal(got, want) {
			t.Fatalf("%s exact: got %d, want %d", stage, len(got), len(want))
		}
		if got, want := index.SearchRanges(nil, cover), refRangeIDs(entries, cover); !slices.Equal(got, want) {
			t.Fatalf("%s cover: got %d, want %d", stage, len(got), len(want))
		}
		if got, want := index.SearchRanges(nil, rectRanges), refRangeIDs(entries, rectRanges); !slices.Equal(got, want) {
			t.Fatalf("%s rect: got %d, want %d", stage, len(got), len(want))
		}
		assertAscendingUniqueNoDuplicates(t, index, index.SearchRanges(nil, allRanges()))

		// Radius (exact cell-center distance).
		gotRadius, err := index.SearchRadius(nil, testLat, testLng, radius)
		if err != nil {
			t.Fatal(err)
		}
		wantRadius := make([]common.ImageID, 0)
		for _, e := range entries {
			if keyDistanceMeters(e.Key, center) <= radius {
				wantRadius = append(wantRadius, e.ImageID)
			}
		}
		slices.Sort(wantRadius)
		if !slices.Equal(gotRadius, wantRadius) {
			t.Fatalf("%s radius: got %d, want %d", stage, len(gotRadius), len(wantRadius))
		}

		// Nearest.
		for _, k := range []int{1, 10, 100} {
			got, err := index.Nearest(nil, testLat, testLng, k)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(got, nearestOracle(entries, center, k)) {
				t.Fatalf("%s nearest k=%d mismatch", stage, k)
			}
		}

		// FilterBy: candidates are every 7th id.
		candidates := make([]uint64, 0, len(entries)/7+1)
		for id := uint64(1); id <= uint64(len(entries)); id += 7 {
			candidates = append(candidates, id)
		}
		bit := roar.MustFromSorted(candidates)
		m := roar.NewMutableBitmap()
		index.FilterRanges(m, bit, cover)
		gotFilter := m.AppendTo(nil)
		wantFilter := intersectSorted(refRangeIDs(entries, cover), candidates)
		if !slices.Equal(gotFilter, wantFilter) {
			t.Fatalf("%s filter: got %d, want %d", stage, len(gotFilter), len(wantFilter))
		}
	}

	check(t, "initial")
	if err := index.Compact(); err != nil {
		t.Fatal(err)
	}
	check(t, "after-compact")
	if err := index.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(dir, cfg)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	index = reopened
	check(t, "after-reopen")
}

func allRanges() []Range { return []Range{{Start: 0, End: S2Key(^uint64(0))}} }

// TestMutationEdgeCases covers duplicate mutations within a batch, deletes of
// nonexistent ids, and repeated changes to one ImageID.
func TestMutationEdgeCases(t *testing.T) {
	index, err := NewMemory(tinyShardConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer index.Close()
	keyA, _ := KeyFromLatLng(testLat, testLng)
	keyB, _ := KeyFromLatLng(testLat+0.2, testLng)

	// Duplicate ids within one batch: the last operation wins.
	if err := index.Apply([]Entry{{Key: keyA, ImageID: 1}, {Key: keyB, ImageID: 1}}, nil); err != nil {
		t.Fatal(err)
	}
	if index.Contains(keyA, 1) || !index.Contains(keyB, 1) {
		t.Fatal("duplicate insert in a batch did not resolve to the last key")
	}

	// Delete a nonexistent id (no-op) and a duplicate delete.
	if err := index.Delete(keyA, 999999); err != nil {
		t.Fatal(err)
	}
	if err := index.Delete(keyA, 1); err != nil {
		t.Fatal(err)
	}
	if err := index.Delete(keyA, 1); err != nil {
		t.Fatal(err)
	}
	if index.Contains(keyB, 1) {
		t.Fatal("stale/duplicate delete did not remove the record")
	}

	// Insert a batch with internal duplicates: id 2 ends at keyB.
	if err := index.InsertBatch([]Entry{{Key: keyA, ImageID: 2}, {Key: keyA, ImageID: 2}, {Key: keyB, ImageID: 2}}); err != nil {
		t.Fatal(err)
	}
	if err := index.Compact(); err != nil {
		t.Fatal(err)
	}
	if index.Contains(keyA, 2) || !index.Contains(keyB, 2) {
		t.Fatal("internal duplicate batch did not resolve to the last key")
	}
	if got := index.Len(); got != 1 {
		t.Fatalf("Len = %d, want 1", got)
	}
}

// TestOnlyOnePersistentFile confirms the on-disk directory holds only the data
// file and the shared lock file, before and after mutation, compaction, close,
// and reopen.
func TestOnlyOnePersistentFile(t *testing.T) {
	dir := t.TempDir()
	cfg := tinyShardConfig()
	cfg.MaxFileBytes = 1 << 20
	index, err := New(dir, cfg)
	if err != nil {
		t.Fatal(err)
	}
	checkFiles := func(stage string) {
		t.Helper()
		dirEntries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		names := make([]string, 0, len(dirEntries))
		for _, e := range dirEntries {
			names = append(names, e.Name())
		}
		slices.Sort(names)
		want := []string{geoidxFile, lockName}
		if !slices.Equal(names, want) {
			t.Fatalf("%s: directory contains %v, want %v", stage, names, want)
		}
	}
	checkFiles("new")
	insertChunked(t, index, shardedMemoryEntries(3000))
	checkFiles("after-insert")
	if err := index.Compact(); err != nil {
		t.Fatal(err)
	}
	checkFiles("after-compact")
	if err := index.Close(); err != nil {
		t.Fatal(err)
	}
	checkFiles("after-close")
	reopened, err := Open(dir, cfg)
	if err != nil {
		t.Fatal(err)
	}
	checkFiles("after-reopen")
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
}
