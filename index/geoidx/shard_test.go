package geoidx

import (
	"errors"
	"math/rand"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"

	schema "github.com/evanoberholster/imageIdx/schema"
	"github.com/evanoberholster/imageIdx/roar"
)

// tinyShardConfig forces many small shards so split, merge, and cross-shard
// behavior are exercised by modest datasets: 128-record target, 256-record max,
// 64-record min (16 B/record).
func tinyShardConfig() Config {
	return Config{
		TargetDelta:       8,
		CompactionTrigger: 16,
		MaxDelta:          512,
		TargetShardBytes:  2 << 10,
		MaxShardBytes:     4 << 10,
		MinShardBytes:     1 << 10,
	}
}

// shardedMemoryEntries builds n deterministic uniform entries.
func shardedMemoryEntries(n int) []Entry {
	rng := rand.New(rand.NewSource(11))
	entries := make([]Entry, n)
	for i := range entries {
		lat := testLat + (rng.Float64()*2-1)*testSpan
		lng := testLng + (rng.Float64()*2-1)*testSpan
		key, err := KeyFromLatLng(lat, lng)
		if err != nil {
			panic(err)
		}
		entries[i] = Entry{Key: key, ImageID: schema.ImageID(i + 1)}
	}
	return entries
}

// insertChunked inserts entries in 64-entry batches to exercise rotation.
func insertChunked(t testing.TB, index *Index, entries []Entry) {
	t.Helper()
	for start := 0; start < len(entries); start += 64 {
		end := start + 64
		if end > len(entries) {
			end = len(entries)
		}
		if err := index.InsertBatch(entries[start:end]); err != nil {
			t.Fatalf("InsertBatch: %v", err)
		}
	}
}

func shardedMemoryIndex(t testing.TB, n int, cfg Config) (*Index, []Entry) {
	t.Helper()
	index, err := NewMemory(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = index.Close() })
	entries := shardedMemoryEntries(n)
	insertChunked(t, index, entries)
	if err := index.Compact(); err != nil {
		t.Fatalf("Compact: %v", err)
	}
	return index, entries
}

// referenceIDs returns the ascending ImageIDs whose entry key is in ranges.
func referenceIDs(entries []Entry, ranges []Range) []schema.ImageID {
	out := make([]schema.ImageID, 0)
	for _, e := range entries {
		if keyInRanges(ranges, e.Key) {
			out = append(out, e.ImageID)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

func TestShardDirectoryHelpers(t *testing.T) {
	keys := []S2Key{}
	for _, lat := range []float64{0, 0.01, 0.02, 0.03} {
		k, err := KeyFromLatLng(testLat+lat, testLng)
		if err != nil {
			t.Fatal(err)
		}
		keys = append(keys, k)
	}
	slices.Sort(keys)
	k0, k1, k2, k3 := keys[0], keys[1], keys[2], keys[3]
	s0 := &shard{keys: []S2Key{k0, k0, k1}, ids: []schema.ImageID{1, 2, 3}}
	s1 := &shard{keys: []S2Key{k1, k2}, ids: []schema.ImageID{4, 5}}
	s2 := &shard{keys: []S2Key{k3}, ids: []schema.ImageID{6}}
	shards := []*shard{s0, s1, s2}

	for _, tc := range []struct {
		key  S2Key
		id   schema.ImageID
		want int
	}{
		{k0 - 1, 1, 0}, {k0, 1, 0}, {k1, 3, 0}, {k1, 4, 1}, {k1 + 1, 1, 1}, {k3, 6, 2}, {k3 + 1, 1, 2},
	} {
		if got := shardFor(shards, tc.key, tc.id); got != tc.want {
			t.Errorf("shardFor(%d,%d) = %d, want %d", tc.key, tc.id, got, tc.want)
		}
	}
	for _, tc := range []struct {
		start S2Key
		want  int
	}{
		{k0 - 1, 0}, {k1, 0}, {k1 + 1, 1}, {k2, 1}, {k2 + 1, 2}, {k3, 2}, {k3 + 1, 3},
	} {
		if got := firstShardAtOrAfter(shards, tc.start); got != tc.want {
			t.Errorf("firstShardAtOrAfter(%d) = %d, want %d", tc.start, got, tc.want)
		}
	}
	if !validateDirectory(shards) {
		t.Fatal("valid synthetic directory rejected")
	}
}

func TestShardDirectoryOrdering(t *testing.T) {
	index, entries := shardedMemoryIndex(t, 4000, tinyShardConfig())
	v := index.view.Load()
	if len(v.shards) < 3 {
		t.Fatalf("expected several shards, got %d", len(v.shards))
	}
	if !validateDirectory(v.shards) {
		t.Fatal("committed directory is not strictly ordered")
	}
	if got := totalEntries(v.shards); got != len(entries) {
		t.Fatalf("total entries = %d, want %d", got, len(entries))
	}
	if got := index.Len(); got != uint64(len(entries)) {
		t.Fatalf("Len = %d, want %d", got, len(entries))
	}
	for _, e := range entries {
		if !index.Contains(e.Key, e.ImageID) {
			t.Fatalf("image %d missing", e.ImageID)
		}
	}
}

// TestRepeatedKeySpansShards forces one S2Key whose run is split across several
// shards and requires every record to remain findable.
func TestRepeatedKeySpansShards(t *testing.T) {
	key, err := KeyFromLatLng(testLat, testLng)
	if err != nil {
		t.Fatal(err)
	}
	const n = 1000
	entries := make([]Entry, n)
	for i := range entries {
		entries[i] = Entry{Key: key, ImageID: schema.ImageID(i + 1)}
	}
	index, err := NewMemory(tinyShardConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer index.Close()
	insertChunked(t, index, entries)
	if err := index.Compact(); err != nil {
		t.Fatal(err)
	}
	v := index.view.Load()
	if len(v.shards) < 3 {
		t.Fatalf("repeated key did not span shards: %d shards", len(v.shards))
	}
	ranges := []Range{{Start: key, End: key}}
	got := index.SearchRanges(nil, ranges)
	if len(got) != n {
		t.Fatalf("SearchRanges exact key = %d ids, want %d", len(got), n)
	}
	// Boundary ImageIDs across shards must all resolve.
	for _, id := range []schema.ImageID{1, 128, 129, 256, 257, 999, 1000} {
		if !index.Contains(key, id) {
			t.Fatalf("image %d missing after split", id)
		}
	}
}

func TestSearchRangesCrossShardsDifferential(t *testing.T) {
	index, entries := shardedMemoryIndex(t, 5000, tinyShardConfig())
	v := index.view.Load()
	if len(v.shards) < 4 {
		t.Fatalf("need >=4 shards, got %d", len(v.shards))
	}
	last := func(s *shard) S2Key { return s.keys[s.count()-1] }
	ranges := [][]Range{
		{{Start: v.shards[0].keys[0], End: last(v.shards[1])}},
		{{Start: v.shards[1].keys[0], End: last(v.shards[3])}},
		{{Start: v.shards[0].keys[0], End: last(v.shards[len(v.shards)-1])}},
		{{Start: v.shards[2].keys[0], End: v.shards[2].keys[0]}},
	}
	for i, r := range ranges {
		want := referenceIDs(entries, r)
		got := index.SearchRanges(nil, r)
		if !slices.Equal(got, want) {
			t.Fatalf("range %d: got %d ids, want %d", i, len(got), len(want))
		}
		// CollectRanges and FilterRanges must agree across shards.
		m := roar.NewMutableBitmap()
		index.CollectRanges(m, r)
		if collected := m.AppendTo(nil); !slices.Equal(collected, want) {
			t.Fatalf("range %d: CollectRanges got %d, want %d", i, len(collected), len(want))
		}
		candidates := make([]uint64, 0, len(entries))
		for id := uint64(1); id <= uint64(len(entries)); id++ {
			candidates = append(candidates, id)
		}
		bit := roar.MustFromSorted(candidates)
		m.Reset()
		index.FilterRanges(m, bit, r)
		if filtered := m.AppendTo(nil); !slices.Equal(filtered, want) {
			t.Fatalf("range %d: FilterRanges got %d, want %d", i, len(filtered), len(want))
		}
	}
}

func TestShardSplitAndMerge(t *testing.T) {
	index, entries := shardedMemoryIndex(t, 4000, tinyShardConfig())
	splits := len(index.view.Load().shards)
	if splits < 8 {
		t.Fatalf("expected many shards after load, got %d", splits)
	}

	// Delete most records; compaction must coalesce the survivors.
	deletes := entries[:3500]
	if err := index.DeleteBatch(deletes); err != nil {
		t.Fatal(err)
	}
	if err := index.Compact(); err != nil {
		t.Fatal(err)
	}
	merged := index.view.Load().shards
	if len(merged) >= splits {
		t.Fatalf("merge did not reduce shard count: %d -> %d", splits, len(merged))
	}
	if !validateDirectory(merged) {
		t.Fatal("merged directory not ordered")
	}
	if got, want := index.Len(), uint64(len(entries)-len(deletes)); got != want {
		t.Fatalf("Len after merge = %d, want %d", got, want)
	}
}

func TestEmptyShardRemoved(t *testing.T) {
	index, entries := shardedMemoryIndex(t, 2000, tinyShardConfig())
	if len(index.view.Load().shards) == 0 {
		t.Fatal("expected shards after load")
	}
	if err := index.DeleteBatch(entries); err != nil {
		t.Fatal(err)
	}
	if err := index.Compact(); err != nil {
		t.Fatal(err)
	}
	if got := len(index.view.Load().shards); got != 0 {
		t.Fatalf("directory not emptied: %d shards", got)
	}
	if got := index.Len(); got != 0 {
		t.Fatalf("Len = %d, want 0", got)
	}
}

// TestSelectiveCompactionRewritesAffectedShards verifies that a small update
// rewrites only the shard(s) it touches, not the whole directory.
func TestSelectiveCompactionRewritesAffectedShards(t *testing.T) {
	dir := t.TempDir()
	cfg := tinyShardConfig()
	index, err := New(dir, cfg)
	if err != nil {
		t.Fatal(err)
	}
	entries := shardedMemoryEntries(3000)
	insertChunked(t, index, entries)
	if err := index.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	before := map[uint64]struct{}{}
	for _, s := range index.view.Load().shards {
		before[s.gen] = struct{}{}
	}

	// One insert plus one delete, both in a single shard's range.
	target := index.view.Load().shards[1]
	if err := index.Insert(target.keys[0], 900000); err != nil {
		t.Fatal(err)
	}
	if err := index.Delete(target.keys[0], target.ids[0]); err != nil {
		t.Fatal(err)
	}
	if err := index.Compact(); err != nil {
		t.Fatal(err)
	}
	changed := 0
	for _, s := range index.view.Load().shards {
		if _, ok := before[s.gen]; !ok {
			changed++
		}
	}
	if changed == 0 {
		t.Fatal("compaction rewrote no shards")
	}
	if changed > 2 {
		t.Fatalf("compaction rewrote %d shards, want at most 2", changed)
	}
	if err := index.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestStaleKeyDeleteAcrossShards(t *testing.T) {
	index, entries := shardedMemoryIndex(t, 3000, tinyShardConfig())
	stale, err := KeyFromLatLng(testLat+10, testLng+10)
	if err != nil {
		t.Fatal(err)
	}
	victim := entries[len(entries)/2]
	if err := index.Delete(stale, victim.ImageID); err != nil {
		t.Fatal(err)
	}
	if err := index.Compact(); err != nil {
		t.Fatal(err)
	}
	if index.Contains(victim.Key, victim.ImageID) {
		t.Fatal("stale-key delete did not remove the record")
	}
	if got, want := index.Len(), uint64(len(entries)-1); got != want {
		t.Fatalf("Len = %d, want %d", got, want)
	}
}

func TestMultiShardMove(t *testing.T) {
	index, entries := shardedMemoryIndex(t, 3000, tinyShardConfig())
	moved := entries[10]
	target, err := KeyFromLatLng(testLat+0.3, testLng)
	if err != nil {
		t.Fatal(err)
	}
	if err := index.Delete(moved.Key, moved.ImageID); err != nil {
		t.Fatal(err)
	}
	if err := index.Insert(target, moved.ImageID); err != nil {
		t.Fatal(err)
	}
	if err := index.Compact(); err != nil {
		t.Fatal(err)
	}
	if index.Contains(moved.Key, moved.ImageID) {
		t.Fatal("moved image still present at its old key")
	}
	if !index.Contains(target, moved.ImageID) {
		t.Fatal("moved image missing at its new key")
	}
	// No duplicate live ImageID across shards.
	seen := map[schema.ImageID]int{}
	for _, s := range index.view.Load().shards {
		for _, id := range s.ids {
			seen[id]++
		}
	}
	for id, n := range seen {
		if n > 1 {
			t.Fatalf("image %d appears in %d shards", id, n)
		}
	}
}

func TestPersistentShardedRoundTrip(t *testing.T) {
	dir := t.TempDir()
	cfg := tinyShardConfig()
	index, err := New(dir, cfg)
	if err != nil {
		t.Fatal(err)
	}
	entries := shardedMemoryEntries(3000)
	insertChunked(t, index, entries)
	if err := index.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	want := index.SearchRanges(nil, []Range{{Start: 0, End: S2Key(^uint64(0))}})
	shardsBefore := len(index.view.Load().shards)
	if shardsBefore < 3 {
		t.Fatalf("expected sharded persistent index, got %d shards", shardsBefore)
	}
	if err := index.Close(); err != nil {
		t.Fatal(err)
	}

	files, _ := filepath.Glob(filepath.Join(dir, "*"))
	for _, f := range files {
		base := filepath.Base(f)
		if base != geoidxFile && base != "geoidx.lock" {
			t.Fatalf("unexpected sidecar file %s", base)
		}
	}

	reopened, err := Open(dir, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if !validateDirectory(reopened.view.Load().shards) {
		t.Fatal("reopened directory not ordered")
	}
	got := reopened.SearchRanges(nil, []Range{{Start: 0, End: S2Key(^uint64(0))}})
	if !slices.Equal(got, want) {
		t.Fatalf("reopened query mismatch: got %d ids, want %d", len(got), len(want))
	}
	if got := reopened.Len(); got != uint64(len(entries)) {
		t.Fatalf("reopened Len = %d, want %d", got, len(entries))
	}
}

func TestLegacyFormatRejected(t *testing.T) {
	dir := t.TempDir()
	// A pre-shard multi-file directory has a manifest and shard files and no
	// geoidx.dat; it must be reported rather than reinterpreted.
	if err := os.WriteFile(filepath.Join(dir, "manifest"), []byte("legacy"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir, tinyShardConfig()); !errors.Is(err, ErrLegacyFormat) {
		t.Fatalf("Open legacy = %v, want ErrLegacyFormat", err)
	}
}

// TestSingleFileExtentReuse verifies the persistent index uses exactly one file
// and that replaced extents are reclaimed and reused rather than growing the
// arena. The small arena forces reuse.
func TestSingleFileExtentReuse(t *testing.T) {
	dir := t.TempDir()
	cfg := tinyShardConfig()
	cfg.MaxFileBytes = 1 << 20 // 1 MiB arena, forcing extent reuse
	index, err := New(dir, cfg)
	if err != nil {
		t.Fatal(err)
	}
	entries := shardedMemoryEntries(3000)
	insertChunked(t, index, entries)
	if err := index.Checkpoint(); err != nil {
		t.Fatal(err)
	}

	// Churn single-image updates so extents are replaced and reused.
	for i := 0; i < 400; i++ {
		id := entries[i%len(entries)].ImageID
		key, err := KeyFromLatLng(testLat+0.05, testLng+float64(i)*1e-5)
		if err != nil {
			t.Fatal(err)
		}
		if err := index.Insert(key, id); err != nil {
			t.Fatal(err)
		}
		if err := index.Compact(); err != nil {
			t.Fatalf("compact %d: %v", i, err)
		}
	}
	stats := index.StorageStats()
	if stats.LiveBytes == 0 {
		t.Fatal("no live bytes")
	}
	if stats.FreeBytes == 0 {
		t.Fatal("expected reusable free space after churn")
	}
	if err := index.Close(); err != nil {
		t.Fatal(err)
	}
	files, _ := filepath.Glob(filepath.Join(dir, "*"))
	for _, f := range files {
		base := filepath.Base(f)
		if base != geoidxFile && base != "geoidx.lock" {
			t.Fatalf("sidecar file %s created", base)
		}
	}
}

// persistentShardedIndex builds a persistent index from entries and compacts it.
func persistentShardedIndex(tb testing.TB, dir string, cfg Config, entries []Entry) *Index {
	tb.Helper()
	index, err := New(dir, cfg)
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { _ = index.Close() })
	insertChunked(tb, index, entries)
	if err := index.Compact(); err != nil {
		tb.Fatal(err)
	}
	return index
}

// monotonicEntries returns n entries whose keys and ImageIDs both increase, so
// each key-ordered shard holds a contiguous, nearly disjoint ImageID range. This
// is the case where per-shard MinImageID/MaxImageID prunes affected-shard scans.
func monotonicEntries(n int) []Entry {
	keys := make([]S2Key, n)
	for i := range keys {
		k, err := KeyFromLatLng(testLat+float64(i)*1e-5, testLng)
		if err != nil {
			panic(err)
		}
		keys[i] = k
	}
	slices.Sort(keys)
	entries := make([]Entry, n)
	for i, k := range keys {
		entries[i] = Entry{Key: k, ImageID: schema.ImageID(i + 1)}
	}
	return entries
}

// TestAffectedShardPruningCorrectness deletes an image through a stale key in a
// monotonic index and requires the record to be removed while most shards are
// pruned by the MinImageID/MaxImageID filter.
func TestAffectedShardPruningCorrectness(t *testing.T) {
	dir := t.TempDir()
	cfg := tinyShardConfig()
	cfg.MaxFileBytes = 1 << 20
	entries := monotonicEntries(3000)
	index := persistentShardedIndex(t, dir, cfg, entries)
	total := len(index.view.Load().shards)
	if total < 4 {
		t.Fatalf("need several shards, got %d", total)
	}
	victim := entries[len(entries)/2]
	stale, err := KeyFromLatLng(testLat+10, testLng+10)
	if err != nil {
		t.Fatal(err)
	}
	if err := index.Delete(stale, victim.ImageID); err != nil {
		t.Fatal(err)
	}
	if err := index.Compact(); err != nil {
		t.Fatal(err)
	}
	if index.Contains(victim.Key, victim.ImageID) {
		t.Fatal("stale-key delete not applied under pruning")
	}
	if index.lastScannedShards >= total {
		t.Fatalf("pruning ineffective: scanned %d of %d shards", index.lastScannedShards, total)
	}
	if got, want := index.Len(), uint64(len(entries)-1); got != want {
		t.Fatalf("Len = %d, want %d", got, want)
	}
}

// TestConcurrentPersistentReadersDuringCompaction keeps lock-free readers on a
// single-file persistent index while writes force extent replacement and reuse.
// Run with -race.
func TestConcurrentPersistentReadersDuringCompaction(t *testing.T) {
	dir := t.TempDir()
	cfg := tinyShardConfig()
	cfg.MaxFileBytes = 4 << 20
	entries := shardedMemoryEntries(2500)
	index := persistentShardedIndex(t, dir, cfg, entries)

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
				if got := index.SearchRanges(nil, all); len(got) < 1000 {
					t.Errorf("reader observed %d ids", len(got))
					return
				}
			}
		}()
	}
	more := shardedMemoryEntries(800)
	for i := range more {
		more[i].ImageID = schema.ImageID(100000 + i)
	}
	insertChunked(t, index, more)
	close(stop)
	wg.Wait()
	if err := index.Compact(); err != nil {
		t.Fatal(err)
	}
	if got := index.Len(); got < uint64(len(entries)) {
		t.Fatalf("Len = %d, want >= %d", got, len(entries))
	}
}

// TestConcurrentReadersAcrossShardReplacement keeps lock-free readers running
// while writes force shard rebuilds, splits, and merges. Run with -race.
func TestConcurrentReadersAcrossShardReplacement(t *testing.T) {
	index, entries := shardedMemoryIndex(t, 2000, tinyShardConfig())
	want := referenceIDs(entries, []Range{{Start: 0, End: S2Key(^uint64(0))}})

	stop := make(chan struct{})
	var wg sync.WaitGroup
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			all := []Range{{Start: 0, End: S2Key(^uint64(0))}}
			m := roar.NewMutableBitmap()
			for {
				select {
				case <-stop:
					return
				default:
				}
				if got := index.SearchRanges(nil, all); len(got) < len(want)/2 {
					t.Errorf("reader observed %d ids, want >= %d", len(got), len(want)/2)
					return
				}
				m.Reset()
				index.CollectRanges(m, all)
			}
		}()
	}
	more := shardedMemoryEntries(500)
	for i := range more {
		more[i].ImageID = schema.ImageID(100000 + i)
	}
	insertChunked(t, index, more)
	close(stop)
	wg.Wait()
	if err := index.Compact(); err != nil {
		t.Fatal(err)
	}
}

// TestShardedMixedMutationDifferential applies a mixed insert/delete/move stream
// against a map reference, compacting frequently so shards split and merge.
func TestShardedMixedMutationDifferential(t *testing.T) {
	index, entries := shardedMemoryIndex(t, 3000, tinyShardConfig())
	live := map[schema.ImageID]Entry{}
	for _, e := range entries {
		live[e.ImageID] = e
	}
	rng := rand.New(rand.NewSource(5))
	keyFor := func(i int) S2Key {
		lat := testLat + (rng.Float64()*2-1)*testSpan
		lng := testLng + (rng.Float64()*2-1)*testSpan
		k, err := KeyFromLatLng(lat, lng)
		if err != nil {
			t.Fatal(err)
		}
		return k
	}

	nextID := schema.ImageID(len(entries) + 1)
	for step := 0; step < 400; step++ {
		switch rng.Intn(3) {
		case 0: // insert
			e := Entry{Key: keyFor(step), ImageID: nextID}
			nextID++
			if err := index.Insert(e.Key, e.ImageID); err != nil {
				t.Fatal(err)
			}
			live[e.ImageID] = e
		case 1: // delete an existing id (with a possibly-stale key)
			for id := range live {
				if err := index.Delete(keyFor(step), id); err != nil {
					t.Fatal(err)
				}
				delete(live, id)
				break
			}
		case 2: // move an existing id
			for id, old := range live {
				e := Entry{Key: keyFor(step), ImageID: id}
				if err := index.Delete(old.Key, id); err != nil {
					t.Fatal(err)
				}
				if err := index.Insert(e.Key, id); err != nil {
					t.Fatal(err)
				}
				live[id] = e
				break
			}
		}
		if step%50 == 0 {
			if err := index.Compact(); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := index.Compact(); err != nil {
		t.Fatal(err)
	}
	if got := index.Len(); got != uint64(len(live)) {
		t.Fatalf("Len = %d, want %d", got, len(live))
	}
	wantIDs := make([]schema.ImageID, 0, len(live))
	for id := range live {
		wantIDs = append(wantIDs, id)
	}
	slices.Sort(wantIDs)
	got := index.SearchRanges(nil, []Range{{Start: 0, End: S2Key(^uint64(0))}})
	if !slices.Equal(got, wantIDs) {
		t.Fatalf("live set mismatch: got %d ids, want %d", len(got), len(wantIDs))
	}
	for id, e := range live {
		if !index.Contains(e.Key, id) {
			t.Fatalf("image %d missing at %d", id, e.Key)
		}
	}
}
