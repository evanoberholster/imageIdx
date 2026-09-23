// Command service shows the long-lived path: one compiled Plan shared across
// callers through search.Executor, which pools exclusive query workspaces.
//
// Usage:
//
//	go run ./cmd/imageidx/examples/service
package main

import (
	"context"
	"fmt"
	"log"
	"os"

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
	fields, err := bitmapidx.NewIndex(fieldDir, []bitmapidx.Field{{
		Type: bitmapidx.IndexTypeTag, Kind: bitmapidx.KindEnum, Storage: bitmapidx.StoragePerType,
	}}, bitmapidx.FieldConfig{}, bitmapidx.DurabilityOS)
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

	times, err := timeidx.New(timeidx.Spec{
		IndexID: 2,
		Entries: []timeidx.Entry{
			{ImageID: 100, Timestamp: quantum},
			{ImageID: 200, Timestamp: 2 * quantum},
			{ImageID: 300, Timestamp: 3 * quantum},
		},
	}, timeidx.Config{})
	if err != nil {
		log.Fatal(err)
	}
	defer times.Close()

	plan, err := search.Compile(
		search.AllValues(favorites, bitmapidx.StringValue("favorite")),
		search.During(times, quantum, 3*quantum),
	)
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

	// Timestamp writers own their batches independently of query execution.
	batch := timeidx.NewBatch(16)
	if ok := batch.Append(timeidx.Move(300, 3*quantum, 2*quantum)); !ok {
		log.Fatal("batch full")
	}
	if err := times.Apply(batch); err != nil {
		log.Fatal(err)
	}
}
