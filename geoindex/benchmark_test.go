package geoindex

import (
	"testing"
	"time"

	"github.com/golang/geo/s1"
	"github.com/golang/geo/s2"
)

func benchmarkIndex(b *testing.B, count int) *Index {
	entries := make([]Entry, count)
	for index := range entries {
		entries[index] = Entry{
			ImageID:   uint64(index + 1),
			Latitude:  37.0 + float64(index%1000)/10_000,
			Longitude: -122.5 + float64(index/1000)/10_000,
		}
	}
	result, err := New(Spec{IndexID: 1, Entries: entries}, Config{PublishThreshold: 2_048, PublishInterval: time.Hour})
	if err != nil {
		b.Fatal(err)
	}
	return result
}

func BenchmarkSingleCell(b *testing.B) {
	index := benchmarkIndex(b, 100_000)
	defer index.Close()
	key := keyAt(b, 37.01, -122.49)
	ranges := []Range{{Start: key, End: key}}
	results := make([]ImageID, 0, 64)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		_ = index.SearchRanges(results[:0], ranges)
	}
}

func BenchmarkCoveredRegion(b *testing.B) {
	index := benchmarkIndex(b, 100_000)
	defer index.Close()
	region := s2.RectFromLatLng(s2.LatLngFromDegrees(37.01, -122.49)).AddPoint(
		s2.LatLngFromDegrees(37.05, -122.45),
	)
	ranges, err := index.CoverRegion(nil, region, CoverOptions{})
	if err != nil {
		b.Fatal(err)
	}
	results := make([]ImageID, 0, 10_000)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		_ = index.SearchRanges(results[:0], ranges)
	}
}

func BenchmarkCoverRegionReusable(b *testing.B) {
	index, err := New(Spec{IndexID: 1}, testConfig())
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = index.Close() })
	center := s2.PointFromLatLng(s2.LatLngFromDegrees(37.03, -122.47))
	var region s2.Region = s2.CapFromCenterAngle(center, s1.Angle(0.002))
	workspace := NewCoverWorkspace(64)
	ranges := make([]Range, 0, 64)
	options := CoverOptions{MaxCells: 32, Workspace: workspace}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		_, _ = index.CoverRegion(ranges[:0], region, options)
	}
}

func BenchmarkBatchIngest1000(b *testing.B) {
	mutations := make([]Mutation, 1_000)
	for index := range mutations {
		key, err := KeyFromLatLng(40, -74+float64(index)/1e6)
		if err != nil {
			b.Fatal(err)
		}
		mutations[index] = Mutation{ImageID: uint64(index + 1), To: key}
	}
	b.ReportAllocs()
	for range b.N {
		index, _ := New(Spec{IndexID: 1}, Config{PublishThreshold: 1_000, PublishInterval: time.Hour})
		_ = index.Apply(mutations)
		_ = index.Close()
	}
}
