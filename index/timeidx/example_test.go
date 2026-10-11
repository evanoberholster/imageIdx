package timeidx_test

import (
	"fmt"
	"os"
	"time"

	"github.com/evanoberholster/imageIdx/index/timeidx"
)

// ExampleIndex exercises the public lifecycle end to end with no
// internals: create, write, search, checkpoint, reopen, and verify. It is
// the usability proof for timeidx as a standalone package.
func ExampleIndex() {
	dir, err := os.MkdirTemp("", "timeidx-example")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(dir)
	const offset = uint64(1_000_000)
	const quantum = uint64(1) << timeidx.TimestampShift
	index, err := timeidx.NewStore(dir, timeidx.Spec{Offset: offset}, timeidx.Config{}, timeidx.DurabilityOS)
	if err != nil {
		panic(err)
	}
	batch := timeidx.NewBatch(3)
	batch.Append(timeidx.Insert(1, offset))
	batch.Append(timeidx.Insert(2, offset+quantum))
	batch.Append(timeidx.Insert(3, offset+2*quantum))
	if err := index.Apply(batch); err != nil {
		panic(err)
	}
	fmt.Println("search:", index.Search(nil, offset, offset+quantum))
	if err := index.Checkpoint(); err != nil {
		panic(err)
	}
	fmt.Println("generation:", index.Metadata().Generation)
	if err := index.Close(); err != nil {
		panic(err)
	}
	reopened, err := timeidx.OpenStore(dir, timeidx.Config{}, timeidx.DurabilityOS)
	if err != nil {
		panic(err)
	}
	defer reopened.Close()
	fmt.Println("reopen search:", reopened.Search(nil, offset, offset+3*quantum))
	fmt.Println("reopen count:", reopened.Metadata().BaseCount)
	// Output:
	// search: [1]
	// generation: 2
	// reopen search: [1 2 3]
	// reopen count: 3
}

func ExampleStore() {
	dir, err := os.MkdirTemp("", "timeidx-namespaces")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(dir)
	store, err := timeidx.NewNamespaceStore(dir, timeidx.Config{}, timeidx.DurabilitySync)
	if err != nil {
		panic(err)
	}
	defer store.Close()
	capture, err := store.CreateNamespace(1)
	if err != nil {
		panic(err)
	}
	imported, err := store.CreateNamespace(2)
	if err != nil {
		panic(err)
	}
	if err := store.SealStartup(); err != nil {
		panic(err)
	}
	key, err := timeidx.EncodeTime(time.Date(1960, 1, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		panic(err)
	}
	batch := timeidx.NewBatch(1)
	batch.Append(timeidx.Insert(7, uint64(key)))
	if err := capture.Apply(batch); err != nil {
		panic(err)
	}
	fmt.Println("capture:", capture.Search(nil, uint64(key), uint64(key)+1))
	fmt.Println("imported:", imported.Search(nil, uint64(key), uint64(key)+1))
	// Output:
	// capture: [7]
	// imported: []
}
