package geoidx

import (
	"fmt"
	"testing"

	schema "github.com/evanoberholster/imageIdx/schema"
)

func shardBenchConfig(target int64) Config {
	return Config{
		TargetShardBytes: target,
		MaxShardBytes:    2 * target,
		MinShardBytes:    target / 4,
	}
}

// BenchmarkShardTargetSize compares a single monolithic shard with size-bounded
// shard targets over one dataset. The compact-small-update case is the point:
// it rewrites only the affected shard(s), so its cost tracks the target size
// rather than the whole base. Run at ~4M images so 8/16/32 MiB produce 8/4/2
// shards.
func BenchmarkShardTargetSize(b *testing.B) {
	const n = 4_000_000
	coords := geoBenchCoords(n, geoBenchSpan)
	entries := make([]Entry, n)
	for i, ll := range coords {
		key, err := KeyFromLatLng(ll.Lat.Degrees(), ll.Lng.Degrees())
		if err != nil {
			b.Fatal(err)
		}
		entries[i] = Entry{Key: key, ImageID: schema.ImageID(i + 1)}
	}
	ranges := CoverCap(nil, geoNewCap(2000), CoverOptions{MaxCells: 16})
	exactKey, _ := KeyFromLatLng(geoBenchLat, geoBenchLng)
	exact := []Range{{Start: exactKey, End: exactKey}}
	moveKey, _ := KeyFromLatLng(geoBenchLat+0.35, geoBenchLng)

	for _, tc := range []struct {
		name   string
		target int64
	}{
		{"single", 1 << 30},
		{"8MiB", 8 << 20},
		{"16MiB", 16 << 20},
		{"32MiB", 32 << 20},
	} {
		b.Run(tc.name, func(b *testing.B) {
			index, err := NewMemory(shardBenchConfig(tc.target))
			if err != nil {
				b.Fatal(err)
			}
			b.Cleanup(func() { _ = index.Close() })
			if err := index.InsertBatch(entries); err != nil {
				b.Fatal(err)
			}
			if err := index.Compact(); err != nil {
				b.Fatal(err)
			}
			b.Logf("shards=%d", len(index.view.Load().shards))

			b.Run("range", func(b *testing.B) {
				dst := make([]schema.ImageID, 0, 4096)
				b.ReportAllocs()
				for i := 0; i < b.N; i++ {
					dst = index.SearchRanges(dst[:0], ranges)
				}
			})
			b.Run("exact", func(b *testing.B) {
				dst := make([]schema.ImageID, 0, 256)
				b.ReportAllocs()
				for i := 0; i < b.N; i++ {
					dst = index.SearchRanges(dst[:0], exact)
				}
			})
			b.Run("nearest10", func(b *testing.B) {
				res := make([]Result, 0, 10)
				b.ReportAllocs()
				for i := 0; i < b.N; i++ {
					res, _ = index.Nearest(res[:0], geoBenchLat, geoBenchLng, 10)
				}
			})
			b.Run("compact-small-update", func(b *testing.B) {
				victim := entries[n/2]
				b.ReportAllocs()
				for i := 0; i < b.N; i++ {
					if err := index.Delete(victim.Key, victim.ImageID); err != nil {
						b.Fatal(err)
					}
					if err := index.Insert(moveKey, victim.ImageID); err != nil {
						b.Fatal(err)
					}
					if err := index.Compact(); err != nil {
						b.Fatal(err)
					}
				}
			})
		})
	}
}

// BenchmarkAffectedShardSelection isolates the affected-shard scan of a
// compaction (no disk I/O): one delete on a 1M-entry memory index. With
// monotonic ImageIDs each shard's MinImageID/MaxImageID range is nearly disjoint,
// so the scan prunes to the victim's shard; with random IDs the ranges overlap
// and most shards are scanned. shards-scanned reports the pruned count.
func BenchmarkAffectedShardSelection(b *testing.B) {
	for _, tc := range []struct {
		name    string
		entries func(int) []Entry
	}{
		{"monotonic", monotonicEntries},
		{"clustered", shardedMemoryEntries},
	} {
		b.Run(tc.name, func(b *testing.B) {
			cfg := Config{TargetShardBytes: 256 << 10, MaxShardBytes: 512 << 10, MinShardBytes: 64 << 10}
			index, err := NewMemory(cfg)
			if err != nil {
				b.Fatal(err)
			}
			b.Cleanup(func() { _ = index.Close() })
			entries := tc.entries(1_000_000)
			if err := index.InsertBatch(entries); err != nil {
				b.Fatal(err)
			}
			if err := index.Compact(); err != nil {
				b.Fatal(err)
			}
			victim := entries[len(entries)/2]
			stale, err := KeyFromLatLng(testLat+10, testLng+10)
			if err != nil {
				b.Fatal(err)
			}
			delta := []mutation{{id: victim.ImageID, key: stale, add: false}}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := index.prepareShards(index.shards, delta); err != nil {
					b.Fatal(err)
				}
			}
			b.StopTimer()
			b.ReportMetric(float64(index.lastScannedShards), "shards-scanned")
			b.ReportMetric(float64(len(index.shards)), "shards-total")
		})
	}
}

// BenchmarkSingleFileCompact measures a one-image update plus a full
// single-file compaction on a persistent index, across shard target sizes, and
// reports the resulting free-space fragmentation. The arena is fixed, so the
// file never grows: replaced extents are reused.
func BenchmarkSingleFileCompact(b *testing.B) {
	for _, target := range []int64{1 << 20, 4 << 20} {
		b.Run(fmt.Sprintf("target=%dMiB", target>>20), func(b *testing.B) {
			dir := b.TempDir()
			cfg := Config{
				TargetShardBytes: target,
				MaxShardBytes:    2 * target,
				MinShardBytes:    target / 4,
				MaxFileBytes:     96 << 20,
			}
			entries := shardedMemoryEntries(500_000)
			index := persistentShardedIndex(b, dir, cfg, entries)
			victim := entries[len(entries)/2]
			moveKey, err := KeyFromLatLng(testLat+0.3, testLng)
			if err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if err := index.Delete(victim.Key, victim.ImageID); err != nil {
					b.Fatal(err)
				}
				if err := index.Insert(moveKey, victim.ImageID); err != nil {
					b.Fatal(err)
				}
				if err := index.Compact(); err != nil {
					b.Fatal(err)
				}
			}
			b.StopTimer()
			stats := index.StorageStats()
			b.ReportMetric(float64(stats.FreeBytes), "free-B")
			b.ReportMetric(float64(stats.LargestFree), "largest-free-B")
			b.ReportMetric(float64(stats.FreeRuns), "free-runs")
		})
	}
}

// BenchmarkShardInsertBatch measures small-batch writes with rotation across
// shard target sizes on a 1M base.
func BenchmarkShardInsertBatch(b *testing.B) {
	const n = 1_000_000
	base := shardedMemoryEntries(n)
	for _, tc := range []struct {
		name   string
		target int64
	}{
		{"single", 1 << 30},
		{"1MiB", 1 << 20},
		{"4MiB", 4 << 20},
	} {
		b.Run(tc.name, func(b *testing.B) {
			index, err := NewMemory(shardBenchConfig(tc.target))
			if err != nil {
				b.Fatal(err)
			}
			b.Cleanup(func() { _ = index.Close() })
			if err := index.InsertBatch(base); err != nil {
				b.Fatal(err)
			}
			if err := index.Compact(); err != nil {
				b.Fatal(err)
			}
			coords := geoBenchCoords(64, geoBenchSpan)
			batch := make([]Entry, 64)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				for j := range batch {
					key, err := KeyFromLatLng(coords[j].Lat.Degrees(), coords[j].Lng.Degrees())
					if err != nil {
						b.Fatal(err)
					}
					batch[j] = Entry{Key: key, ImageID: schema.ImageID(n + i*64 + j + 1)}
				}
				if err := index.InsertBatch(batch); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
