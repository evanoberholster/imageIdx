package geoidx

import (
	"fmt"
	"math/rand"
	"testing"

	"github.com/evanoberholster/imageIdx/roar"
	"github.com/golang/geo/s2"
)

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
