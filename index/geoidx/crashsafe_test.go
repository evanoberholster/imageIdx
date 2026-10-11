package geoidx

import (
	"errors"
	"sync"
	"testing"

	common "github.com/evanoberholster/imageIdx/common"
)

// growthConfig returns a config whose fixed arena is too small for the data used
// in these tests, forcing safe growth.
func growthConfig() Config {
	return Config{
		TargetDelta:       8,
		CompactionTrigger: 16,
		MaxDelta:          4096,
		TargetShardBytes:  256 << 10,
		MaxShardBytes:     512 << 10,
		MinShardBytes:     64 << 10,
		MaxFileBytes:      1 << 20, // rounded up to the 4 MiB growth alignment
	}
}

func withNewIDs(entries []Entry, base uint64) []Entry {
	out := make([]Entry, len(entries))
	for i, e := range entries {
		out[i] = Entry{Key: e.Key, ImageID: common.ImageID(base + uint64(i))}
	}
	return out
}

// TestGrowthWithConcurrentReaders forces the arena to grow while lock-free
// readers hold references into the pre-growth mapping. Run with -race; a reader
// touching an unmapped or overwritten extent would fault or fail the race.
func TestGrowthWithConcurrentReaders(t *testing.T) {
	dir := t.TempDir()
	cfg := growthConfig()
	index, err := New(dir, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer index.Close()

	seed := shardedMemoryEntries(50_000)
	insertChunked(t, index, seed)
	if err := index.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	before := index.StorageStats().ArenaBytes

	stop := make(chan struct{})
	var wg sync.WaitGroup
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			all := []Range{{Start: 0, End: S2Key(^uint64(0))}}
			for {
				select {
				case <-stop:
					return
				default:
				}
				if got := index.SearchRanges(nil, all); len(got) < 50_000 {
					t.Errorf("reader saw %d ids, want >= 50000", len(got))
					return
				}
			}
		}()
	}

	more := withNewIDs(shardedMemoryEntries(400_000), 1_000_000)
	insertChunked(t, index, more)
	close(stop)
	wg.Wait()

	if err := index.Compact(); err != nil {
		t.Fatal(err)
	}
	after := index.StorageStats()
	if after.ArenaBytes <= before {
		t.Fatalf("arena did not grow: %d -> %d", before, after.ArenaBytes)
	}
	if after.Mappings != 1 {
		t.Fatalf("growth mappings not released: %d", after.Mappings)
	}
	if got := index.Len(); got < uint64(len(seed)+len(more))-1000 {
		t.Fatalf("Len = %d after growth", got)
	}
}

// TestGrowFaultInjection injects failures at file extension and mapping creation
// and requires the previous generation to stay readable and a retry to succeed.
func TestGrowFaultInjection(t *testing.T) {
	for _, stage := range []persistStage{persistGrow, persistMmap} {
		t.Run(map[persistStage]string{persistGrow: "grow", persistMmap: "mmap"}[stage], func(t *testing.T) {
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
			committed := index.Len()

			more := withNewIDs(shardedMemoryEntries(400_000), 1_000_000)
			if err := index.InsertBatch(more); err != nil {
				t.Fatal(err)
			}
			persistFail = func(s persistStage) error {
				if s == stage {
					return errors.New("injected " + map[persistStage]string{persistGrow: "grow", persistMmap: "mmap"}[stage])
				}
				return nil
			}
			if err := index.Compact(); err == nil {
				t.Fatal("compaction succeeded despite injected growth failure")
			}
			persistFail = nil
			if got := index.Len(); got < committed {
				t.Fatalf("committed data lost: Len = %d, want >= %d", got, committed)
			}
			if err := index.Compact(); err != nil {
				t.Fatalf("retry: %v", err)
			}
			if got, want := index.Len(), committed+uint64(len(more)); got != want {
				t.Fatalf("Len after retry = %d, want %d", got, want)
			}
		})
	}
}

// TestAllocatorReconstructionAfterReopen verifies the free-space accounting is
// reconstructed from committed metadata and matches the pre-close state.
func TestAllocatorReconstructionAfterReopen(t *testing.T) {
	dir := t.TempDir()
	cfg := tinyShardConfig()
	cfg.MaxFileBytes = 1 << 20
	index, err := New(dir, cfg)
	if err != nil {
		t.Fatal(err)
	}
	insertChunked(t, index, shardedMemoryEntries(20_000))
	if err := index.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	before := index.StorageStats()
	if err := index.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(dir, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	after := reopened.StorageStats()
	if after.ArenaBytes != before.ArenaBytes || after.FreeBytes != before.FreeBytes || after.LiveBytes != before.LiveBytes {
		t.Fatalf("allocator not reconstructed: before %+v after %+v", before, after)
	}
	if reopened.Len() != 20_000 {
		t.Fatalf("Len = %d, want 20000", reopened.Len())
	}
}

// TestRepeatedReplacementSteadyState rewrites the same shards many times and
// requires the fixed arena to reach a steady state rather than growing without
// bound.
func TestRepeatedReplacementSteadyState(t *testing.T) {
	dir := t.TempDir()
	cfg := growthConfig()
	index, err := New(dir, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer index.Close()
	entries := shardedMemoryEntries(80_000)
	insertChunked(t, index, entries)
	if err := index.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	var midArena uint64
	for i := 0; i < 600; i++ {
		victim := entries[i%len(entries)]
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
		if err := index.Compact(); err != nil {
			t.Fatalf("compact %d: %v", i, err)
		}
		if i == 300 {
			midArena = index.StorageStats().ArenaBytes
		}
	}
	final := index.StorageStats()
	if final.ArenaBytes > midArena {
		t.Fatalf("arena kept growing: mid %d final %d", midArena, final.ArenaBytes)
	}
	if final.LiveBytes == 0 || final.FreeBytes == 0 {
		t.Fatalf("unexpected stats: %+v", final)
	}
}
