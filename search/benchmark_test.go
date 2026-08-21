package search

import (
	"testing"

	"github.com/evanoberholster/imageIdx/bitmapidx"
	"github.com/evanoberholster/imageIdx/geoindex"
	"github.com/evanoberholster/imageIdx/timestamp"
)

func BenchmarkCombinedSearch(b *testing.B) {
	const count = 10_000
	quantum := uint64(1) << timestamp.TimestampShift
	epoch := bitmapidx.NewEpoch()
	bitmapIDs := make([]uint64, 0, count/2)
	records := make([]timestamp.Record, count)
	geoEntries := make([]geoindex.GeoCellEntry, count)
	key, err := geoindex.KeyFromLatLng(37, -122)
	if err != nil {
		b.Fatal(err)
	}
	for index := range count {
		id := uint64(index + 1)
		if id%2 == 0 {
			bitmapIDs = append(bitmapIDs, id)
		}
		records[index] = timestamp.Record{Timestamp: id * quantum, ImageID: timestamp.ImageID(id)}
		geoEntries[index] = geoindex.GeoCellEntry{Key: key, ImageID: id}
	}
	bitmap, err := bitmapidx.New(bitmapidx.BitmapKey{Type: bitmapidx.IndexTypeTag, ValueID: 1}, 1, bitmapIDs, bitmapidx.Config{Epoch: epoch})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = bitmap.Close() })
	timeIndex, err := timestamp.New(records, 0, timestamp.Config{})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = timeIndex.Close() })
	geo, err := geoindex.NewFromCells(2, geoEntries, geoindex.Config{Epoch: epoch})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = geo.Close() })

	plan, err := (Scheduler{}).Plan(Query{
		Bitmaps: []*bitmapidx.BitmapIndex{bitmap},
		TimestampRanges: []TimestampRange{{
			Index: timeIndex,
			Start: 4_000 * quantum,
			End:   6_000 * quantum,
		}},
		GeoRanges: []GeoRange{{Index: geo, Ranges: []geoindex.S2KeyRange{{Start: key, End: key}}}},
	})
	if err != nil {
		b.Fatal(err)
	}
	workspace := NewWorkspace(10_000)
	results := make([]ImageID, 0, 10_000)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		results = plan.Execute(results[:0], workspace)
	}
}
