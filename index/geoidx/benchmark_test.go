package geoidx

import (
	"fmt"
	"math/rand"
	"slices"
	"sort"
	"sync"
	"testing"

	common "github.com/evanoberholster/imageIdx/common"
	"github.com/evanoberholster/imageIdx/roar"
	"github.com/golang/geo/s2"
)

const benchLat, benchLng = 37.7749, -122.4194

func benchEntries(n int, spread float64, seed int64) []Entry {
	rng := rand.New(rand.NewSource(seed))
	entries := make([]Entry, n)
	for i := range entries {
		lat := benchLat + (rng.Float64()*2-1)*spread
		lng := benchLng + (rng.Float64()*2-1)*spread
		key, _ := KeyFromLatLng(lat, lng)
		entries[i] = Entry{Key: key, ImageID: uint64(i + 1)}
	}
	return entries
}

func benchIndex(b *testing.B, n int, spread float64, persistent bool) *Index {
	b.Helper()
	var index *Index
	var err error
	if persistent {
		index, err = New(b.TempDir(), Config{})
	} else {
		index, err = NewMemory(Config{})
	}
	if err != nil {
		b.Fatalf("index: %v", err)
	}
	if err := index.InsertBatch(benchEntries(n, spread, 1)); err != nil {
		b.Fatalf("InsertBatch: %v", err)
	}
	if err := index.Compact(); err != nil {
		b.Fatalf("Compact: %v", err)
	}
	b.Cleanup(func() { _ = index.Close() })
	return index
}

func reportScan(b *testing.B, index *Index, ranges []Range) {
	b.Helper()
	entries := index.EstimateRanges(ranges)
	b.ReportMetric(float64(entries*16), "bytes/query")
}

func BenchmarkSearchCell(b *testing.B) {
	index := benchIndex(b, 1_000_000, 0.4, false)
	key, _ := KeyFromLatLng(benchLat, benchLng)
	ranges := []Range{{Start: key, End: key}}
	dst := make([]uint64, 0, 256)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		dst = index.SearchRanges(dst[:0], ranges)
	}
	reportScan(b, index, ranges)
}

func BenchmarkSearchSingleRange(b *testing.B) {
	index := benchIndex(b, 1_000_000, 0.4, false)
	key, _ := KeyFromLatLng(benchLat, benchLng)
	coarse := s2.CellID(DecodeKey(key)).Parent(14)
	ranges, _ := RangesFromCellIDs(nil, []uint64{uint64(coarse)})
	dst := make([]uint64, 0, 1024)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		dst = index.SearchRanges(dst[:0], ranges)
	}
	reportScan(b, index, ranges)
}

func BenchmarkSearchMultiRange(b *testing.B) {
	index := benchIndex(b, 1_000_000, 0.4, false)
	center := s2.LatLngFromDegrees(benchLat, benchLng)
	ranges := CoverCap(nil, CapFromMeters(center, 5000), CoverOptions{MaxCells: 16})
	dst := make([]uint64, 0, 4096)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		dst = index.SearchRanges(dst[:0], ranges)
	}
	reportScan(b, index, ranges)
}

func BenchmarkSearchBoundingBox(b *testing.B) {
	index := benchIndex(b, 1_000_000, 0.4, false)
	rect := s2.RectFromCenterSize(s2.LatLngFromDegrees(benchLat, benchLng), s2.LatLngFromDegrees(0.05, 0.05))
	dst := make([]uint64, 0, 4096)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		dst = index.SearchRect(dst[:0], rect, CoverOptions{MaxCells: 16})
	}
}

func BenchmarkSearchPolygon(b *testing.B) {
	index := benchIndex(b, 1_000_000, 0.4, false)
	polygon := s2.PolygonFromLoops([]*s2.Loop{s2.LoopFromPoints([]s2.Point{
		s2.PointFromLatLng(s2.LatLngFromDegrees(benchLat-0.03, benchLng-0.03)),
		s2.PointFromLatLng(s2.LatLngFromDegrees(benchLat-0.03, benchLng+0.03)),
		s2.PointFromLatLng(s2.LatLngFromDegrees(benchLat+0.03, benchLng+0.03)),
		s2.PointFromLatLng(s2.LatLngFromDegrees(benchLat+0.03, benchLng-0.03)),
	})})
	dst := make([]uint64, 0, 4096)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		dst = index.SearchPolygon(dst[:0], polygon, CoverOptions{MaxCells: 16})
	}
}

func BenchmarkSearchRadius(b *testing.B) {
	index := benchIndex(b, 1_000_000, 0.4, false)
	dst := make([]uint64, 0, 4096)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		dst, _ = index.SearchRadius(dst[:0], benchLat, benchLng, 2000, CoverOptions{MaxCells: 16})
	}
}

func BenchmarkNearest(b *testing.B) {
	index := benchIndex(b, 1_000_000, 0.4, false)
	for _, k := range []int{1, 10, 100} {
		b.Run(fmt.Sprintf("k=%d", k), func(b *testing.B) {
			dst := make([]Result, 0, k)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				dst, _ = index.Nearest(dst[:0], benchLat, benchLng, k)
			}
		})
	}
}

func BenchmarkRepeatedKey(b *testing.B) {
	index, _ := NewMemory(Config{})
	defer index.Close()
	key, _ := KeyFromLatLng(benchLat, benchLng)
	entries := make([]Entry, 10_000)
	for i := range entries {
		entries[i] = Entry{Key: key, ImageID: uint64(i + 1)}
	}
	if err := index.InsertBatch(entries); err != nil {
		b.Fatalf("InsertBatch: %v", err)
	}
	if err := index.Compact(); err != nil {
		b.Fatalf("Compact: %v", err)
	}
	ranges := []Range{{Start: key, End: key}}
	dst := make([]uint64, 0, 10_000)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		dst = index.SearchRanges(dst[:0], ranges)
	}
}

func BenchmarkDistribution(b *testing.B) {
	for _, tc := range []struct {
		name   string
		spread float64
	}{
		{"sparse", 2.0},
		{"dense", 0.005},
	} {
		b.Run(tc.name, func(b *testing.B) {
			index := benchIndex(b, 500_000, tc.spread, false)
			center := s2.LatLngFromDegrees(benchLat, benchLng)
			ranges := CoverCap(nil, CapFromMeters(center, 500), CoverOptions{MaxCells: 16})
			dst := make([]uint64, 0, 1024)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				dst = index.SearchRanges(dst[:0], ranges)
			}
		})
	}
}

func BenchmarkInsertBatch(b *testing.B) {
	for _, size := range []int{1, 8, 32, 64} {
		b.Run(fmt.Sprintf("n=%d", size), func(b *testing.B) {
			entries := benchEntries(size, 0.4, 2)
			batch := make([]Entry, size)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				index, _ := NewMemory(Config{})
				for n := range batch {
					batch[n] = entries[n]
					batch[n].ImageID = uint64(i*size + n + 1)
				}
				if err := index.InsertBatch(batch); err != nil {
					b.Fatal(err)
				}
				index.Close()
			}
		})
	}
}

func BenchmarkCompact(b *testing.B) {
	index, _ := NewMemory(Config{})
	defer index.Close()
	entries := benchEntries(1_000_000, 0.4, 3)
	if err := index.InsertBatch(entries); err != nil {
		b.Fatalf("InsertBatch: %v", err)
	}
	if err := index.Compact(); err != nil {
		b.Fatalf("Compact: %v", err)
	}
	delta := benchEntries(64, 0.4, 4)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for n := range delta {
			delta[n].ImageID = uint64(2_000_000 + i*64 + n)
		}
		if err := index.InsertBatch(delta); err != nil {
			b.Fatal(err)
		}
		if err := index.Compact(); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkReopen(b *testing.B) {
	dir := b.TempDir()
	index, err := New(dir, Config{})
	if err != nil {
		b.Fatal(err)
	}
	if err := index.InsertBatch(benchEntries(1_000_000, 0.4, 5)); err != nil {
		b.Fatal(err)
	}
	if err := index.Checkpoint(); err != nil {
		b.Fatal(err)
	}
	if err := index.Close(); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		reopened, err := Open(dir, Config{})
		if err != nil {
			b.Fatal(err)
		}
		if err := reopened.Close(); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkFilterCandidates(b *testing.B) {
	index := benchIndex(b, 1_000_000, 0.4, false)
	center := s2.LatLngFromDegrees(benchLat, benchLng)
	ranges := CoverCap(nil, CapFromMeters(center, 2000), CoverOptions{MaxCells: 16})
	geoIDs := index.SearchRanges(nil, ranges)
	total := index.Len()
	for _, tc := range []struct {
		name string
		size uint64
	}{
		{"small", total / 1000},
		{"medium", total / 20},
		{"large", total},
	} {
		b.Run(tc.name, func(b *testing.B) {
			candidates := make([]uint64, 0, tc.size)
			for id := uint64(1); id <= tc.size; id++ {
				candidates = append(candidates, id)
			}
			bitmap := roar.MustFromSorted(candidates)
			dst := make([]uint64, 0, len(geoIDs))
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				dst = dst[:0]
				for _, id := range geoIDs {
					if bitmap.Contains(id) {
						dst = append(dst, id)
					}
				}
			}
		})
	}
}

// Identical workloads to index/bitmapidx's geocompare_benchmark_test.go (on the
// pre-geoidx commit), so the two implementations can be compared directly.

const (
	geoBenchLat  = 37.7749
	geoBenchLng  = -122.4194
	geoBenchN    = 200_000
	geoBenchSpan = 0.4
)

func geoBenchCoords(n int, spread float64) []s2.LatLng {
	rng := rand.New(rand.NewSource(1))
	coords := make([]s2.LatLng, n)
	for i := range coords {
		lat := geoBenchLat + (rng.Float64()*2-1)*spread
		lng := geoBenchLng + (rng.Float64()*2-1)*spread
		coords[i] = s2.LatLngFromDegrees(lat, lng)
	}
	return coords
}

func buildGeoNew(b *testing.B, n int, spread float64) (*Index, []s2.LatLng) {
	b.Helper()
	coords := geoBenchCoords(n, spread)
	index, err := NewMemory(Config{})
	if err != nil {
		b.Fatal(err)
	}
	entries := make([]Entry, n)
	for i, ll := range coords {
		key, err := KeyFromLatLng(ll.Lat.Degrees(), ll.Lng.Degrees())
		if err != nil {
			b.Fatal(err)
		}
		entries[i] = Entry{Key: key, ImageID: uint64(i + 1)}
	}
	if err := index.InsertBatch(entries); err != nil {
		b.Fatal(err)
	}
	if err := index.Compact(); err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = index.Close() })
	return index, coords
}

func geoNewCap(radiusMeters float64) s2.Cap {
	return CapFromMeters(s2.LatLngFromDegrees(geoBenchLat, geoBenchLng), radiusMeters)
}

func BenchmarkGeoNewExactCell(b *testing.B) {
	index, coords := buildGeoNew(b, geoBenchN, geoBenchSpan)
	key, err := KeyFromLatLng(coords[0].Lat.Degrees(), coords[0].Lng.Degrees())
	if err != nil {
		b.Fatal(err)
	}
	ranges := []Range{{Start: key, End: key}}
	dst := make([]uint64, 0, 256)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		dst = index.SearchRanges(dst[:0], ranges)
	}
}

func BenchmarkGeoNewSingleRange(b *testing.B) {
	index, _ := buildGeoNew(b, geoBenchN, geoBenchSpan)
	cell := s2.CellIDFromLatLng(s2.LatLngFromDegrees(geoBenchLat, geoBenchLng)).Parent(14)
	ranges, err := RangesFromCellIDs(nil, []uint64{uint64(cell)})
	if err != nil {
		b.Fatal(err)
	}
	dst := make([]uint64, 0, 1024)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		dst = index.SearchRanges(dst[:0], ranges)
	}
}

func BenchmarkGeoNewMultiRange(b *testing.B) {
	index, _ := buildGeoNew(b, geoBenchN, geoBenchSpan)
	ranges := CoverCap(nil, geoNewCap(2000), CoverOptions{MaxCells: 16})
	dst := make([]uint64, 0, 4096)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		dst = index.SearchRanges(dst[:0], ranges)
	}
}

func BenchmarkGeoNewBoundingBox(b *testing.B) {
	index, _ := buildGeoNew(b, geoBenchN, geoBenchSpan)
	rect := s2.RectFromCenterSize(s2.LatLngFromDegrees(geoBenchLat, geoBenchLng), s2.LatLngFromDegrees(0.05, 0.05))
	ranges := CoverRect(nil, rect, CoverOptions{MaxCells: 16})
	dst := make([]uint64, 0, 4096)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		dst = index.SearchRanges(dst[:0], ranges)
	}
}

func BenchmarkGeoNewPolygon(b *testing.B) {
	index, _ := buildGeoNew(b, geoBenchN, geoBenchSpan)
	polygon := s2.PolygonFromLoops([]*s2.Loop{s2.LoopFromPoints([]s2.Point{
		s2.PointFromLatLng(s2.LatLngFromDegrees(geoBenchLat-0.03, geoBenchLng-0.03)),
		s2.PointFromLatLng(s2.LatLngFromDegrees(geoBenchLat-0.03, geoBenchLng+0.03)),
		s2.PointFromLatLng(s2.LatLngFromDegrees(geoBenchLat+0.03, geoBenchLng+0.03)),
		s2.PointFromLatLng(s2.LatLngFromDegrees(geoBenchLat+0.03, geoBenchLng-0.03)),
	})})
	ranges := CoverPolygon(nil, polygon, CoverOptions{MaxCells: 16})
	dst := make([]uint64, 0, 4096)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		dst = index.SearchRanges(dst[:0], ranges)
	}
}

func BenchmarkGeoNewNearest(b *testing.B) {
	index, _ := buildGeoNew(b, geoBenchN, geoBenchSpan)
	for _, k := range []int{1, 10, 100} {
		b.Run(fmt.Sprintf("k=%d", k), func(b *testing.B) {
			dst := make([]Result, 0, k)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				dst, _ = index.Nearest(dst[:0], geoBenchLat, geoBenchLng, k)
			}
		})
	}
}

func BenchmarkGeoNewRepeatedKey(b *testing.B) {
	const n = 10_000
	key, err := KeyFromLatLng(geoBenchLat, geoBenchLng)
	if err != nil {
		b.Fatal(err)
	}
	index, _ := NewMemory(Config{})
	defer index.Close()
	entries := make([]Entry, n)
	for i := range entries {
		entries[i] = Entry{Key: key, ImageID: uint64(i + 1)}
	}
	if err := index.InsertBatch(entries); err != nil {
		b.Fatal(err)
	}
	if err := index.Compact(); err != nil {
		b.Fatal(err)
	}
	ranges := []Range{{Start: key, End: key}}
	dst := make([]uint64, 0, n)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		dst = index.SearchRanges(dst[:0], ranges)
	}
}

func BenchmarkGeoNewDistribution(b *testing.B) {
	for _, tc := range []struct {
		name   string
		spread float64
	}{
		{"sparse", 2.0},
		{"dense", 0.005},
	} {
		b.Run(tc.name, func(b *testing.B) {
			index, _ := buildGeoNew(b, 500_000, tc.spread)
			ranges := CoverCap(nil, geoNewCap(500), CoverOptions{MaxCells: 16})
			dst := make([]uint64, 0, 4096)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				dst = index.SearchRanges(dst[:0], ranges)
			}
		})
	}
}

func BenchmarkGeoNewFilterBitmap(b *testing.B) {
	index, _ := buildGeoNew(b, geoBenchN, geoBenchSpan)
	ranges := CoverCap(nil, geoNewCap(2000), CoverOptions{MaxCells: 16})
	geoIDs := index.SearchRanges(nil, ranges)
	for _, tc := range []struct {
		name string
		size int
	}{
		{"small", geoBenchN / 1000},
		{"medium", geoBenchN / 20},
		{"large", geoBenchN},
	} {
		b.Run(tc.name, func(b *testing.B) {
			candidates := make([]uint64, tc.size)
			for i := range candidates {
				candidates[i] = uint64(i + 1)
			}
			bitmap := roar.MustFromSorted(candidates)
			dst := make([]uint64, 0, len(geoIDs))
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				dst = dst[:0]
				for _, id := range geoIDs {
					if bitmap.Contains(id) {
						dst = append(dst, id)
					}
				}
			}
		})
	}
}

func BenchmarkGeoNewInsertBatch(b *testing.B) {
	for _, size := range []int{1, 8, 32, 64} {
		b.Run(fmt.Sprintf("n=%d", size), func(b *testing.B) {
			coords := geoBenchCoords(size, geoBenchSpan)
			batch := make([]Entry, size)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				for n := range batch {
					key, err := KeyFromLatLng(coords[n].Lat.Degrees(), coords[n].Lng.Degrees())
					if err != nil {
						b.Fatal(err)
					}
					batch[n] = Entry{Key: key, ImageID: uint64(i*size + n + 1)}
				}
				index, err := NewMemory(Config{})
				if err != nil {
					b.Fatal(err)
				}
				if err := index.InsertBatch(batch); err != nil {
					b.Fatal(err)
				}
				_ = index.Close()
			}
		})
	}
}

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

// --- Paging experiment (see PAGING.md) ---

func buildGeoBase(tb testing.TB, n int, spread float64) *Index {
	tb.Helper()
	coords := geoBenchCoords(n, spread)
	index, err := NewMemory(Config{})
	if err != nil {
		tb.Fatal(err)
	}
	entries := make([]Entry, n)
	for i, ll := range coords {
		key, err := KeyFromLatLng(ll.Lat.Degrees(), ll.Lng.Degrees())
		if err != nil {
			tb.Fatal(err)
		}
		entries[i] = Entry{Key: key, ImageID: uint64(i + 1)}
	}
	if err := index.InsertBatch(entries); err != nil {
		tb.Fatal(err)
	}
	if err := index.Compact(); err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { _ = index.Close() })
	return index
}

// flattenView concatenates a view's shards into one (keys, ids) pair. Benchmarks
// that need a single flat SoA array use it outside their timed loop.
func flattenView(v *view) ([]S2Key, []common.ImageID) {
	keys := make([]S2Key, 0, v.entries)
	ids := make([]common.ImageID, 0, v.entries)
	for _, s := range v.shards {
		keys = append(keys, s.keys...)
		ids = append(ids, s.ids...)
	}
	return keys, ids
}

// pageDir is a compact single-level directory over the flat sorted key array.
type pageDir struct {
	firstKeys   []S2Key
	offsets     []int
	pageRecords int
}

func buildPageDir(keys []S2Key, pageRecords int) pageDir {
	d := pageDir{pageRecords: pageRecords}
	for i := 0; i < len(keys); i += pageRecords {
		d.firstKeys = append(d.firstKeys, keys[i])
		d.offsets = append(d.offsets, i)
	}
	return d
}

// locate returns the first index whose key is >= key. It finds the last page
// whose first key is <= key and binary-searches within it. That page is the one
// that can contain the first occurrence even when a repeated key spans several
// pages: the run may begin in that page's tail, and if it begins at the next
// page's start the in-page search returns this page's end, which is that start.
func (d pageDir) locate(keys []S2Key, key S2Key) int {
	if len(d.firstKeys) == 0 {
		return 0
	}
	// First page whose first key is >= key; step back one to cover a run that
	// starts in the previous page.
	p := sort.Search(len(d.firstKeys), func(i int) bool { return d.firstKeys[i] >= key })
	if p > 0 {
		p--
	}
	base := d.offsets[p]
	end := base + d.pageRecords
	if end > len(keys) {
		end = len(keys)
	}
	return base + sort.Search(end-base, func(i int) bool { return keys[base+i] >= key })
}

func pagedSearch(keys []S2Key, ids []common.ImageID, d pageDir, ranges []Range, dst []common.ImageID) []common.ImageID {
	mark := len(dst)
	for _, r := range ranges {
		i := d.locate(keys, r.Start)
		for i < len(keys) && keys[i] <= r.End {
			dst = append(dst, ids[i])
			i++
		}
	}
	tail := dst[mark:]
	if !slices.IsSorted(tail) {
		slices.Sort(tail)
	}
	return dst
}

// flatSearch mirrors pagedSearch but binary-searches the whole key array, so the
// only difference is the page directory.
func flatSearch(keys []S2Key, ids []common.ImageID, ranges []Range, dst []common.ImageID) []common.ImageID {
	mark := len(dst)
	for _, r := range ranges {
		i := sort.Search(len(keys), func(k int) bool { return keys[k] >= r.Start })
		for i < len(keys) && keys[i] <= r.End {
			dst = append(dst, ids[i])
			i++
		}
	}
	tail := dst[mark:]
	if !slices.IsSorted(tail) {
		slices.Sort(tail)
	}
	return dst
}

// TestPageDirLocateMatchesFlat builds a key array whose repeated keys span page
// boundaries and requires the paged locate/scan to return exactly the flat
// result for every tested range and page size. It guards the repeated-key case
// that a naive "last page whose first key <= key" lookup misses.
func TestPageDirLocateMatchesFlat(t *testing.T) {
	rng := rand.New(rand.NewSource(23))
	const runs = 50
	keys := make([]S2Key, 0, runs*8)
	ids := make([]common.ImageID, 0, runs*8)
	nextID := common.ImageID(1)
	runKeys := make([]S2Key, 0, runs)
	for r := 0; r < runs; r++ {
		key := S2Key(1 + uint64(r)*2) // odd keys with an absent even key between runs
		runKeys = append(runKeys, key)
		n := 1 + rng.Intn(9) // run length 1..9, not a multiple of the page sizes below
		for j := 0; j < n; j++ {
			keys = append(keys, key)
			ids = append(ids, nextID)
			nextID++
		}
	}
	if len(keys) == 0 {
		t.Fatal("no test data")
	}

	ranges := make([]Range, 0, runs*2+2)
	for _, key := range runKeys {
		ranges = append(ranges, Range{Start: key, End: key}) // exact run
	}
	for r := 0; r+1 < runs; r++ {
		// Start on an absent even key between two runs, end inside the next run.
		ranges = append(ranges, Range{Start: runKeys[r+1] - 1, End: runKeys[r+1]})
	}
	ranges = append(ranges,
		Range{Start: runKeys[0], End: runKeys[len(runKeys)-1]}, // spans every run
		Range{Start: 0, End: S2Key(^uint64(0))},                // covers everything
	)

	for _, pageRecords := range []int{1, 2, 3, 4, 5, 7, 8, 16, 31, 32, 64, 128, 256} {
		dir := buildPageDir(keys, pageRecords)
		for _, r := range ranges {
			want := flatSearch(keys, ids, []Range{r}, nil)
			got := pagedSearch(keys, ids, dir, []Range{r}, nil)
			if !slices.Equal(got, want) {
				t.Fatalf("pageRecords=%d range=[%d,%d]: paged=%v flat=%v", pageRecords, r.Start, r.End, got, want)
			}
		}
	}
}

// BenchmarkPagedLookup compares the flat sorted-array scan with a two-level page
// directory over the same data.
func BenchmarkPagedLookup(b *testing.B) {
	index := buildGeoBase(b, 1_000_000, geoBenchSpan)
	keys, ids := flattenView(index.view.Load())
	exactKey, _ := KeyFromLatLng(geoBenchLat, geoBenchLng)
	exact := []Range{{Start: exactKey, End: exactKey}}
	multi := CoverCap(nil, geoNewCap(2000), CoverOptions{MaxCells: 16})

	b.Run("flat/exact", func(b *testing.B) {
		dst := make([]common.ImageID, 0, 256)
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			dst = index.SearchRanges(dst[:0], exact)
		}
	})
	b.Run("flat/multi", func(b *testing.B) {
		dst := make([]common.ImageID, 0, 4096)
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			dst = index.SearchRanges(dst[:0], multi)
		}
	})
	b.Run("flat-locate/exact", func(b *testing.B) {
		dst := make([]common.ImageID, 0, 256)
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			dst = flatSearch(keys, ids, exact, dst[:0])
		}
	})
	b.Run("flat-locate/multi", func(b *testing.B) {
		dst := make([]common.ImageID, 0, 4096)
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			dst = flatSearch(keys, ids, multi, dst[:0])
		}
	})
	for _, pr := range []int{256, 512, 1024, 2048} {
		d := buildPageDir(keys, pr)
		b.Run(fmt.Sprintf("paged/size=%d/exact", pr), func(b *testing.B) {
			dst := make([]common.ImageID, 0, 256)
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				dst = pagedSearch(keys, ids, d, exact, dst[:0])
			}
		})
		b.Run(fmt.Sprintf("paged/size=%d/multi", pr), func(b *testing.B) {
			dst := make([]common.ImageID, 0, 4096)
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				dst = pagedSearch(keys, ids, d, multi, dst[:0])
			}
		})
	}
}

// TestPageWriteAmplification models page-level COW: a 64-mutation batch rewrites
// one page per distinct page touched. It compares that against the flat
// buffered compaction, which rewrites the whole base every base/8 mutations
// (~128 bytes per mutation at 16 B/image).
func TestPageWriteAmplification(t *testing.T) {
	for _, tc := range []struct {
		name   string
		spread float64
		mutate float64
	}{
		{"clustered", 0.01, 0.02},
		{"uniform", geoBenchSpan, geoBenchSpan},
	} {
		index := buildGeoBase(t, 1_000_000, tc.spread)
		keys, _ := flattenView(index.view.Load())
		const (
			batch   = 64
			batches = 200
		)
		for _, pr := range []int{256, 512, 1024, 2048} {
			dir := buildPageDir(keys, pr)
			rng := rand.New(rand.NewSource(42))
			totalPages := 0
			for b := 0; b < batches; b++ {
				seen := make(map[int]struct{}, batch)
				for m := 0; m < batch; m++ {
					lat := geoBenchLat + (rng.Float64()*2-1)*tc.mutate
					lng := geoBenchLng + (rng.Float64()*2-1)*tc.mutate
					key, err := KeyFromLatLng(lat, lng)
					if err != nil {
						t.Fatal(err)
					}
					seen[dir.locate(keys, key)/pr] = struct{}{}
				}
				totalPages += len(seen)
			}
			avgPages := float64(totalPages) / batches
			bytesPerMutation := avgPages * float64(pr*16) / batch
			t.Logf("%-9s pageRecords=%-4d avgPages/batch=%.1f bytes/mutation=%.0f", tc.name, pr, avgPages, bytesPerMutation)
		}
	}
	t.Logf("flat buffered compaction bytes/mutation ~= 128")
}

// BenchmarkKeyScan measures raw sequential S2Key scan cost; it is the ceiling a
// SIMD range kernel could approach. ns/key near memory bandwidth means SIMD has
// little headroom.
func BenchmarkKeyScan(b *testing.B) {
	index := buildGeoBase(b, 1_000_000, geoBenchSpan)
	keys, _ := flattenView(index.view.Load())
	var sink uint64
	b.SetBytes(int64(len(keys) * 8))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var s uint64
		for _, k := range keys {
			s += uint64(k)
		}
		sink = s
	}
	_ = sink
}

// BenchmarkRepeatedKeyCompaction measures one compaction merge when every base
// image shares a single S2Key and the delta mixes inserts, deletes, and moves.
// It targets the O(base * log delta) regression where each insertion rescanned
// the identical-key run; the streaming count/merge walks the run once.
func BenchmarkRepeatedKeyCompaction(b *testing.B) {
	for _, n := range []int{1_000, 10_000, 100_000} {
		b.Run(fmt.Sprintf("n=%d", n), func(b *testing.B) {
			keyA, err := KeyFromLatLng(testLat, testLng)
			if err != nil {
				b.Fatal(err)
			}
			keyB, err := KeyFromLatLng(testLat+0.01, testLng)
			if err != nil {
				b.Fatal(err)
			}
			baseKeys := make([]S2Key, n)
			baseIDs := make([]common.ImageID, n)
			for i := 0; i < n; i++ {
				baseKeys[i] = keyA
				baseIDs[i] = common.ImageID(i + 1)
			}
			frozen := make([]mutation, 0, 2*n)
			for i := 0; i < n; i++ {
				id := common.ImageID(i + 1)
				switch i % 3 {
				case 0: // delete
					frozen = append(frozen, mutation{id: id, key: keyA, add: false})
				case 1: // move to keyB
					frozen = append(frozen, mutation{id: id, key: keyB, add: true})
				}
			}
			for k := 0; k < n; k++ { // insert new images at keyA
				frozen = append(frozen, mutation{id: common.ImageID(n + 1 + k), key: keyA, add: true})
			}
			slices.SortFunc(frozen, compareMutationID)

			cfg, err := Config{}.withDefaults()
			if err != nil {
				b.Fatal(err)
			}
			index := &Index{config: cfg}
			one := &shard{keys: baseKeys, ids: baseIDs, maxID: common.ImageID(n)}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := index.prepareShards([]*shard{one}, frozen); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
