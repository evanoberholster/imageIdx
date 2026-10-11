package index_test

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"

	common "github.com/evanoberholster/imageIdx/common"
	"github.com/evanoberholster/imageIdx/index"
	"github.com/evanoberholster/imageIdx/index/bitmapidx"
	"github.com/evanoberholster/imageIdx/index/radixhash"
	"github.com/evanoberholster/imageIdx/index/vectoridx"
	"github.com/evanoberholster/imageIdx/search"
)

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

// assertStoresEqual compares the observable time, tag, and liveness state of two
// stores.
func assertStoresEqual(t *testing.T, a, b *index.Store) {
	t.Helper()
	at, _ := a.Time(1)
	bt, _ := b.Time(1)
	if at == nil || bt == nil {
		t.Fatal("time namespace unavailable")
	}
	max := ^uint64(0)
	if got, want := at.Search(nil, 0, max), bt.Search(nil, 0, max); !slices.Equal(got, want) {
		t.Fatalf("time search mismatch: batch=%v sequential=%v", got, want)
	}
	atag, _ := a.Bitmap(6)
	btag, _ := b.Bitmap(6)
	av, _ := atag.MatchValues(bitmapidx.EnumStringValue("t"))
	bv, _ := btag.MatchValues(bitmapidx.EnumStringValue("t"))
	if got, want := av.AppendTo(nil), bv.AppendTo(nil); !slices.Equal(got, want) {
		t.Fatalf("tag mismatch: batch=%v sequential=%v", got, want)
	}
	al, av2, _ := a.Liveness(common.EntityImage)
	bl, bv2, _ := b.Liveness(common.EntityImage)
	am, _ := al.MatchValues(av2)
	bm, _ := bl.MatchValues(bv2)
	if got, want := am.AppendTo(nil), bm.AppendTo(nil); !slices.Equal(got, want) {
		t.Fatalf("liveness mismatch: batch=%v sequential=%v", got, want)
	}
}

// TestBatchMatchesSequential verifies grouped engine batching produces exactly
// the same observable state as applying each operation individually.
func TestBatchMatchesSequential(t *testing.T) {
	a, err := index.Create(t.TempDir(), index.Spec{Schema: testSchema(t)})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := index.Create(t.TempDir(), index.Spec{Schema: testSchema(t)})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()

	var (
		adds    []index.Entry
		changes []index.Change
		dels    []index.DeleteOp
	)
	for id := uint64(1); id <= 64; id++ {
		adds = append(adds, index.Entry{
			ValueID: id,
			Times:   []index.TimeValue{{Namespace: 1, Timestamp: id}},
			Bitmaps: []index.BitmapValue{{Namespace: 6, Value: bitmapidx.EnumStringValue("t")}},
		})
	}
	for id := uint64(2); id <= 64; id += 2 {
		changes = append(changes, index.Change{
			Before: index.Entry{ValueID: id, Times: []index.TimeValue{{Namespace: 1, Timestamp: id}}},
			After:  index.Entry{ValueID: id, Times: []index.TimeValue{{Namespace: 1, Timestamp: id + 100}}},
		})
	}
	for id := uint64(7); id <= 63; id += 7 {
		dels = append(dels, index.DeleteOp{Entity: common.EntityImage, ID: id})
	}

	// The documented batch contract applies deletes, then changes, then adds.
	// Mirror that order for the sequential reference.
	batch := a.NewBatch(0)
	for _, d := range dels {
		if err := batch.Delete(d.Entity, d.ID); err != nil {
			t.Fatal(err)
		}
		if err := b.Delete(d.Entity, d.ID); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range changes {
		if err := batch.Update(c); err != nil {
			t.Fatal(err)
		}
		if err := b.Update(c); err != nil {
			t.Fatal(err)
		}
	}
	for _, e := range adds {
		if err := batch.Add(e); err != nil {
			t.Fatal(err)
		}
		if err := b.Add(e); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.Apply(batch); err != nil {
		t.Fatal(err)
	}
	assertStoresEqual(t, a, b)
}

// TestSchemaImmutable verifies a Store never shares mutable schema state with
// the caller and cannot gain namespaces at runtime.
func TestSchemaImmutable(t *testing.T) {
	schema := testSchema(t)
	db, err := index.Create(t.TempDir(), index.Spec{Schema: schema})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// Mutating the caller schema after Create must not affect the Store.
	if err := schema.Register(common.NamespaceDef{Namespace: 20, Name: "late", Kind: common.KindTimestamp, Entity: common.EntityImage}); err != nil {
		t.Fatal(err)
	}
	if _, ok := db.Time(20); ok {
		t.Fatal("runtime namespace became visible")
	}

	// A returned NamespaceDef is a deep copy.
	original, ok := db.NamespaceDef(6)
	if !ok || len(original.Config) == 0 {
		t.Fatalf("namespace 6 def = %+v", original)
	}
	original.Config[0] ^= 0xFF
	again, _ := db.NamespaceDef(6)
	if again.Config[0] == original.Config[0] {
		t.Fatal("mutating a returned NamespaceDef changed stored configuration")
	}

	// Schema() is a defensive copy.
	clone := db.Schema()
	if err := clone.Register(common.NamespaceDef{Namespace: 21, Name: "clone", Kind: common.KindTimestamp, Entity: common.EntityImage}); err != nil {
		t.Fatal(err)
	}
	if _, ok := db.NamespaceDef(21); ok {
		t.Fatal("mutating Schema() copy changed the Store")
	}
}

// TestLifecycleConcurrentWithMutations runs checkpoint/sync against a mutating
// writer; it is intended for -race.
func TestLifecycleConcurrentWithMutations(t *testing.T) {
	db, err := index.Create(t.TempDir(), index.Spec{Schema: testSchema(t)})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				_ = db.Checkpoint()
				_ = db.Sync()
			}
		}
	}()
	for i := 0; i < 100; i++ {
		id := uint64(i%32 + 1)
		if err := db.Add(index.Entry{ValueID: id, Times: []index.TimeValue{{Namespace: 1, Timestamp: uint64(i)}}}); err != nil {
			t.Fatal(err)
		}
		if i%10 == 0 {
			if err := db.Delete(common.EntityImage, id); err != nil {
				t.Fatal(err)
			}
		}
	}
	close(stop)
	wg.Wait()
}

// TestVectorFlushFailureDegrades verifies a vector engine failure during flush
// degrades the vector namespace and leaves unrelated namespaces usable.
func TestVectorFlushFailureDegrades(t *testing.T) {
	db, err := index.Create(t.TempDir(), index.Spec{Schema: testSchema(t)})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	v, ok := db.Vector(5)
	if !ok {
		t.Fatal("vector namespace missing")
	}
	if err := v.Close(); err != nil {
		t.Fatal(err)
	}
	vec := make([]float32, 512)
	err = db.Add(index.Entry{ValueID: 1, Vectors: []index.VectorValue{{Namespace: 5, Element: vectoridx.ElementFloat32, Float32: vec}}})
	if err == nil {
		t.Fatal("expected an error from the closed vector engine")
	}
	if _, ok := db.Vector(5); ok {
		t.Fatal("degraded vector namespace is still resolvable")
	}
	h, _ := db.HealthOf(5)
	if h.Available || !h.Stale {
		t.Fatalf("health = %+v, want unavailable+stale", h)
	}
	if _, ok := db.Bitmap(6); !ok {
		t.Fatal("unrelated namespace became unavailable")
	}
}

// TestVectorAddUpdateDeleteReopen verifies vector add/replace/delete through
// staging, persisted and reopened.
func TestVectorAddUpdateDeleteReopen(t *testing.T) {
	dir := t.TempDir()
	db, err := index.Create(dir, index.Spec{Schema: testSchema(t)})
	if err != nil {
		t.Fatal(err)
	}
	vec := make([]float32, 512)
	vec[0] = 1
	if err := db.Add(index.Entry{ValueID: 1, Vectors: []index.VectorValue{{Namespace: 5, Element: vectoridx.ElementFloat32, Float32: vec}}}); err != nil {
		t.Fatal(err)
	}
	v, _ := db.Vector(5)
	got, err := v.SearchFloat32(vec, nil, 1)
	if err != nil || len(got) != 1 || got[0].ValueID != 1 {
		t.Fatalf("initial search = %v, %v", got, err)
	}
	vec2 := make([]float32, 512)
	vec2[1] = 1
	if err := db.Update(index.Change{
		Before: index.Entry{ValueID: 1, Vectors: []index.VectorValue{{Namespace: 5, Element: vectoridx.ElementFloat32, Float32: vec}}},
		After:  index.Entry{ValueID: 1, Vectors: []index.VectorValue{{Namespace: 5, Element: vectoridx.ElementFloat32, Float32: vec2}}},
	}); err != nil {
		t.Fatal(err)
	}
	got, _ = v.SearchFloat32(vec2, nil, 1)
	if len(got) != 1 || got[0].ValueID != 1 {
		t.Fatalf("after update = %v", got)
	}
	if err := db.Delete(common.EntityImage, 1); err != nil {
		t.Fatal(err)
	}
	got, _ = v.SearchFloat32(vec2, nil, 1)
	if len(got) != 0 {
		t.Fatalf("after delete = %v, want empty", got)
	}
	if err := db.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := index.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	v2, ok := reopened.Vector(5)
	if !ok {
		t.Fatal("vector namespace missing after reopen")
	}
	got, _ = v2.SearchFloat32(vec2, nil, 1)
	if len(got) != 0 {
		t.Fatalf("reopened delete not persisted: %v", got)
	}
}
