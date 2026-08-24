package search

import (
	"testing"

	imageidx "github.com/evanoberholster/imageIdx"
	"github.com/evanoberholster/imageIdx/bitmapidx"
	"github.com/evanoberholster/imageIdx/geoindex"
	"github.com/evanoberholster/imageIdx/timestamp"
)

func BenchmarkCombinedSearch(b *testing.B) {
	const count = 10_000
	quantum := uint64(1) << timestamp.TimestampShift
	bitmapIDs := make([]uint64, 0, count/2)
	records := make([]timestamp.Entry, count)
	geoEntries := make([]geoindex.Entry, count)
	key, err := geoindex.KeyFromLatLng(37, -122)
	if err != nil {
		b.Fatal(err)
	}
	for index := range count {
		id := uint64(index + 1)
		if id%2 == 0 {
			bitmapIDs = append(bitmapIDs, id)
		}
		records[index] = timestamp.Entry{Timestamp: id * quantum, ImageID: imageidx.ImageID(id)}
		geoEntries[index] = geoindex.Entry{ImageID: id, Latitude: 37, Longitude: -122}
	}
	bitmap, err := bitmapidx.New(bitmapidx.Spec{
		Key: bitmapidx.Key{Type: bitmapidx.IndexTypeTag, ValueID: 1}, IndexID: 1, IDs: bitmapIDs,
	}, bitmapidx.Config{})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = bitmap.Close() })
	timeIndex, err := timestamp.New(timestamp.Spec{IndexID: 3, Entries: records}, timestamp.Config{})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = timeIndex.Close() })
	geo, err := geoindex.New(geoindex.Spec{IndexID: 2, Entries: geoEntries}, geoindex.Config{})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = geo.Close() })
	geoClause, err := WithinCells(geo, geoindex.DecodeS2Key(key))
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
