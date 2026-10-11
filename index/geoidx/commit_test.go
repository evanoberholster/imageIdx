package geoidx

import (
	"errors"
	"sync"
	"testing"

	"github.com/evanoberholster/imageIdx/internal/storage"
)

// TestCommitFailureInjection injects failures at each superblock commit step and
// verifies the previous generation stays authoritative, a retry succeeds, and a
// reopen reads a complete valid generation.
func TestCommitFailureInjection(t *testing.T) {
	config := Config{TargetDelta: 16, CompactionTrigger: 1 << 20, MaxDelta: 1 << 21}
	newIndex := func(t *testing.T) (*Index, string) {
		t.Helper()
		dir := t.TempDir()
		index, err := New(dir, config)
		if err != nil {
			t.Fatal(err)
		}
		insertKeys(t, index, testKeys(100), 1)
		if err := index.Checkpoint(); err != nil {
			t.Fatal(err)
		}
		insertKeys(t, index, testKeys(50), 1000) // pending, uncommitted
		return index, dir
	}
	reopen := func(t *testing.T, index *Index, dir string) {
		t.Helper()
		if err := index.Close(); err != nil {
			t.Fatal(err)
		}
		reopened, err := Open(dir, config)
		if err != nil {
			t.Fatalf("reopen: %v", err)
		}
		defer reopened.Close()
		if got := reopened.Len(); got != 150 {
			t.Fatalf("reopened Len = %d, want 150", got)
		}
		if !validateDirectory(reopened.view.Load().shards) {
			t.Fatal("reopened directory invalid")
		}
	}

	t.Run("before-write", func(t *testing.T) {
		index, dir := newIndex(t)
		defer index.Close()
		persistFail = func(s persistStage) error {
			if s == persistCommit {
				return errors.New("before write")
			}
			return nil
		}
		if err := index.Compact(); err == nil {
			t.Fatal("Compact succeeded despite injected write failure")
		}
		persistFail = nil
		if got := index.Len(); got != 150 {
			t.Fatalf("Len = %d, want 150", got)
		}
		if err := index.Compact(); err != nil {
			t.Fatalf("retry: %v", err)
		}
		reopen(t, index, dir)
	})

	t.Run("during-write", func(t *testing.T) {
		index, dir := newIndex(t)
		defer index.Close()
		original := writeSuperblockSlotFunc
		writeSuperblockSlotFunc = func(_ []byte, _ uint64, _ []byte) error { return errors.New("write") }
		if err := index.Compact(); err == nil {
			t.Fatal("Compact succeeded despite injected slot write failure")
		}
		writeSuperblockSlotFunc = original
		if err := index.Compact(); err != nil {
			t.Fatalf("retry: %v", err)
		}
		reopen(t, index, dir)
	})

	t.Run("before-sync", func(t *testing.T) {
		index, dir := newIndex(t)
		defer index.Close()
		persistFail = func(s persistStage) error {
			if s == persistSuperSync {
				return errors.New("before sync")
			}
			return nil
		}
		if err := index.Compact(); !errors.Is(err, errCommitUncertain) {
			t.Fatalf("Compact = %v, want errCommitUncertain", err)
		}
		persistFail = nil
		// The previous generation remains authoritative in memory.
		if got := index.Len(); got != 150 {
			t.Fatalf("Len = %d, want 150", got)
		}
		if err := index.Compact(); err != nil {
			t.Fatalf("retry: %v", err)
		}
		reopen(t, index, dir)
	})

	t.Run("during-sync", func(t *testing.T) {
		index, dir := newIndex(t)
		defer index.Close()
		original := syncArenaFunc
		syncArenaFunc = func(_ *storage.MappedFile) error { return errors.New("sync") }
		if err := index.Compact(); !errors.Is(err, errCommitUncertain) {
			t.Fatalf("Compact = %v, want errCommitUncertain", err)
		}
		syncArenaFunc = original
		if err := index.Compact(); err != nil {
			t.Fatalf("retry: %v", err)
		}
		reopen(t, index, dir)
	})

	t.Run("after-sync-before-publish", func(t *testing.T) {
		index, dir := newIndex(t)
		defer index.Close()
		// Publication failure must not roll back a durable commit.
		persistFail = func(s persistStage) error {
			if s == persistPublish {
				return errors.New("publish")
			}
			return nil
		}
		if err := index.Compact(); err != nil {
			t.Fatalf("Compact = %v; a durable commit must stand", err)
		}
		persistFail = nil
		if err := index.Compact(); err != nil {
			t.Fatalf("retry: %v", err)
		}
		reopen(t, index, dir)
	})
}

// TestExtentIdentity rejects inconsistent extent references.
func TestExtentIdentity(t *testing.T) {
	if _, err := dedupExtents([]extentRef{{ref: 5, size: 512}, {ref: 5, size: 1024}}); err == nil {
		t.Fatal("same offset with different sizes accepted")
	}
	out, err := dedupExtents([]extentRef{{ref: 5, size: 512}, {ref: 5, size: 512}, {ref: 9, size: 512}})
	if err != nil || len(out) != 2 {
		t.Fatalf("identical extents should dedup: out=%v err=%v", out, err)
	}
	if _, err := allocatorFromLive(1<<20, []extentRef{{ref: 5, size: 4096}, {ref: 7, size: 512}}); err == nil {
		t.Fatal("partially overlapping extents accepted")
	}
	if _, err := allocatorFromLive(1<<20, []extentRef{{ref: 5, size: 0}}); err == nil {
		t.Fatal("zero-length extent accepted")
	}
	if _, err := allocatorFromLive(1<<20, []extentRef{{ref: 1, size: 512}}); err == nil {
		t.Fatal("extent overlapping the reserved superblock accepted")
	}
}

// TestGrowthWriteAmplification logs the cost distinction between selective shard
// compaction and a forced growth migration, so the two are not conflated.
func TestGrowthWriteAmplification(t *testing.T) {
	if testing.Short() {
		t.Skip("growth measurement")
	}
	dir := t.TempDir()
	cfg := growthConfig()
	index, err := New(dir, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer index.Close()
	insertChunked(t, index, shardedMemoryEntries(50_000))
	if err := index.Checkpoint(); err != nil {
		t.Fatal(err)
	}

	// Selective: one small update rewrites only the affected shard(s).
	before := index.StorageStats()
	key, err := KeyFromLatLng(testLat+0.1, testLng)
	if err != nil {
		t.Fatal(err)
	}
	if err := index.Insert(key, 900000); err != nil {
		t.Fatal(err)
	}
	if err := index.Compact(); err != nil {
		t.Fatal(err)
	}
	selective := index.StorageStats()
	t.Logf("selective: arena=%d live=%d free=%d largestFree=%d runs=%d",
		selective.ArenaBytes, selective.LiveBytes, selective.FreeBytes, selective.LargestFree, selective.FreeRuns)
	_ = before

	// Growth migration: a batch larger than the arena forces a full migration.
	index.InsertBatch(withNewIDs(shardedMemoryEntries(400_000), 1_000_000))
	if err := index.Compact(); err != nil {
		t.Fatal(err)
	}
	growth := index.StorageStats()
	t.Logf("growth-migration: arena=%d live=%d free=%d largestFree=%d runs=%d mappings=%d",
		growth.ArenaBytes, growth.LiveBytes, growth.FreeBytes, growth.LargestFree, growth.FreeRuns, growth.Mappings)
	t.Logf("growth-migration wrote a full copy: live delta=%d bytes, arena delta=%d bytes (logical, not physical)",
		growth.LiveBytes-selective.LiveBytes, growth.ArenaBytes-selective.ArenaBytes)
	_ = before
}

// TestConcurrentSearchGrowthShutdown stresses searches, repeated shard
// replacement, forced growth, and shutdown together. Run with -race.
func TestConcurrentSearchGrowthShutdown(t *testing.T) {
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

	stop := make(chan struct{})
	var wg sync.WaitGroup
	all := []Range{{Start: 0, End: S2Key(^uint64(0))}}
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				index.SearchRanges(nil, all)
				_, _ = index.Nearest(nil, testLat, testLng, 10)
			}
		}()
	}

	seed := shardedMemoryEntries(30_000)
	for i := 0; i < 200; i++ {
		victim := seed[i%len(seed)]
		key, err := KeyFromLatLng(testLat+0.2, testLng+float64(i)*1e-6)
		if err != nil {
			t.Fatal(err)
		}
		if err := index.Delete(victim.Key, victim.ImageID); err != nil {
			t.Fatal(err)
		}
		if err := index.Insert(key, victim.ImageID); err != nil {
			t.Fatal(err)
		}
		if i%10 == 0 {
			if err := index.Compact(); err != nil {
				t.Fatal(err)
			}
		}
	}
	// Force growth with a large batch while readers run.
	if err := index.InsertBatch(withNewIDs(shardedMemoryEntries(300_000), 1_000_000)); err != nil {
		t.Fatal(err)
	}
	if err := index.Compact(); err != nil {
		t.Fatal(err)
	}
	close(stop)
	wg.Wait()
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
	if reopened.Len() < 30_000 {
		t.Fatalf("reopened Len = %d, want >= 30000", reopened.Len())
	}
}
