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
	"fmt"
	"log"

	imageidx "github.com/evanoberholster/imageIdx"
	"github.com/evanoberholster/imageIdx/bitmapidx"
	"github.com/evanoberholster/imageIdx/search"
	"github.com/evanoberholster/imageIdx/timeidx"
)

const quantum = uint64(1) << timeidx.TimestampShift

func main() {
	favorites, err := bitmapidx.New(bitmapidx.Spec{
		Key:     bitmapidx.Key{Type: bitmapidx.IndexTypeTag, ValueID: 1},
		IndexID: 1,
		IDs:     []imageidx.ImageID{100, 200, 300},
	}, bitmapidx.Config{})
	if err != nil {
		log.Fatal(err)
	}
	defer favorites.Close()

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

	geo, err := bitmapidx.NewGeoField(bitmapidx.IndexTypeLocation, []bitmapidx.GeoEntry{
		{ImageID: 100, Latitude: 37.7749, Longitude: -122.4194},
		{ImageID: 200, Latitude: 37.7749, Longitude: -122.4194},
		{ImageID: 300, Latitude: 34.0522, Longitude: -118.2437},
		{ImageID: 400, Latitude: 40.7128, Longitude: -74.0060},
	}, bitmapidx.FieldConfig{})
	if err != nil {
		log.Fatal(err)
	}
	defer geo.Close()

	// WithinCells is the only clause constructor that returns an error:
	// unknown cells, an empty set, or a non-geo field are rejected here.
	geoClause, err := search.WithinCells(geo, bitmapidx.CellIDFromLatLng(37.7749, -122.4194))
	if err != nil {
		log.Fatal(err)
	}

	plan, err := search.Compile(
		search.AllBitmaps(favorites),
		search.During(times, quantum, 3*quantum,
			search.ExactTimestamp(func(id imageidx.ImageID) (uint64, bool) {
				value, ok := canonical[id]
				return value, ok
			})),
		geoClause,
	)
	if err != nil {
		log.Fatal(err)
	}

	workspace := search.NewWorkspace(64)
	matches := make([]search.ImageID, 0, 16)
	matches = plan.Search(matches, workspace)
	fmt.Printf("matches=%v stages=%v estimate=%d\n", matches, plan.Stages(), plan.Estimate())
}
