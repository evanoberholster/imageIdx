package search

import (
	"math/rand"
	"testing"

	"github.com/golang/geo/s2"

	imageidx "github.com/evanoberholster/imageIdx"
	"github.com/evanoberholster/imageIdx/bitmapidx"

	"github.com/evanoberholster/imageIdx/timeidx"
)

func BenchmarkCombinedSearch(b *testing.B) {
	const count = 10_000
	quantum := uint64(1) << timeidx.TimestampShift
	bitmapIDs := make([]uint64, 0, count/2)
	records := make([]timeidx.Entry, count)
	geoEntries := make([]bitmapidx.GeoEntry, count)
	key, err := bitmapidx.KeyFromLatLng(37, -122)
	if err != nil {
		b.Fatal(err)
	}
	for index := range count {
		id := uint64(index + 1)
		if id%2 == 0 {
			bitmapIDs = append(bitmapIDs, id)
		}
		records[index] = timeidx.Entry{Timestamp: id * quantum, ImageID: imageidx.ImageID(id)}
		geoEntries[index] = bitmapidx.GeoEntry{ImageID: id, Latitude: 37, Longitude: -122}
	}
	bitmap, err := bitmapidx.New(bitmapidx.Spec{
		Key: bitmapidx.Key{Type: bitmapidx.IndexTypeTag, ValueID: 1}, IndexID: 1, IDs: bitmapIDs,
	}, bitmapidx.Config{})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = bitmap.Close() })
	timeIndex, err := timeidx.New(timeidx.Spec{IndexID: 3, Entries: records}, timeidx.Config{})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = timeIndex.Close() })
	geo, err := bitmapidx.NewGeoField(bitmapidx.IndexType(2), geoEntries, bitmapidx.FieldConfig{})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = geo.Close() })
	geoClause, err := WithinCells(geo, bitmapidx.DecodeS2Key(key))
	if err != nil {
		b.Fatal(err)
	}

	plan, err := Compile(
		AllBitmaps(bitmap),
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

// BenchmarkGeoCollectSearch forces the geo stage down the collect+intersect
// path: 50k bitmap candidates against ~100k covered cells far exceed the
// verify-vs-collect probe bound, so ranges accumulate into the
// workspace-owned MutableBitmap. Steady state must allocate nothing.
func BenchmarkGeoCollectSearch(b *testing.B) {
	const count = 100_000
	bitmapIDs := make([]uint64, 0, count/2)
	geoEntries := make([]bitmapidx.GeoEntry, 0, count)
	for id := uint64(1); id <= count; id++ {
		if id%2 == 0 {
			bitmapIDs = append(bitmapIDs, id)
		}
		geoEntries = append(geoEntries, bitmapidx.GeoEntry{
			ImageID:   id,
			Latitude:  30.0 + float64(id%200)*0.05,
			Longitude: -120.0 + float64(id/200)*0.05,
		})
	}
	bitmap, err := bitmapidx.New(bitmapidx.Spec{
		Key: bitmapidx.Key{Type: bitmapidx.IndexTypeTag, ValueID: 1}, IndexID: 1, IDs: bitmapIDs,
	}, bitmapidx.Config{})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = bitmap.Close() })
	geo, err := bitmapidx.NewGeoField(bitmapidx.IndexType(2), geoEntries, bitmapidx.FieldConfig{})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = geo.Close() })
	cover := uint64(s2.CellIDFromLatLng(s2.LatLngFromDegrees(30, -120)).Parent(0))
	geoClause, err := WithinCells(geo, cover)
	if err != nil {
		b.Fatal(err)
	}

	plan, err := Compile(AllBitmaps(bitmap), geoClause)
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

// BenchmarkGeoCityPlan measures an end-to-end "photos in Paris" search: a
// bitmap predicate intersected with a city geo clause through a reusable plan
// and workspace. Steady state must allocate nothing.
func BenchmarkGeoCityPlan(b *testing.B) {
	cities := []struct {
		name     string
		lat, lng float64
	}{
		{"Paris", 48.8566, 2.3522},
		{"London", 51.5074, -0.1278},
		{"New York", 40.7128, -74.0060},
		{"Tokyo", 35.6762, 139.6503},
	}
	const perCity = 5_000
	rng := rand.New(rand.NewSource(7))
	total := perCity * len(cities)
	geoEntries := make([]bitmapidx.GeoEntry, 0, total)
	bitmapIDs := make([]uint64, 0, total/2)
	for id := uint64(1); id <= uint64(total); id++ {
		city := cities[(int(id)-1)/perCity]
		latitude := city.lat + (rng.Float64()-0.5)*0.3
		longitude := city.lng + (rng.Float64()-0.5)*0.3
		geoEntries = append(geoEntries, bitmapidx.GeoEntry{ImageID: id, Latitude: latitude, Longitude: longitude})
		if id%2 == 0 {
			bitmapIDs = append(bitmapIDs, id)
		}
	}
	bitmap, err := bitmapidx.New(bitmapidx.Spec{
		Key: bitmapidx.Key{Type: bitmapidx.IndexTypeTag, ValueID: 1}, IndexID: 1, IDs: bitmapIDs,
	}, bitmapidx.Config{})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = bitmap.Close() })
	geo, err := bitmapidx.NewGeoField(bitmapidx.IndexType(2), geoEntries, bitmapidx.FieldConfig{})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = geo.Close() })

	for _, city := range cities {
		cityCell := uint64(s2.CellID(bitmapidx.CellIDFromLatLng(city.lat, city.lng)).Parent(8))
		geoClause, err := WithinCells(geo, cityCell)
		if err != nil {
			b.Fatal(err)
		}
		plan, err := Compile(AllBitmaps(bitmap), geoClause)
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
		_ = results
	}
}
