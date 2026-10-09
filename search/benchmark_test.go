package search

import (
	"context"
	"math/rand"
	"testing"

	common "github.com/evanoberholster/imageIdx/common"
	"github.com/evanoberholster/imageIdx/index/bitmapidx"
	"github.com/evanoberholster/imageIdx/index/geoidx"
	"github.com/evanoberholster/imageIdx/index/timeidx"
	"github.com/golang/geo/s2"
)

func benchmarkEnumNamespace(tb testing.TB, typ common.Namespace, value string, ids []uint64) *bitmapidx.Index {
	tb.Helper()
	index, err := bitmapidx.NewMemory(bitmapidx.NamespaceSpec{Namespace: typ, Kind: common.KindBitmapEnum}, bitmapidx.NamespaceConfig{})
	if err != nil {
		tb.Fatal(err)
	}
	entries := make([]bitmapidx.Entry, len(ids))
	for n, id := range ids {
		entries[n] = bitmapidx.Entry{ImageID: id, Value: bitmapidx.EnumStringValue(value)}
	}
	commitBitmap(tb, index, func(b *bitmapidx.Batch) error { return b.Add(entries...) })
	tb.Cleanup(func() { _ = index.Close() })
	return index
}

// commitBitmap stages fill into a one-shot bitmapidx batch, applies it,
// and barriers until visible: the test equivalent of the old synchronous
// write.
func commitBitmap(tb testing.TB, index *bitmapidx.Index, fill func(*bitmapidx.Batch) error) {
	tb.Helper()
	batch := index.NewBatch()
	defer batch.Close()
	if err := fill(batch); err != nil {
		tb.Fatal(err)
	}
	if err := batch.Apply(); err != nil {
		tb.Fatal(err)
	}
	if err := index.Barrier(); err != nil {
		tb.Fatal(err)
	}
}

func BenchmarkCombinedSearch(b *testing.B) {
	const count = 10_000
	quantum := uint64(1) << timeidx.TimestampShift
	namespaceIDs := make([]uint64, 0, count/2)
	records := make([]timeidx.Entry, count)
	geoEntries := make([]geoEntry, count)
	key, err := geoidx.KeyFromLatLng(37, -122)
	if err != nil {
		b.Fatal(err)
	}
	for index := range count {
		id := uint64(index + 1)
		if id%2 == 0 {
			namespaceIDs = append(namespaceIDs, id)
		}
		records[index] = timeidx.Entry{Timestamp: id * quantum, ImageID: common.ImageID(id)}
		geoEntries[index] = geoEntry{id: id, lat: 37, lng: -122}
	}
	namespaceIndex := benchmarkEnumNamespace(b, common.NamespaceTag, "favorite", namespaceIDs)
	timeIndex, err := timeidx.NewMemory(timeidx.Spec{Entries: records}, timeidx.Config{})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = timeIndex.Close() })
	geo := newTestGeoNamespace(b, common.Namespace(30), geoEntries)
	plan, err := Compile(
		AllValues(namespaceIndex, bitmapidx.EnumStringValue("favorite")),
		During(timeIndex, 4_000*quantum, 6_000*quantum),
		InCells(geo, geoidx.DecodeKey(key)),
	)
	if err != nil {
		b.Fatal(err)
	}
	workspace := NewWorkspace(10_000)
	results := make([]ImageID, 0, 10_000)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		results, err = plan.AppendIDs(context.Background(), results[:0], workspace)
		if err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkGeoCollectSearch exercises the Search-only MutableBitmap path.
func BenchmarkGeoCollectSearch(b *testing.B) {
	const count = 100_000
	namespaceIDs := make([]uint64, 0, count/2)
	geoEntries := make([]geoEntry, 0, count)
	for id := uint64(1); id <= count; id++ {
		if id%2 == 0 {
			namespaceIDs = append(namespaceIDs, id)
		}
		geoEntries = append(geoEntries, geoEntry{
			id: id, lat: 30 + float64(id%200)*0.05, lng: -120 + float64(id/200)*0.05,
		})
	}
	namespaceIndex := benchmarkEnumNamespace(b, common.NamespaceTag, "favorite", namespaceIDs)
	geo := newTestGeoNamespace(b, common.Namespace(31), geoEntries)
	cover := uint64(s2.CellIDFromLatLng(s2.LatLngFromDegrees(30, -120)).Parent(0))
	plan, err := Compile(AllValues(namespaceIndex, bitmapidx.EnumStringValue("favorite")), InCells(geo, cover))
	if err != nil {
		b.Fatal(err)
	}
	workspace := NewWorkspace(count)
	results := make([]ImageID, 0, count)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		results, err = plan.AppendIDs(context.Background(), results[:0], workspace)
		if err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkGeoCityPlan(b *testing.B) {
	cities := []struct {
		name     string
		lat, lng float64
	}{
		{"Paris", 48.8566, 2.3522}, {"London", 51.5074, -0.1278},
		{"New York", 40.7128, -74.0060}, {"Tokyo", 35.6762, 139.6503},
	}
	const perCity = 5_000
	rng := rand.New(rand.NewSource(7))
	total := perCity * len(cities)
	geoEntries := make([]geoEntry, 0, total)
	namespaceIDs := make([]uint64, 0, total/2)
	for id := uint64(1); id <= uint64(total); id++ {
		city := cities[(int(id)-1)/perCity]
		geoEntries = append(geoEntries, geoEntry{
			id: id, lat: city.lat + (rng.Float64()-0.5)*0.3, lng: city.lng + (rng.Float64()-0.5)*0.3,
		})
		if id%2 == 0 {
			namespaceIDs = append(namespaceIDs, id)
		}
	}
	namespaceIndex := benchmarkEnumNamespace(b, common.NamespaceTag, "favorite", namespaceIDs)
	geo := newTestGeoNamespace(b, common.Namespace(32), geoEntries)
	for _, city := range cities {
		cityCell := uint64(s2.CellIDFromLatLng(s2.LatLngFromDegrees(city.lat, city.lng)).Parent(8))
		plan, err := Compile(AllValues(namespaceIndex, bitmapidx.EnumStringValue("favorite")), InCells(geo, cityCell))
		if err != nil {
			b.Fatal(err)
		}
		workspace := NewWorkspace(perCity)
		results := make([]ImageID, 0, perCity)
		if got, err := plan.Count(context.Background(), workspace); err != nil || got == 0 {
			b.Fatalf("%s plan matched nothing", city.name)
		}
		b.Run(city.name, func(b *testing.B) {
			b.ReportAllocs()
			for range b.N {
				results, err = plan.AppendIDs(context.Background(), results[:0], workspace)
				if err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
