// Command bitmaponly exercises field-only execution shapes: Search, Count,
// SearchLimit, and Iterate.
//
// Usage:
//
//	go run ./cmd/imageidx/examples/bitmaponly
package main

import (
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
		search.FieldValues(tags, bitmapidx.StringValue("favorite")),
		search.FieldValues(album, bitmapidx.StringValue("holiday")),
	)
	if err != nil {
		log.Fatal(err)
	}

	workspace := search.NewWorkspace(8)
	fmt.Printf("matches=%v\n", plan.Search(nil, workspace))
	fmt.Printf("count=%d\n", plan.Count(workspace))
	fmt.Printf("limit1=%v\n", plan.SearchLimit(nil, workspace, 1))
	fmt.Print("iterated=")
	for id := range plan.Iterate(workspace) {
		fmt.Printf("%d ", id)
	}
	fmt.Println()
}
