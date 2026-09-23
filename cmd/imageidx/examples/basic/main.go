// Command basic runs one combined bitmap + timestamp + geo query.
//
// It mirrors cmd/imageidx runOnce: seed three small indexes, compile a plan
// once, and execute it with a reusable workspace and result buffer.
//
// Usage:
//
//	go run ./cmd/imageidx/examples/basic
package main

import (
	"context"
	"fmt"
	"log"
	"os"

	imageidx "github.com/evanoberholster/imageIdx"
	"github.com/evanoberholster/imageIdx/index/bitmapidx"
	"github.com/evanoberholster/imageIdx/index/timeidx"
	"github.com/evanoberholster/imageIdx/search"
)

const quantum = uint64(1) << timeidx.TimestampShift

func main() {
	fieldDir, err := os.MkdirTemp("", "imageidx-fields-")
	if err != nil {
		log.Fatal(err)
	}
	fields, err := bitmapidx.NewIndex(fieldDir, []bitmapidx.Field{
		{Type: bitmapidx.IndexTypeTag, Kind: bitmapidx.KindEnum, Storage: bitmapidx.StoragePerType},
		{Type: bitmapidx.IndexTypeLocation, Kind: bitmapidx.KindGeo, Storage: bitmapidx.StoragePerType, Geo: bitmapidx.GeoCodec{Version: 1}},
	}, bitmapidx.FieldConfig{}, bitmapidx.DurabilityOS)
	if err != nil {
		log.Fatal(err)
	}
	defer func() { _ = fields.Close(); _ = os.RemoveAll(fieldDir) }()
	favorites, ok := fields.Lookup(bitmapidx.IndexTypeTag)
	if !ok {
		log.Fatal("tag field missing")
	}
	if err := favorites.Add(
		bitmapidx.Entry{ImageID: 100, Value: bitmapidx.StringValue("favorite")},
		bitmapidx.Entry{ImageID: 200, Value: bitmapidx.StringValue("favorite")},
		bitmapidx.Entry{ImageID: 300, Value: bitmapidx.StringValue("favorite")},
	); err != nil {
		log.Fatal(err)
	}
	geo, ok := fields.Lookup(bitmapidx.IndexTypeLocation)
	if !ok {
		log.Fatal("location field missing")
	}
	for _, entry := range []struct {
		id  imageidx.ImageID
		lat float64
		lng float64
	}{
		{100, 37.7749, -122.4194}, {200, 37.7749, -122.4194},
		{300, 34.0522, -118.2437}, {400, 40.7128, -74.0060},
	} {
		key, err := bitmapidx.KeyFromLatLng(entry.lat, entry.lng)
		if err != nil {
			log.Fatal(err)
		}
		if err := geo.Move(bitmapidx.Move{ImageID: entry.id, To: int64(key)}); err != nil {
			log.Fatal(err)
		}
	}

	canonical := map[imageidx.ImageID]uint64{
		100: quantum,
		200: 2 * quantum,
		300: 3 * quantum,
		400: 4 * quantum,
	}
	times, err := timeidx.New(timeidx.Spec{
		IndexID: 2,
		Entries: []timeidx.Entry{
			{ImageID: 100, Timestamp: canonical[100]},
			{ImageID: 200, Timestamp: canonical[200]},
			{ImageID: 300, Timestamp: canonical[300]},
			{ImageID: 400, Timestamp: canonical[400]},
		},
	}, timeidx.Config{})
	if err != nil {
		log.Fatal(err)
	}
	defer times.Close()

	plan, err := search.Compile(
		search.AllValues(favorites, bitmapidx.StringValue("favorite")),
		search.During(times, quantum, 3*quantum,
			search.ExactTimestamp(func(id imageidx.ImageID) (uint64, bool) {
				value, ok := canonical[id]
				return value, ok
			})),
		search.InCells(geo, bitmapidx.CellIDFromLatLng(37.7749, -122.4194)),
	)
	if err != nil {
		log.Fatal(err)
	}

	workspace := search.NewWorkspace(64)
	matches := make([]search.ImageID, 0, 16)
	matches, err = plan.AppendIDs(context.Background(), matches, workspace)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("matches=%v stages=%v estimate=%d\n", matches, plan.Stages(), plan.Estimate())
}
