package hashidx_test

import (
	"fmt"
	"os"

	"github.com/evanoberholster/imageIdx/index/hashIdx"
)

// ExampleStore exercises the public Store lifecycle end to end with no
// internals: create, write both indexes, search, checkpoint, reopen, and
// verify. It is the usability proof for hashIdx as a standalone package:
// combined search integration stays a post-V1 co-design item, with
// CandidateSet and the shared ImageID as the seam.
func ExampleStore() {
	dir, err := os.MkdirTemp("", "hashidx-example")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(dir)

	store, err := hashidx.NewStore(dir, hashidx.StoreSpec{
		Exact: hashidx.ExactSpec{Algorithm: hashidx.AlgorithmSHA256},
	}, hashidx.DurabilityOS)
	if err != nil {
		panic(err)
	}
	for i, phash := range []uint64{0x0, 0x1, 0xFFFF} {
		if err := store.Perceptual().Add(hashidx.Entry{ImageID: hashidx.ImageID(i + 1), PHash64: phash}); err != nil {
			panic(err)
		}
	}
	digest := hashidx.SumSHA256([]byte("cover"))
	if err := store.Exact().Add(digest, 1); err != nil {
		panic(err)
	}
	if err := store.Exact().Add(digest, 2); err != nil {
		panic(err)
	}
	// Radius 64 matches every 64-bit hash; the query pays one popcount
	// per record and nothing else.
	all := store.Perceptual().SearchPHashRadius(nil, 0, 64, nil)
	hits := store.Exact().Lookup(nil, digest)
	fmt.Println("perceptual records:", store.Perceptual().Metadata().RecordCount)
	fmt.Println("radius matches:", len(all))
	fmt.Println("exact ids:", hits)
	fmt.Println("exact live:", store.Exact().LiveCount())
	if err := store.Checkpoint(); err != nil {
		panic(err)
	}
	fmt.Println("perceptual gen:", store.Perceptual().Metadata().Generation)
	fmt.Println("exact gen:", store.Exact().Metadata().Generation)
	if err := store.Close(); err != nil {
		panic(err)
	}
	reopened, err := hashidx.OpenStore(dir, hashidx.DurabilityOS)
	if err != nil {
		panic(err)
	}
	defer reopened.Close()
	fmt.Println("reopen records:", reopened.Perceptual().Metadata().RecordCount)
	fmt.Println("reopen exact ids:", reopened.Exact().Lookup(nil, digest))
	// Output:
	// perceptual records: 3
	// radius matches: 3
	// exact ids: [1 2]
	// exact live: 2
	// perceptual gen: 4
	// exact gen: 3
	// reopen records: 3
	// reopen exact ids: [1 2]
}
