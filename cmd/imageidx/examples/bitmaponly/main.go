// Command bitmaponly exercises the bitmap-only execution shapes: Search,
// Count (which skips materialization via bitmapidx.Count), SearchLimit, and
// Iterate.
//
// Usage:
//
//	go run ./cmd/imageidx/examples/bitmaponly
package main

import (
	"fmt"
	"log"

	imageidx "github.com/evanoberholster/imageIdx"
	"github.com/evanoberholster/imageIdx/bitmapidx"
	"github.com/evanoberholster/imageIdx/search"
)

func mustIndex(key bitmapidx.Key, indexID uint64, ids []imageidx.ImageID) *bitmapidx.Index {
	index, err := bitmapidx.New(bitmapidx.Spec{Key: key, IndexID: indexID, IDs: ids}, bitmapidx.Config{})
	if err != nil {
		log.Fatal(err)
	}
	return index
}

func main() {
	tags := mustIndex(bitmapidx.Key{Type: bitmapidx.IndexTypeTag, ValueID: 7}, 1,
		[]imageidx.ImageID{1, 2, 3, 5})
	defer tags.Close()
	album := mustIndex(bitmapidx.Key{Type: bitmapidx.IndexTypeAlbum, ValueID: 8}, 2,
		[]imageidx.ImageID{2, 3, 4})
	defer album.Close()

	plan, err := search.Compile(search.AllBitmaps(tags), search.AllBitmaps(album))
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
