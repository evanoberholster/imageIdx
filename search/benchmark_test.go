package search

import (
	"testing"

	"github.com/golang/geo/s2"

	imageidx "github.com/evanoberholster/imageIdx"
	"github.com/evanoberholster/imageIdx/bitmapidx"
	"github.com/evanoberholster/imageIdx/geoidx"
	"github.com/evanoberholster/imageIdx/timeidx"
)

func BenchmarkCombinedSearch(b *testing.B) {
	const count = 10_000
	quantum := uint64(1) << timeidx.TimestampShift
	bitmapIDs := make([]uint64, 0, count/2)
	records := make([]timeidx.Entry, count)
	geoEntries := make([]geoidx.Entry, count)
	key, err := geoidx.KeyFromLatLng(37, -122)
	if err != nil {
		b.Fatal(err)
	}
	for index := range count {
		id := uint64(index + 1)
		if id%2 == 0 {
			bitmapIDs = append(bitmapIDs, id)
		}
		records[index] = timeidx.Entry{Timestamp: id * quantum, ImageID: imageidx.ImageID(id)}
		geoEntries[index] = geoidx.Entry{ImageID: id, Latitude: 37, Longitude: -122}
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
	geo, err := geoidx.New(geoidx.Spec{IndexID: 2, Entries: geoEntries}, geoidx.Config{})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = geo.Close() })
	geoClause, err := WithinCells(geo, geoidx.DecodeS2Key(key))
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
	geoEntries := make([]geoidx.Entry, 0, count)
	for id := uint64(1); id <= count; id++ {
		if id%2 == 0 {
			bitmapIDs = append(bitmapIDs, id)
		}
		geoEntries = append(geoEntries, geoidx.Entry{
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
	geo, err := geoidx.New(geoidx.Spec{IndexID: 2, Entries: geoEntries}, geoidx.Config{})
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
