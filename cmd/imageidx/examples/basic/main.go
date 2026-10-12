// Command basic runs one entity-scoped bitmap + timestamp + geo query against
// a unified index.Store. Usage: go run ./cmd/imageidx/examples/basic
package main

import (
	"context"
	"fmt"
	"log"
	"os"

	common "github.com/evanoberholster/imageIdx/common"
	"github.com/evanoberholster/imageIdx/index"
	"github.com/evanoberholster/imageIdx/index/bitmapidx"
	"github.com/evanoberholster/imageIdx/index/geoidx"
	"github.com/evanoberholster/imageIdx/index/timeidx"
	"github.com/evanoberholster/imageIdx/search"
)

const quantum = uint64(1) << timeidx.TimestampShift

func main() {
	schema := common.NewSchema()
	tags, err := index.BitmapDef(bitmapidx.NamespaceSpec{
		Namespace: common.NamespaceTag, Kind: common.KindBitmapEnum,
	}, "tags", common.EntityImage)
	if err != nil {
		log.Fatal(err)
	}
	if err := schema.RegisterAll(tags,
		common.NamespaceDef{Namespace: 100, Name: "timestamp", Kind: common.KindTimestamp, Entity: common.EntityImage},
		common.NamespaceDef{Namespace: common.NamespaceLocation, Name: "location", Kind: common.KindGeo, Entity: common.EntityImage},
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

	canonical := map[common.ImageID]uint64{100: quantum, 200: 2 * quantum, 300: 3 * quantum, 400: 4 * quantum}
	for _, row := range []struct {
		id       common.ImageID
		lat, lng float64
	}{{100, 37.7749, -122.4194}, {200, 37.7749, -122.4194},
		{300, 34.0522, -118.2437}, {400, 40.7128, -74.0060}} {
		key, err := geoidx.KeyFromLatLng(row.lat, row.lng)
		if err != nil {
			log.Fatal(err)
		}
		entry := index.Entry{ValueID: row.id,
			Times: []index.TimeValue{{Namespace: 100, Timestamp: canonical[row.id]}},
			Geos:  []index.GeoValue{{Namespace: common.NamespaceLocation, Key: key}},
		}
		if row.id != 400 {
			entry.Bitmaps = []index.BitmapValue{{Namespace: common.NamespaceTag, Value: bitmapidx.EnumStringValue("favorite")}}
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
	favorites, err := engine.Values(common.NamespaceTag, bitmapidx.EnumStringValue("favorite"))
	if err != nil {
		log.Fatal(err)
	}
	during, err := engine.During(100, quantum, 3*quantum,
		search.ExactTimestamp(func(id common.ImageID) (uint64, bool) {
			stamp, ok := canonical[id]
			return stamp, ok
		}))
	if err != nil {
		log.Fatal(err)
	}
	cells, err := engine.InCells(common.NamespaceLocation, cellID)
	if err != nil {
		log.Fatal(err)
	}
	plan, err := engine.Compile(common.EntityImage, favorites, during, cells)
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
