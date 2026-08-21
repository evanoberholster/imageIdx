package geoindex

import (
	"testing"
	"time"
)

func benchmarkIndex(b *testing.B, count int) *GeoIndex {
	entries := make([]GeoEntry, count)
	for index := range entries {
		entries[index] = GeoEntry{
			ImageID:   uint64(index + 1),
			Latitude:  37.0 + float64(index%1000)/10_000,
			Longitude: -122.5 + float64(index/1000)/10_000,
		}
	}
	result, err := New(1, entries, Config{PublishThreshold: 2_048, PublishInterval: time.Hour})
	if err != nil {
		b.Fatal(err)
	}
	return result
}

func BenchmarkSingleCell(b *testing.B) {
	index := benchmarkIndex(b, 100_000)
	defer index.Close()
	cell := DecodeS2Key(keyAt(b, 37.01, -122.49))
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		results := make([]ImageID, 0, 64)
		_ = index.SearchCell(results, cell)
	}
}

func BenchmarkBoundingBox(b *testing.B) {
	index := benchmarkIndex(b, 100_000)
	defer index.Close()
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		results := make([]ImageID, 0, 10_000)
		_ = index.SearchBounds(results, 37.01, -122.49, 37.05, -122.45, nil)
	}
}

func BenchmarkBatchIngest1000(b *testing.B) {
	entries := make([]GeoEntry, 1_000)
	for index := range entries {
		entries[index] = GeoEntry{ImageID: uint64(index + 1), Latitude: 40, Longitude: -74 + float64(index)/1e6}
	}
	b.ReportAllocs()
	for range b.N {
		index, _ := New(1, nil, Config{PublishThreshold: 1_000, PublishInterval: time.Hour})
		_ = index.AddBatch(entries)
		_ = index.Close()
	}
}
