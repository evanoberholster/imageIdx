// Command service shares a unified-store Plan across queries through a pooled
// search.Executor. Usage: go run ./cmd/imageidx/examples/service
package main

import (
	"context"
	"fmt"
	"log"
	"os"

	schematypes "github.com/evanoberholster/imageIdx/schema"
	"github.com/evanoberholster/imageIdx/index"
	"github.com/evanoberholster/imageIdx/index/bitmapidx"
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
	); err != nil {
		log.Fatal(err)
	}
	dir, err := os.MkdirTemp("", "imageidx-service-")
	if err != nil {
		log.Fatal(err)
	}
	defer os.RemoveAll(dir)
	db, err := index.Create(dir, index.Spec{Schema: schema})
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()
	for _, row := range []struct {
		id    schematypes.ValueID
		stamp uint64
	}{{100, quantum}, {200, 2 * quantum}, {300, 3 * quantum}} {
		if err := db.Add(index.Entry{ValueID: row.id,
			Times:   []index.TimeValue{{Namespace: 100, Timestamp: row.stamp}},
			Bitmaps: []index.BitmapValue{{Namespace: schematypes.NamespaceTag, Value: bitmapidx.EnumStringValue("favorite")}},
		}); err != nil {
			log.Fatal(err)
		}
	}
	engine, err := search.New(db)
	if err != nil {
		log.Fatal(err)
	}
	favorites, err := engine.Values(schematypes.NamespaceTag, bitmapidx.EnumStringValue("favorite"))
	if err != nil {
		log.Fatal(err)
	}
	during, err := engine.During(100, quantum, 3*quantum)
	if err != nil {
		log.Fatal(err)
	}
	plan, err := engine.Compile(schematypes.EntityImage, favorites, during)
	if err != nil {
		log.Fatal(err)
	}
	executor, err := search.NewExecutor(search.ExecutorConfig{WorkspaceCapacity: 64})
	if err != nil {
		log.Fatal(err)
	}
	executor.Prewarm(2)
	ctx := context.Background()
	matches, err := executor.AppendIDs(ctx, nil, plan)
	if err != nil {
		log.Fatal(err)
	}
	count, err := executor.Count(ctx, plan)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("matches=%v count=%d empty=%v\n", matches, count, plan.IsEmpty())

	// A unified update moves the timestamp; the same plan reads its new view.
	if err := db.Update(index.Change{
		Before: index.Entry{ValueID: 300, Times: []index.TimeValue{{Namespace: 100, Timestamp: 3 * quantum}}},
		After:  index.Entry{ValueID: 300, Times: []index.TimeValue{{Namespace: 100, Timestamp: 2 * quantum}}},
	}); err != nil {
		log.Fatal(err)
	}
	updated, err := executor.AppendIDs(ctx, nil, plan)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("after update=%v\n", updated)
}
