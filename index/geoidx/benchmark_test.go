package geoidx

import (
	"fmt"
	"math/rand"
	"testing"

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
