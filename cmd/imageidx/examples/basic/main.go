// Command basic runs one entity-scoped bitmap + timestamp + geo query against
// a unified index.Store. Usage: go run ./cmd/imageidx/examples/basic
package main

import (
	"context"
	"fmt"
	"log"
	"os"

	schematypes "github.com/evanoberholster/imageIdx/schema"
	"github.com/evanoberholster/imageIdx/index"
	"github.com/evanoberholster/imageIdx/index/bitmapidx"
	"github.com/evanoberholster/imageIdx/index/geoidx"
	"github.com/evanoberholster/imageIdx/index/timeidx"
	"github.com/evanoberholster/imageIdx/search"
)

const quantum = uint64(1) << timeidx.TimestampShift

func main() {
	schema := schematypes.NewDefinitions()
	tags, err := index.BitmapDef(bitmapidx.NamespaceSpec{
		Namespace: schematypes.NamespaceTag, Kind: schematypes.KindBitmapEnum,
	}, "tags", schematypes.EntityImage)
	if err != nil {
		log.Fatal(err)
	}
	if err := schema.RegisterAll(tags,
		schematypes.NamespaceDef{Namespace: 100, Name: "timestamp", Kind: schematypes.KindTimestamp, Entity: schematypes.EntityImage},
		schematypes.NamespaceDef{Namespace: schematypes.NamespaceLocation, Name: "location", Kind: schematypes.KindGeo, Entity: schematypes.EntityImage},
	); err != nil {
		log.Fatal(err)
	}
	dir, err := os.MkdirTemp("", "imageidx-basic-")
	if err != nil {
		log.Fatal(err)
	}
	defer os.RemoveAll(dir)
	db, err := index.Create(dir, index.Spec{Schema: schema})
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	canonical := map[schematypes.ImageID]uint64{100: quantum, 200: 2 * quantum, 300: 3 * quantum, 400: 4 * quantum}
	for _, row := range []struct {
		id       schematypes.ImageID
		lat, lng float64
	}{{100, 37.7749, -122.4194}, {200, 37.7749, -122.4194},
		{300, 34.0522, -118.2437}, {400, 40.7128, -74.0060}} {
		key, err := geoidx.KeyFromLatLng(row.lat, row.lng)
		if err != nil {
			log.Fatal(err)
		}
		entry := index.Entry{ValueID: row.id,
			Times: []index.TimeValue{{Namespace: 100, Timestamp: canonical[row.id]}},
			Geos:  []index.GeoValue{{Namespace: schematypes.NamespaceLocation, Key: key}},
		}
		if row.id != 400 {
			entry.Bitmaps = []index.BitmapValue{{Namespace: schematypes.NamespaceTag, Value: bitmapidx.EnumStringValue("favorite")}}
		}
		if err := db.Add(entry); err != nil {
			log.Fatal(err)
		}
	}
	cellID, err := geoidx.CellIDFromLatLng(37.7749, -122.4194)
	if err != nil {
		log.Fatal(err)
	}
	engine, err := search.New(db)
	if err != nil {
		log.Fatal(err)
	}
	favorites, err := engine.Values(schematypes.NamespaceTag, bitmapidx.EnumStringValue("favorite"))
	if err != nil {
		log.Fatal(err)
	}
	during, err := engine.During(100, quantum, 3*quantum,
		search.ExactTimestamp(func(id schematypes.ImageID) (uint64, bool) {
			stamp, ok := canonical[id]
			return stamp, ok
		}))
	if err != nil {
		log.Fatal(err)
	}
	cells, err := engine.InCells(schematypes.NamespaceLocation, cellID)
	if err != nil {
		log.Fatal(err)
	}
	plan, err := engine.Compile(schematypes.EntityImage, favorites, during, cells)
	if err != nil {
		log.Fatal(err)
	}
	workspace := search.NewWorkspace(64)
	matches, err := plan.AppendIDs(context.Background(), nil, workspace)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("matches=%v stages=%v estimate=%d\n", matches, plan.Stages(), plan.Estimate())
}
