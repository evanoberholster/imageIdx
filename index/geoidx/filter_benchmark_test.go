package geoidx

import (
	"fmt"
	"math/rand"
	"sync"
	"testing"

	common "github.com/evanoberholster/imageIdx/common"
	"github.com/evanoberholster/imageIdx/roar"
	"github.com/golang/geo/s2"
)

// geoBenchClustered mimics a photo library: a few dense city clusters plus a
// thin uniform background.
func geoBenchClustered(n int) ([]Entry, []s2.LatLng) {
	cities := [][2]float64{
		{48.8566, 2.3522}, {51.5074, -0.1278}, {40.7128, -74.0060},
		{35.6762, 139.6503}, {37.7749, -122.4194}, {-33.8688, 151.2093},
		{55.7558, 37.6173}, {-23.5505, -46.6333},
	}
	rng := rand.New(rand.NewSource(11))
	entries := make([]Entry, n)
	coords := make([]s2.LatLng, n)
	for i := range entries {
		var lat, lng float64
		if i%20 == 0 { // 5% uniform background
			lat = -60 + rng.Float64()*120
			lng = -180 + rng.Float64()*360
		} else {
			city := cities[rng.Intn(len(cities))]
			lat = city[0] + (rng.Float64()*2-1)*0.05
			lng = city[1] + (rng.Float64()*2-1)*0.05
		}
		key, err := KeyFromLatLng(lat, lng)
		if err != nil {
			panic(err)
		}
		entries[i] = Entry{Key: key, ImageID: uint64(i + 1)}
		coords[i] = s2.LatLngFromDegrees(lat, lng)
	}
	return entries, coords
}

func buildGeoClustered(b *testing.B, n int) *Index {
	b.Helper()
	entries, _ := geoBenchClustered(n)
	index, err := NewMemory(Config{})
	if err != nil {
		b.Fatal(err)
	}
	if err := index.InsertBatch(entries); err != nil {
		b.Fatal(err)
	}
	if err := index.Compact(); err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = index.Close() })
	return index
}

// BenchmarkGeoFilterNew compares the two geographic query modes at 1M uniform
// points over a 2 km cover: initial []ImageID, direct collection, and FilterBy
// against candidate bitmaps of increasing size.
func BenchmarkGeoFilterNew(b *testing.B) {
	index, _ := buildGeoNew(b, geoBenchN, geoBenchSpan)
	ranges := CoverCap(nil, geoNewCap(2000), CoverOptions{MaxCells: 16})
	dst := make([]uint64, 0, 4096)

	b.Run("initial", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			dst = index.SearchRanges(dst[:0], ranges)
		}
	})
	b.Run("collect", func(b *testing.B) {
		m := roar.NewMutableBitmap()
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			m.Reset()
			index.CollectRanges(m, ranges)
			_ = m.Cardinality()
		}
	})
	for _, tc := range []struct {
		name string
		size int
	}{
		{"filter/small", geoBenchN / 1000},
		{"filter/medium", geoBenchN / 20},
		{"filter/large", geoBenchN},
	} {
		b.Run(tc.name, func(b *testing.B) {
			candidates := make([]uint64, tc.size)
			for i := range candidates {
				candidates[i] = uint64(i + 1)
			}
			bit := roar.MustFromSorted(candidates)
			m := roar.NewMutableBitmap()
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				m.Reset()
				index.FilterRanges(m, bit, ranges)
				_ = m.Cardinality()
			}
		})
	}
}

// BenchmarkGeoClusteredNew exercises a realistic clustered distribution.
func BenchmarkGeoClusteredNew(b *testing.B) {
	index := buildGeoClustered(b, 1_000_000)
	ranges := CoverCap(nil, geoNewCap(2000), CoverOptions{MaxCells: 16})
	dst := make([]uint64, 0, 4096)
	b.Run("range", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			dst = index.SearchRanges(dst[:0], ranges)
		}
	})
	b.Run("nearest10", func(b *testing.B) {
		res := make([]Result, 0, 10)
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			res, _ = index.Nearest(res[:0], 48.8566, 2.3522, 10)
		}
	})
	bit := roar.MustFromSorted(func() []uint64 {
		ids := make([]uint64, 0, 1_000_000)
		for id := uint64(1); id <= 1_000_000; id++ {
			ids = append(ids, id)
		}
		return ids
	}())
	m := roar.NewMutableBitmap()
	b.Run("filter/all", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			m.Reset()
			index.FilterRanges(m, bit, ranges)
			_ = m.Cardinality()
		}
	})
}

// BenchmarkGeoScale10M checks that the design holds at production scale.
func BenchmarkGeoScale10M(b *testing.B) {
	const n = 10_000_000
	index, _ := buildGeoNew(b, n, geoBenchSpan)
	ranges := CoverCap(nil, geoNewCap(2000), CoverOptions{MaxCells: 16})
	dst := make([]uint64, 0, 4096)
	b.Run("initial", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			dst = index.SearchRanges(dst[:0], ranges)
		}
	})
	b.Run("collect", func(b *testing.B) {
		m := roar.NewMutableBitmap()
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			m.Reset()
			index.CollectRanges(m, ranges)
			_ = m.Cardinality()
		}
	})
	b.Run(fmt.Sprintf("nearest10"), func(b *testing.B) {
		res := make([]Result, 0, 10)
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			res, _ = index.Nearest(res[:0], geoBenchLat, geoBenchLng, 10)
		}
	})
}

// BenchmarkGeoCollectStrategy compares bitmap collection strategies: bounded
// chunked AddMany (the collector), a fully buffered unsorted AddMany, and a
// fully buffered sorted AddMany, over the same native scan.
func BenchmarkGeoCollectStrategy(b *testing.B) {
	index, _ := buildGeoNew(b, 1_000_000, geoBenchSpan)
	ranges := CoverCap(nil, geoNewCap(2000), CoverOptions{MaxCells: 16})

	b.Run("chunked-addmany", func(b *testing.B) {
		m := roar.NewMutableBitmap()
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			m.Reset()
			index.CollectRanges(m, ranges)
			_ = m.Cardinality()
		}
	})
	b.Run("buffered-unsorted", func(b *testing.B) {
		m := roar.NewMutableBitmap()
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			m.Reset()
			var buf []uint64
			index.withView(func(v *view) {
				scanRanges(v, ranges, func(_ S2Key, id uint64) bool {
					buf = append(buf, id)
					return true
				})
			})
			m.AddMany(buf)
			_ = m.Cardinality()
		}
	})
	b.Run("buffered-sorted", func(b *testing.B) {
		m := roar.NewMutableBitmap()
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			m.Reset()
			var buf []uint64
			maxID := uint64(0)
			index.withView(func(v *view) {
				maxID = uint64(v.maxID)
				scanRanges(v, ranges, func(_ S2Key, id uint64) bool {
					buf = append(buf, id)
					return true
				})
			})
			buf = sortIDs(buf, common.ImageID(maxID))
			m.AddMany(buf)
			_ = m.Cardinality()
		}
	})
}

// BenchmarkGeoQueryPendingMutations measures steady-state query latency across
// pending-delta sizes. The effective delta is merged once per view, so the
// warm-up iteration absorbs the merge and the timed loop measures steady state.
func BenchmarkGeoQueryPendingMutations(b *testing.B) {
	ranges := CoverCap(nil, geoNewCap(2000), CoverOptions{MaxCells: 16})
	for _, pending := range []int{0, 64, 4096, 65536} {
		b.Run(fmt.Sprintf("pending=%d", pending), func(b *testing.B) {
			index, err := NewMemory(Config{TargetDelta: 64, CompactionTrigger: 1 << 30, MaxDelta: 1 << 31})
			if err != nil {
				b.Fatal(err)
			}
			defer index.Close()
			if err := index.InsertBatch(entryBatch(testKeys(200_000), 1)); err != nil {
				b.Fatal(err)
			}
			if err := index.Compact(); err != nil {
				b.Fatal(err)
			}
			keys := testKeys(pending)
			for n := 0; n < pending; n += 64 {
				batch := make([]Entry, 0, 64)
				for k := 0; k < 64 && n+k < pending; k++ {
					batch = append(batch, Entry{Key: keys[n+k], ImageID: uint64(1_000_000 + n + k)})
				}
				if err := index.InsertBatch(batch); err != nil {
					b.Fatal(err)
				}
			}
			dst := make([]uint64, 0, 4096)
			dst = index.SearchRanges(dst[:0], ranges) // warm the per-view merge
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				dst = index.SearchRanges(dst[:0], ranges)
			}
		})
	}
}

func entryBatch(keys []S2Key, base uint64) []Entry {
	out := make([]Entry, len(keys))
	for i, key := range keys {
		out[i] = Entry{Key: key, ImageID: base + uint64(i)}
	}
	return out
}

// BenchmarkGeoWriterWithReaders measures 64-entry insert throughput while
// readers continuously pin the current view, checking that a writer never waits
// on active readers.
func BenchmarkGeoWriterWithReaders(b *testing.B) {
	ranges := CoverCap(nil, geoNewCap(2000), CoverOptions{MaxCells: 16})
	for _, readers := range []int{0, 4} {
		b.Run(fmt.Sprintf("readers=%d", readers), func(b *testing.B) {
			index, err := NewMemory(Config{TargetDelta: 64, CompactionTrigger: 1 << 20, MaxDelta: 1 << 21})
			if err != nil {
				b.Fatal(err)
			}
			defer index.Close()
			if err := index.InsertBatch(entryBatch(testKeys(100_000), 1)); err != nil {
				b.Fatal(err)
			}
			if err := index.Compact(); err != nil {
				b.Fatal(err)
			}
			stop := make(chan struct{})
			var wg sync.WaitGroup
			for r := 0; r < readers; r++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					for {
						select {
						case <-stop:
							return
						default:
						}
						_ = index.SearchRanges(nil, ranges)
					}
				}()
			}
			keys := testKeys(64)
			batch := make([]Entry, 64)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				for n := range batch {
					batch[n] = Entry{Key: keys[n], ImageID: uint64(1_000_000 + i*64 + n)}
				}
				if err := index.InsertBatch(batch); err != nil {
					b.Fatal(err)
				}
			}
			b.StopTimer()
			close(stop)
			wg.Wait()
		})
	}
}
func BenchmarkGeoInsertRotation(b *testing.B) {
	for _, trigger := range []int{1 << 20, 256} {
		b.Run(fmt.Sprintf("trigger=%d", trigger), func(b *testing.B) {
			index, err := NewMemory(Config{TargetDelta: 64, CompactionTrigger: trigger, MaxDelta: 2 * trigger})
			if err != nil {
				b.Fatal(err)
			}
			defer index.Close()
			insertKeys(b, index, testKeys(100_000), 1)
			batch := make([]Entry, 64)
			keys := testKeys(64)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				for n := range batch {
					batch[n] = Entry{Key: keys[n], ImageID: uint64(1_000_000 + i*64 + n)}
				}
				if err := index.InsertBatch(batch); err != nil {
					b.Fatal(err)
				}
			}
			b.StopTimer()
			_ = index.Compact()
		})
	}
}
