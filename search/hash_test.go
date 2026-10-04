package search

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"testing"

	common "github.com/evanoberholster/imageIdx/common"
	"github.com/evanoberholster/imageIdx/index/bitmapidx"
	"github.com/evanoberholster/imageIdx/index/radixhash"
	"github.com/zeebo/blake3"
)

func ExampleExactHash() {
	digest := blake3.Sum256([]byte("example content"))

	index, err := radixhash.New(1<<20, radixhash.Config{})
	if err != nil {
		panic(err)
	}
	defer index.Close()
	if err := index.Apply([]radixhash.Entry{
		{Hash: radixhash.Hash(digest), ImageID: 1},
		{Hash: radixhash.Hash(digest), ImageID: 2},
	}); err != nil {
		panic(err)
	}

	plan, err := Compile(ExactHash(index, HashBLAKE3, digest))
	if err != nil {
		panic(err)
	}
	ids, err := plan.Search(context.Background(), nil, nil)
	if err != nil {
		panic(err)
	}
	fmt.Println(ids)

	// Output: [1 2]
}

func newHashIndexForSearch(t *testing.T, entries []radixhash.Entry) *radixhash.Index {
	t.Helper()
	index, err := radixhash.New(1<<20, radixhash.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := index.Apply(entries); err != nil {
		_ = index.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = index.Close() })
	return index
}

func TestHashAlgorithmMetadata(t *testing.T) {
	if !HashSHA256.Valid() || !HashBLAKE3.Valid() || HashUnknown.Valid() {
		t.Fatalf("algorithm Valid mismatch")
	}
	if HashSHA256.String() != "sha256" || HashBLAKE3.String() != "blake3" || HashUnknown.String() != "unknown" {
		t.Fatalf("algorithm String mismatch: %q %q %q", HashSHA256, HashBLAKE3, HashUnknown)
	}
}

func TestExactHashSearch(t *testing.T) {
	digestA := blake3.Sum256([]byte("content-a"))
	digestB := blake3.Sum256([]byte("content-b"))
	digestC := sha256.Sum256([]byte("content-c"))

	blake := newHashIndexForSearch(t, []radixhash.Entry{
		{Hash: radixhash.Hash(digestA), ImageID: 1},
		{Hash: radixhash.Hash(digestA), ImageID: 2}, // duplicate content
		{Hash: radixhash.Hash(digestB), ImageID: 3},
	})
	sha := newHashIndexForSearch(t, []radixhash.Entry{
		{Hash: radixhash.Hash(digestC), ImageID: 4},
	})

	ctx := context.Background()
	var ws Workspace

	plan, err := Compile(ExactHash(blake, HashBLAKE3, digestA))
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	got, err := plan.Search(ctx, nil, &ws)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if !equalIDs(got, []ImageID{1, 2}) {
		t.Fatalf("blake3 duplicate = %v, want [1 2]", got)
	}

	plan, err = Compile(ExactHash(sha, HashSHA256, digestC))
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	got, err = plan.Search(ctx, nil, &ws)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if !equalIDs(got, []ImageID{4}) {
		t.Fatalf("sha256 = %v, want [4]", got)
	}

	missing := sha256.Sum256([]byte("absent"))
	plan, _ = Compile(ExactHash(blake, HashBLAKE3, missing))
	got, err = plan.Search(ctx, nil, &ws)
	if err != nil {
		t.Fatalf("Search missing: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("missing digest = %v, want empty", got)
	}
}

func TestExactHashIntersectsNamespace(t *testing.T) {
	digest := blake3.Sum256([]byte("shared"))
	blake := newHashIndexForSearch(t, []radixhash.Entry{
		{Hash: radixhash.Hash(digest), ImageID: 1},
		{Hash: radixhash.Hash(digest), ImageID: 2},
		{Hash: radixhash.Hash(digest), ImageID: 3},
	})
	tags := enumNamespaceForSearch(t, common.NamespaceTag, "favorite", []uint64{2, 3})

	plan, err := Compile(
		ExactHash(blake, HashBLAKE3, digest),
		AllValues(tags, bitmapidx.EnumStringValue("favorite")),
	)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	var ws Workspace
	got, err := plan.Search(context.Background(), nil, &ws)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if !equalIDs(got, []ImageID{2, 3}) {
		t.Fatalf("intersection = %v, want [2 3]", got)
	}
}

func TestExactHashFilterIDsPreservesOrder(t *testing.T) {
	digest := blake3.Sum256([]byte("filter"))
	blake := newHashIndexForSearch(t, []radixhash.Entry{
		{Hash: radixhash.Hash(digest), ImageID: 1},
		{Hash: radixhash.Hash(digest), ImageID: 2},
	})
	plan, err := Compile(ExactHash(blake, HashBLAKE3, digest))
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	var ws Workspace
	kept, err := plan.FilterIDs(context.Background(), nil, []ImageID{3, 1, 2, 1}, &ws)
	if err != nil {
		t.Fatalf("FilterIDs: %v", err)
	}
	if !equalIDs(kept, []ImageID{1, 2, 1}) {
		t.Fatalf("FilterIDs = %v, want [1 2 1]", kept)
	}
}

func TestExactHashCountAndExists(t *testing.T) {
	digest := blake3.Sum256([]byte("count"))
	blake := newHashIndexForSearch(t, []radixhash.Entry{
		{Hash: radixhash.Hash(digest), ImageID: 7},
		{Hash: radixhash.Hash(digest), ImageID: 8},
	})
	plan, err := Compile(ExactHash(blake, HashBLAKE3, digest))
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	var ws Workspace
	count, err := plan.Count(context.Background(), &ws)
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if count != 2 {
		t.Fatalf("Count = %d, want 2", count)
	}
	found, err := plan.Exists(context.Background(), &ws)
	if err != nil || !found {
		t.Fatalf("Exists = %v, %v; want true", found, err)
	}
}

func TestExactHashStageMetadata(t *testing.T) {
	digest := blake3.Sum256([]byte("meta"))
	blake := newHashIndexForSearch(t, []radixhash.Entry{{Hash: radixhash.Hash(digest), ImageID: 1}})
	plan, err := Compile(ExactHash(blake, HashBLAKE3, digest))
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	stages := plan.Stages()
	if len(stages) != 1 {
		t.Fatalf("stages = %d, want 1", len(stages))
	}
	if stages[0].Kind != StageHash || stages[0].Algorithm != HashBLAKE3 ||
		stages[0].Predicate != "exact-hash" || !stages[0].BufferedFallback {
		t.Fatalf("stage metadata = %+v", stages[0])
	}
	if stages[0].Kind.String() != "hash" {
		t.Fatalf("kind string = %q", stages[0].Kind.String())
	}
}

func TestExactHashRejectsInvalidClause(t *testing.T) {
	digest := blake3.Sum256([]byte("invalid"))
	if _, err := Compile(ExactHash(nil, HashBLAKE3, digest)); !errors.Is(err, ErrInvalidClause) {
		t.Fatalf("nil index = %v, want ErrInvalidClause", err)
	}
	blake := newHashIndexForSearch(t, nil)
	if _, err := Compile(ExactHash(blake, HashUnknown, digest)); !errors.Is(err, ErrInvalidClause) {
		t.Fatalf("unknown algorithm = %v, want ErrInvalidClause", err)
	}
}

func equalIDs(a, b []ImageID) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
