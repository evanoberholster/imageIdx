package index_test

import (
	"context"
	"errors"
	"slices"
	"testing"

	common "github.com/evanoberholster/imageIdx/common"
	"github.com/evanoberholster/imageIdx/index"
	"github.com/evanoberholster/imageIdx/index/bitmapidx"
	"github.com/evanoberholster/imageIdx/index/radixhash"
	"github.com/evanoberholster/imageIdx/index/vectoridx"
	"github.com/evanoberholster/imageIdx/search"
)

// P0-1: a hash update must invalidate the previous (hash, id) association
// without a rebuild and without relying on entity liveness.
func TestHashUpdateInvalidatesStaleAssociation(t *testing.T) {
	db, err := index.Create(t.TempDir(), index.Spec{Schema: testSchema(t)})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	hA := radixhash.Hash{0xA}
	hB := radixhash.Hash{0xB}
	if err := db.Add(index.Entry{ValueID: 100, Hashes: []index.HashValue{{Namespace: 4, Hash: hA}}}); err != nil {
		t.Fatal(err)
	}
	// A second ValueID also associated with hA must survive.
	if err := db.Add(index.Entry{ValueID: 200, Hashes: []index.HashValue{{Namespace: 4, Hash: hA}}}); err != nil {
		t.Fatal(err)
	}
	if err := db.Update(index.Change{
		Before: index.Entry{ValueID: 100, Hashes: []index.HashValue{{Namespace: 4, Hash: hA}}},
		After:  index.Entry{ValueID: 100, Hashes: []index.HashValue{{Namespace: 4, Hash: hB}}},
	}); err != nil {
		t.Fatal(err)
	}
	hashIdx, _ := db.Hash(4)
	if got := hashIdx.Lookup(nil, hA); slices.Contains(got, uint64(100)) {
		t.Fatalf("stale hash A still returns 100: %v", got)
	}
	if got := hashIdx.Lookup(nil, hA); !slices.Contains(got, uint64(200)) {
		t.Fatalf("sibling 200 lost from hash A: %v", got)
	}
	if got := hashIdx.Lookup(nil, hB); !slices.Contains(got, uint64(100)) {
		t.Fatalf("hash B does not return 100: %v", got)
	}
}

// P0-2: clauses from different entity kinds must not compile together, and a
// clause bound to another Store must be rejected.
func TestSearchRejectsCrossEntityCompilation(t *testing.T) {
	db, err := index.Create(t.TempDir(), index.Spec{Schema: testSchema(t)})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	eng, err := search.New(db)
	if err != nil {
		t.Fatal(err)
	}
	imageTime, err := eng.During(1, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	faceTime, err := eng.During(7, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	imageTag, err := eng.Values(6, bitmapidx.EnumStringValue("cat"))
	if err != nil {
		t.Fatal(err)
	}

	// Same-entity intersections compile.
	if _, err := eng.Compile(common.EntityImage, imageTime, imageTag); err != nil {
		t.Fatalf("image/image compile failed: %v", err)
	}
	// Image + face is rejected.
	if _, err := eng.Compile(common.EntityImage, imageTime, faceTime); !errors.Is(err, search.ErrIncompatibleEntity) {
		t.Fatalf("image/face compile err = %v, want ErrIncompatibleEntity", err)
	}
	// An image bitmap clause under the face entity is rejected.
	if _, err := eng.Compile(common.EntityFace, imageTag); !errors.Is(err, search.ErrIncompatibleEntity) {
		t.Fatalf("face/imageTag compile err = %v, want ErrIncompatibleEntity", err)
	}
	// A clause referencing another Store's engine is rejected.
	other, err := index.Create(t.TempDir(), index.Spec{Schema: testSchema(t)})
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	foreignTag, ok := other.Bitmap(6)
	if !ok {
		t.Fatal("foreign tag missing")
	}
	if _, err := eng.Compile(common.EntityImage, search.AllValues(foreignTag, bitmapidx.EnumStringValue("cat"))); !errors.Is(err, search.ErrIncompatibleEntity) {
		t.Fatalf("foreign store clause err = %v, want ErrIncompatibleEntity", err)
	}
}

// P0-2: overlapping numeric ValueIDs stay isolated by entity kind.
func TestSearchEntityIsolationOverlappingIDs(t *testing.T) {
	db, err := index.Create(t.TempDir(), index.Spec{Schema: testSchema(t)})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Add(index.Entry{ValueID: 100, Times: []index.TimeValue{{Namespace: 1, Timestamp: 5}}}); err != nil {
		t.Fatal(err)
	}
	if err := db.Add(index.Entry{ValueID: 100, Times: []index.TimeValue{{Namespace: 7, Timestamp: 5}}}); err != nil {
		t.Fatal(err)
	}
	eng, _ := search.New(db)
	img, _ := eng.During(1, 0, 10)
	face, _ := eng.During(7, 0, 10)
	ctx := context.Background()
	ws := search.NewWorkspace(16)
	imgPlan, err := eng.Compile(common.EntityImage, img)
	if err != nil {
		t.Fatal(err)
	}
	imgIDs, err := imgPlan.AppendIDs(ctx, nil, ws)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(imgIDs, []uint64{100}) {
		t.Fatalf("image plan = %v", imgIDs)
	}
	facePlan, err := eng.Compile(common.EntityFace, face)
	if err != nil {
		t.Fatal(err)
	}
	faceIDs, err := facePlan.AppendIDs(ctx, nil, ws)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(faceIDs, []uint64{100}) {
		t.Fatalf("face plan = %v", faceIDs)
	}
}

// P0-3: a batch that fails prevalidation must not mutate any engine.
func TestPrevalidationPreventsPartialMutation(t *testing.T) {
	db, err := index.Create(t.TempDir(), index.Spec{Schema: testSchema(t)})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	// The vector entry has an out-of-range ValueID; the time entry is valid.
	// Prevalidation must reject the whole batch before the time engine commits.
	vec := make([]float32, 512)
	batch := db.NewBatch(2)
	if err := batch.Add(index.Entry{ValueID: 7, Times: []index.TimeValue{{Namespace: 1, Timestamp: 1}}}); err != nil {
		t.Fatal(err)
	}
	if err := batch.Add(index.Entry{ValueID: uint64(1) << 33, Vectors: []index.VectorValue{{Namespace: 5, Element: vectoridx.ElementFloat32, Float32: vec}}}); err != nil {
		t.Fatal(err)
	}
	if err := db.Apply(batch); !errors.Is(err, index.ErrValueIDTooLarge) {
		t.Fatalf("err = %v, want ErrValueIDTooLarge", err)
	}
	timeIdx, _ := db.Time(1)
	if got := timeIdx.Search(nil, 0, 10); len(got) != 0 {
		t.Fatalf("time engine mutated despite prevalidation failure: %v", got)
	}
}

// P0-3: a failure during engine application marks the intended namespace stale
// and hides it from accessors, while unrelated namespaces stay usable.
func TestPartialFailureDegradesNamespace(t *testing.T) {
	db, err := index.Create(t.TempDir(), index.Spec{Schema: testSchema(t)})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	hashIdx, ok := db.Hash(4)
	if !ok {
		t.Fatal("hash namespace missing")
	}
	if err := hashIdx.Close(); err != nil {
		t.Fatal(err)
	}
	err = db.Add(index.Entry{ValueID: 5, Hashes: []index.HashValue{{Namespace: 4, Hash: radixhash.Hash{9}}}})
	if err == nil {
		t.Fatal("expected an error from the closed hash engine")
	}
	if _, ok := db.Hash(4); ok {
		t.Fatal("degraded hash namespace is still resolvable")
	}
	h, _ := db.HealthOf(4)
	if h.Available || !h.Stale {
		t.Fatalf("health = %+v, want unavailable+stale", h)
	}
	if degraded := db.DegradedNamespaces(); len(degraded) == 0 {
		t.Fatal("no degraded namespaces reported")
	}
	if _, ok := db.Bitmap(6); !ok {
		t.Fatal("unrelated namespace became unavailable")
	}
}

// P0-4: completeness is per namespace and the safe watermark is the minimum.
func TestSourceGenerationCompleteness(t *testing.T) {
	dir := t.TempDir()
	db, err := index.Create(dir, index.Spec{Schema: testSchema(t)})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.CheckSourceGeneration(1); !errors.Is(err, index.ErrIndexStale) {
		t.Fatalf("fresh store check err = %v, want ErrIndexStale", err)
	}
	// An empty generation-only batch claims completeness for every namespace.
	if err := db.ApplyWithGeneration(db.NewBatch(0), 5); err != nil {
		t.Fatal(err)
	}
	if got := db.SourceGeneration(); got != 5 {
		t.Fatalf("SourceGeneration = %d, want 5", got)
	}
	if err := db.CheckSourceGeneration(5); err != nil {
		t.Fatalf("CheckSourceGeneration(5) = %v", err)
	}
	if err := db.CheckSourceGeneration(6); !errors.Is(err, index.ErrIndexStale) {
		t.Fatalf("CheckSourceGeneration(6) = %v, want ErrIndexStale", err)
	}
	// The timeidx engine carries its own watermark too.
	if timeIdx, ok := db.Time(1); ok {
		if got := timeIdx.SourceGeneration(); got != 5 {
			t.Fatalf("timeidx watermark = %d, want 5", got)
		}
	}
	// Out-of-order generation is rejected.
	if err := db.ApplyWithGeneration(db.NewBatch(0), 3); err == nil {
		t.Fatal("decreasing source generation accepted")
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := index.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if got := reopened.SourceGeneration(); got != 5 {
		t.Fatalf("reopened SourceGeneration = %d, want 5", got)
	}
	if err := reopened.CheckSourceGeneration(5); err != nil {
		t.Fatalf("reopened CheckSourceGeneration(5) = %v", err)
	}
}

// P0-4: a partial namespace change cannot advance the store-wide safe
// watermark past untouched namespaces.
func TestSourceGenerationPartialBatchDoesNotAdvanceSafe(t *testing.T) {
	db, err := index.Create(t.TempDir(), index.Spec{Schema: testSchema(t)})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	batch := db.NewBatch(1)
	if err := batch.Add(index.Entry{ValueID: 1, Times: []index.TimeValue{{Namespace: 1, Timestamp: 1}}}); err != nil {
		t.Fatal(err)
	}
	if err := db.ApplyWithGeneration(batch, 4); err != nil {
		t.Fatal(err)
	}
	// Only namespace 1 advanced; every other namespace is unrecorded (0).
	if got := db.SourceGeneration(); got != 0 {
		t.Fatalf("safe watermark = %d, want 0 for a partial batch", got)
	}
}
