// Command service shows the long-lived path: one compiled Plan shared across
// agents through search.Service, which pools workspaces and timestamp write
// batches. A nil workspace must never be shared; the service owns pooling.
//
// Usage:
//
//	go run ./cmd/imageidx/examples/service
package main

import (
	"context"
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
		search.AllBitmaps(favorites),
		search.During(times, quantum, 3*quantum),
	)
	if err != nil {
		log.Fatal(err)
	}

	service := search.NewService(plan, 64, 16)
	service.Prewarm(2)

	ctx := context.Background()
	matches, err := service.QueryContext(ctx, nil)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("matches=%v count=%d empty=%v\n", matches, service.Count(), service.IsEmpty())

	// Timestamp writes go through the owning index, not the service, but the
	// service pools reusable batches: one batch per writer at a time.
	batch := service.AcquireBatch()
	defer service.ReleaseBatch(batch)
	if ok := batch.Append(timeidx.Move(300, 3*quantum, 2*quantum)); !ok {
		log.Fatal("batch full")
	}
	if err := times.Apply(batch); err != nil {
		log.Fatal(err)
	}
}
