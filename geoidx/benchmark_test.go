package geoidx

import (
	"fmt"
	"slices"
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

// BenchmarkFlushSmallBatch measures the steady-state cost of publishing a
// small move batch into a populated index: ten images ping-pong between two
// dense cells, exercising both removal and addition paths per Flush.
func BenchmarkFlushSmallBatch(b *testing.B) {
	index := benchmarkIndex(b, 100_000)
	defer index.Close()
	keyA := keyAt(b, 37.01, -122.49)
	keyB := keyAt(b, 37.02, -122.48)
	const batch = 10
	seed := make([]Mutation, batch)
	for i := range seed {
		seed[i] = Mutation{ImageID: uint64(1_000_001 + i), To: keyA}
	}
	if err := index.Apply(seed); err != nil {
		b.Fatal(err)
	}
	if err := index.Flush(); err != nil {
		b.Fatal(err)
	}
	forward := make([]Mutation, batch)
	backward := make([]Mutation, batch)
	for i := range forward {
		id := uint64(1_000_001 + i)
		forward[i] = Mutation{ImageID: id, From: keyA, To: keyB}
		backward[i] = Mutation{ImageID: id, From: keyB, To: keyA}
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		mutations := forward
		if i%2 == 1 {
			mutations = backward
		}
		if err := index.Apply(mutations); err != nil {
			b.Fatal(err)
		}
		if err := index.Flush(); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkFilterVsCollect compares verifying candidates against covered
// cells (FilterRanges) with materializing the covered population and
// intersecting. It guards the probe-bound tuning of the search planner's geo
// filter stage: few candidates favor verification, large candidate sets favor
// collect+intersect.
func BenchmarkFilterVsCollect(b *testing.B) {
	benchFilterVsCollect(b, "narrow", s2.RectFromLatLng(s2.LatLngFromDegrees(37.01, -122.49)).AddPoint(
		s2.LatLngFromDegrees(37.05, -122.45),
	))
	benchFilterVsCollect(b, "wide", s2.RectFromLatLng(s2.LatLngFromDegrees(37.0, -122.5)).AddPoint(
		s2.LatLngFromDegrees(37.0999, -122.4),
	))
}

func benchFilterVsCollect(b *testing.B, name string, region s2.Region) {
	index := benchmarkIndex(b, 100_000)
	defer index.Close()
	ranges, err := index.CoverRegion(nil, region, CoverOptions{})
	if err != nil {
		b.Fatal(err)
	}
	geo := index.SearchRanges(nil, ranges)
	collectBuf := make([]ImageID, 0, len(geo))
	for _, n := range []int{16, 128, 1024, 8192} {
		candidates := make([]ImageID, 0, n)
		for i := range n {
			candidates = append(candidates, ImageID(i*(100_000/n)+1))
		}
		b.Run(fmt.Sprintf("%s/Collect_n=%d", name, n), func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				buf := index.SearchRanges(collectBuf[:0], ranges)
				slices.Sort(buf)
				buf = slices.Compact(buf)
				buf = intersectSortedForTest(buf, candidates)
				_ = buf
			}
		})
		b.Run(fmt.Sprintf("%s/Filter_n=%d", name, n), func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				buf := index.FilterRanges(collectBuf[:0], candidates, ranges)
				slices.Sort(buf)
				_ = buf
			}
		})
	}
}
