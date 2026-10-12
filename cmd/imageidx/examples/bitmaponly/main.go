// Command bitmaponly demonstrates unified image liveness, bitmap predicates,
// and BSI range queries with one reusable search plan.
//
// Usage: go run ./cmd/imageidx/examples/bitmaponly
package main

import (
	"context"
	"fmt"
	"log"
	"os"

	common "github.com/evanoberholster/imageIdx/common"
	"github.com/evanoberholster/imageIdx/index"
	"github.com/evanoberholster/imageIdx/index/bitmapidx"
	"github.com/evanoberholster/imageIdx/search"
)

func main() {
	schema := common.NewSchema()
	tags, err := index.BitmapDef(bitmapidx.NamespaceSpec{
		Namespace: common.NamespaceTag, Kind: common.KindBitmapEnum,
	}, "tags", common.EntityImage)
	if err != nil {
		log.Fatal(err)
	}
	quality, err := index.BitmapDef(bitmapidx.NamespaceSpec{
		Namespace: 40, Kind: common.KindBitmapBSI, BSIWidth: bitmapidx.BSIWidth16,
	}, "quality", common.EntityImage)
	if err != nil {
		log.Fatal(err)
	}
	if err := schema.RegisterAll(tags, quality); err != nil {
		log.Fatal(err)
	}
	dir, err := os.MkdirTemp("", "imageidx-unified-")
	if err != nil {
		log.Fatal(err)
	}
	defer os.RemoveAll(dir)
	db, err := index.Create(dir, index.Spec{Schema: schema})
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	for _, entry := range []struct {
		id      common.ValueID
		quality uint16
	}{{1, 5}, {2, 40}, {3, 85}, {4, 100}} {
		if err := db.Add(index.Entry{ValueID: entry.id, Bitmaps: []index.BitmapValue{
			{Namespace: common.NamespaceTag, Value: bitmapidx.EnumStringValue("favorite")},
			{Namespace: 40, Value: bitmapidx.Uint16Value(entry.quality)},
		}}); err != nil {
			log.Fatal(err)
		}
	}
	engine, err := search.New(db)
	if err != nil {
		log.Fatal(err)
	}
	favorites, err := engine.Values(common.NamespaceTag, bitmapidx.EnumStringValue("favorite"))
	if err != nil {
		log.Fatal(err)
	}
	rangeClause, err := engine.BSIRange(40, bitmapidx.ValuesBetween(40, 85))
	if err != nil {
		log.Fatal(err)
	}
	plan, err := engine.Compile(common.EntityImage, favorites, rangeClause)
	if err != nil {
		log.Fatal(err)
	}
	ctx := context.Background()
	ws := search.NewWorkspace(8)
	ids, err := plan.AppendIDs(ctx, nil, ws)
	if err != nil {
		log.Fatal(err)
	}
	count, err := plan.Count(ctx, ws)
	if err != nil {
		log.Fatal(err)
	}
	page, err := plan.AppendPage(ctx, nil, ws, search.Page{Limit: 1})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("matches=%v count=%d first=%v\n", ids, count, page)
}
