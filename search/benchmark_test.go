package search

import (
	"math/rand"
	"testing"

	imageidx "github.com/evanoberholster/imageIdx"
	"github.com/evanoberholster/imageIdx/index/bitmapidx"
	"github.com/evanoberholster/imageIdx/index/timeidx"
	"github.com/golang/geo/s2"
)

func benchmarkEnumField(tb testing.TB, typ bitmapidx.IndexType, value string, ids []uint64) *bitmapidx.FieldIndex {
	tb.Helper()
	index, err := bitmapidx.NewFieldIndex(bitmapidx.Field{Type: typ, Kind: bitmapidx.KindEnum, Storage: bitmapidx.StoragePerType}, bitmapidx.FieldConfig{})
	if err != nil {
		tb.Fatal(err)
	}
	entries := make([]bitmapidx.Entry, len(ids))
	for n, id := range ids {
		entries[n] = bitmapidx.Entry{ImageID: id, Value: bitmapidx.StringValue(value)}
	}
	if err := index.Add(entries...); err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { _ = index.Close() })
	return index
}

func BenchmarkCombinedSearch(b *testing.B) {
	const count = 10_000
	quantum := uint64(1) << timeidx.TimestampShift
	fieldIDs := make([]uint64, 0, count/2)
	records := make([]timeidx.Entry, count)
	geoEntries := make([]bitmapidx.GeoEntry, count)
	key, err := bitmapidx.KeyFromLatLng(37, -122)
	if err != nil {
		b.Fatal(err)
	}
	for index := range count {
		id := uint64(index + 1)
		if id%2 == 0 {
			fieldIDs = append(fieldIDs, id)
		}
		records[index] = timeidx.Entry{Timestamp: id * quantum, ImageID: imageidx.ImageID(id)}
		geoEntries[index] = bitmapidx.GeoEntry{ImageID: id, Latitude: 37, Longitude: -122}
	}
	field := benchmarkEnumField(b, bitmapidx.IndexTypeTag, "favorite", fieldIDs)
	timeIndex, err := timeidx.New(timeidx.Spec{Entries: records}, timeidx.Config{})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = timeIndex.Close() })
	geo, err := bitmapidx.NewGeoField(bitmapidx.IndexType(30), geoEntries, bitmapidx.FieldConfig{})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = geo.Close() })
	geoClause, err := WithinCells(geo, bitmapidx.DecodeS2Key(key))
	if err != nil {
		b.Fatal(err)
	}
	plan, err := Compile(
		FieldValues(field, bitmapidx.StringValue("favorite")),
		During(timeIndex, 4_000*quantum, 6_000*quantum),
		geoClause,
	)
	if err != nil {
		b.Fatal(err)
	}
	workspace := NewWorkspace(10_000)
	results := make([]ImageID, 0, 10_000)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		results = plan.Search(results[:0], workspace)
	}
}

// BenchmarkGeoCollectSearch exercises the Search-only MutableBitmap path.
func BenchmarkGeoCollectSearch(b *testing.B) {
	const count = 100_000
	fieldIDs := make([]uint64, 0, count/2)
	geoEntries := make([]bitmapidx.GeoEntry, 0, count)
	for id := uint64(1); id <= count; id++ {
		if id%2 == 0 {
			fieldIDs = append(fieldIDs, id)
		}
		geoEntries = append(geoEntries, bitmapidx.GeoEntry{
			ImageID: id, Latitude: 30 + float64(id%200)*0.05, Longitude: -120 + float64(id/200)*0.05,
		})
	}
	field := benchmarkEnumField(b, bitmapidx.IndexTypeTag, "favorite", fieldIDs)
	geo, err := bitmapidx.NewGeoField(bitmapidx.IndexType(31), geoEntries, bitmapidx.FieldConfig{})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = geo.Close() })
	cover := uint64(s2.CellIDFromLatLng(s2.LatLngFromDegrees(30, -120)).Parent(0))
	geoClause, err := WithinCells(geo, cover)
	if err != nil {
		b.Fatal(err)
	}
	plan, err := Compile(FieldValues(field, bitmapidx.StringValue("favorite")), geoClause)
	if err != nil {
		b.Fatal(err)
	}
	workspace := NewWorkspace(count)
	results := make([]ImageID, 0, count)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		results = plan.Search(results[:0], workspace)
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
	geoEntries := make([]bitmapidx.GeoEntry, 0, total)
	fieldIDs := make([]uint64, 0, total/2)
	for id := uint64(1); id <= uint64(total); id++ {
		city := cities[(int(id)-1)/perCity]
		geoEntries = append(geoEntries, bitmapidx.GeoEntry{
			ImageID: id, Latitude: city.lat + (rng.Float64()-0.5)*0.3, Longitude: city.lng + (rng.Float64()-0.5)*0.3,
		})
		if id%2 == 0 {
			fieldIDs = append(fieldIDs, id)
		}
	}
	field := benchmarkEnumField(b, bitmapidx.IndexTypeTag, "favorite", fieldIDs)
	geo, err := bitmapidx.NewGeoField(bitmapidx.IndexType(32), geoEntries, bitmapidx.FieldConfig{})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = geo.Close() })
	for _, city := range cities {
		cityCell := uint64(s2.CellIDFromLatLng(s2.LatLngFromDegrees(city.lat, city.lng)).Parent(8))
		geoClause, err := WithinCells(geo, cityCell)
		if err != nil {
			b.Fatal(err)
		}
		plan, err := Compile(FieldValues(field, bitmapidx.StringValue("favorite")), geoClause)
		if err != nil {
			b.Fatal(err)
		}
		workspace := NewWorkspace(perCity)
		results := make([]ImageID, 0, perCity)
		if got := plan.Count(workspace); got == 0 {
			b.Fatalf("%s plan matched nothing", city.name)
		}
		b.Run(city.name, func(b *testing.B) {
			b.ReportAllocs()
			for range b.N {
				results = plan.Search(results[:0], workspace)
			}
		})
	}
}
