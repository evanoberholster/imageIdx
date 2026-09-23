// Command bitmaponly exercises field-only execution shapes: AppendIDs, Count,
// AppendPage, and Visit.
//
// Usage:
//
//	go run ./cmd/imageidx/examples/bitmaponly
package main

import (
	"context"
	"fmt"
	"log"
	"os"

	"github.com/evanoberholster/imageIdx/index/bitmapidx"
	"github.com/evanoberholster/imageIdx/search"
)

func main() {
	dir, err := os.MkdirTemp("", "imageidx-fields-")
	if err != nil {
		log.Fatal(err)
	}
	defer os.RemoveAll(dir)
	store, err := bitmapidx.NewIndex(dir, []bitmapidx.Field{
		{Type: bitmapidx.IndexTypeTag, Kind: bitmapidx.KindEnum, Storage: bitmapidx.StoragePerType},
		{Type: bitmapidx.IndexTypeAlbum, Kind: bitmapidx.KindEnum, Storage: bitmapidx.StoragePerType},
	}, bitmapidx.FieldConfig{}, bitmapidx.DurabilityOS)
	if err != nil {
		log.Fatal(err)
	}
	defer store.Close()
	tags, _ := store.Lookup(bitmapidx.IndexTypeTag)
	album, _ := store.Lookup(bitmapidx.IndexTypeAlbum)
	if err := tags.Add(
		bitmapidx.Entry{ImageID: 1, Value: bitmapidx.StringValue("favorite")},
		bitmapidx.Entry{ImageID: 2, Value: bitmapidx.StringValue("favorite")},
		bitmapidx.Entry{ImageID: 3, Value: bitmapidx.StringValue("favorite")},
		bitmapidx.Entry{ImageID: 5, Value: bitmapidx.StringValue("favorite")},
	); err != nil {
		log.Fatal(err)
	}
	if err := album.Add(
		bitmapidx.Entry{ImageID: 2, Value: bitmapidx.StringValue("holiday")},
		bitmapidx.Entry{ImageID: 3, Value: bitmapidx.StringValue("holiday")},
		bitmapidx.Entry{ImageID: 4, Value: bitmapidx.StringValue("holiday")},
	); err != nil {
		log.Fatal(err)
	}

	plan, err := search.Compile(
		search.AllValues(tags, bitmapidx.StringValue("favorite")),
		search.AllValues(album, bitmapidx.StringValue("holiday")),
	)
	if err != nil {
		log.Fatal(err)
	}

	workspace := search.NewWorkspace(8)
	ctx := context.Background()
	matches, err := plan.AppendIDs(ctx, nil, workspace)
	if err != nil {
		log.Fatal(err)
	}
	count, err := plan.Count(ctx, workspace)
	if err != nil {
		log.Fatal(err)
	}
	first, err := plan.AppendPage(ctx, nil, workspace, search.Page{Limit: 1})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("matches=%v\ncount=%d\nfirst=%v\n", matches, count, first)
	fmt.Print("iterated=")
	if err := plan.Visit(ctx, workspace, func(id search.ImageID) bool {
		fmt.Printf("%d ", id)
		return true
	}); err != nil {
		log.Fatal(err)
	}
	fmt.Println()
}
