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

	common "github.com/evanoberholster/imageIdx/common"
	"github.com/evanoberholster/imageIdx/index/bitmapidx"
	"github.com/evanoberholster/imageIdx/index/timeidx"
	"github.com/evanoberholster/imageIdx/search"
)

const quantum = uint64(1) << timeidx.TimestampShift

func main() {
	namespaceDir, err := os.MkdirTemp("", "imageidx-namespaces-")
	if err != nil {
		log.Fatal(err)
	}
	namespaces, err := bitmapidx.NewIndex(namespaceDir, []bitmapidx.NamespaceSpec{{
		Namespace: common.NamespaceTag, Name: "tag", Kind: common.KindBitmapEnum, Storage: bitmapidx.StoragePerType,
	}}, bitmapidx.NamespaceConfig{}, common.DurabilityOS)
	if err != nil {
		log.Fatal(err)
	}
	defer func() { _ = namespaces.Close(); _ = os.RemoveAll(namespaceDir) }()
	favorites, ok := namespaces.Lookup(common.NamespaceTag)
	if !ok {
		log.Fatal("tag namespace missing")
	}
	favoritesBatch := favorites.NewBatch()
	if err := favoritesBatch.Add(
		bitmapidx.Entry{ImageID: 100, Value: bitmapidx.StringValue("favorite")},
		bitmapidx.Entry{ImageID: 200, Value: bitmapidx.StringValue("favorite")},
		bitmapidx.Entry{ImageID: 300, Value: bitmapidx.StringValue("favorite")},
	); err != nil {
		log.Fatal(err)
	}
	if err := favoritesBatch.Apply(); err != nil {
		log.Fatal(err)
	}
	favoritesBatch.Close()
	if err := favorites.Barrier(); err != nil {
		log.Fatal(err)
	}

	times, err := timeidx.New(timeidx.Spec{
		Namespace: common.Namespace(100),
		Name:      "timestamp",
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
