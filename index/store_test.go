package index_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	common "github.com/evanoberholster/imageIdx/common"
	"github.com/evanoberholster/imageIdx/index"
	"github.com/evanoberholster/imageIdx/index/bitmapidx"
	"github.com/evanoberholster/imageIdx/index/geoidx"
	"github.com/evanoberholster/imageIdx/index/simhash"
	"github.com/evanoberholster/imageIdx/index/vectoridx"
	"github.com/evanoberholster/imageIdx/search"
)

func testSchema(t testing.TB) *common.Schema {
	t.Helper()
	schema := common.NewSchema()
	defs := []common.NamespaceDef{
		{Namespace: 1, Name: "time", Kind: common.KindTimestamp, Entity: common.EntityImage},
		{Namespace: 2, Name: "geo", Kind: common.KindGeo, Entity: common.EntityImage},
		{Namespace: 3, Name: "sim", Kind: common.KindHashPerceptual, Entity: common.EntityImage},
		{Namespace: 4, Name: "sha256", Kind: common.KindHashExact, Entity: common.EntityImage},
		{Namespace: 7, Name: "face-time", Kind: common.KindTimestamp, Entity: common.EntityFace},
	}
	if err := schema.RegisterAll(defs...); err != nil {
		t.Fatal(err)
	}
	tagsDef, err := index.BitmapDef(bitmapidx.NamespaceSpec{Namespace: 6, Kind: common.KindBitmapEnum}, "tags", common.EntityImage)
	if err != nil {
		t.Fatal(err)
	}
	if err := schema.Register(tagsDef); err != nil {
		t.Fatal(err)
	}
	vdef, err := index.VectorDef(vectoridx.Config{
		Namespace:   5,
		Dimensions:  vectoridx.Dim512,
		ElementType: vectoridx.ElementFloat32,
	}, "embedding", common.EntityImage)
	if err != nil {
		t.Fatal(err)
	}
	if err := schema.Register(vdef); err != nil {
		t.Fatal(err)
	}
	return schema
}

func TestCreateAddSearchReopen(t *testing.T) {
	dir := t.TempDir()
	db, err := index.Create(dir, index.Spec{Schema: testSchema(t)}, index.WithDurability(common.DurabilityOS))
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range db.Health() {
		if !h.Available {
			t.Fatalf("namespace %d unavailable: %v", h.Namespace, h.Err)
		}
	}

	geoKey, err := geoidx.KeyFromLatLng(37.7749, -122.4194)
	if err != nil {
		t.Fatal(err)
	}
	vec := make([]float32, 512)
	vec[0] = 1.5
	entry := index.Entry{
		ValueID: 100,
		Times:   []index.TimeValue{{Namespace: 1, Timestamp: 1_000}},
		Geos:    []index.GeoValue{{Namespace: 2, Key: geoKey}},
		Hashes:  []index.HashValue{{Namespace: 4, Hash: [32]byte{1, 2, 3}}},
		Vectors: []index.VectorValue{{Namespace: 5, Element: vectoridx.ElementFloat32, Float32: vec}},
		Bitmaps: []index.BitmapValue{{Namespace: 6, Value: bitmapidx.EnumStringValue("cat")}},
	}
	if err := db.Add(entry); err != nil {
		t.Fatal(err)
	}
	// simhash assigns dense 1-based record IDs, so its first record uses id 1.
	if err := db.Add(index.Entry{
		ValueID: 1,
		Sims:    []index.SimValue{{Namespace: 3, Record: simhash.Record{PHash: 0xAB, Quality: 7}}},
	}); err != nil {
		t.Fatal(err)
	}

	timeIdx, _ := db.Time(1)
	if got := timeIdx.Search(nil, 999, 1_001); !slices.Equal(got, []common.ValueID{100}) {
		t.Fatalf("time search = %v", got)
	}
	tags, _ := db.Bitmap(6)
	cat, err := tags.MatchValues(bitmapidx.EnumStringValue("cat"))
	if err != nil || !cat.Contains(100) {
		t.Fatalf("tag search = %v, %v", cat, err)
	}
	if _, ok := db.Hash(4); !ok {
		t.Fatal("hash namespace missing")
	}
	if _, ok := db.Vector(5); !ok {
		t.Fatal("vector namespace missing")
	}
	sim, ok := db.Sim(3)
	if !ok {
		t.Fatal("sim namespace missing")
	}
	if rec, ok := sim.Get(1); !ok || rec.PHash != 0xAB {
		t.Fatalf("sim record = %+v, %v", rec, ok)
	}

	// Entity isolation: Image 100 is live, Face 100 is not (no face values).
	if err := db.Delete(common.EntityImage, 100); err != nil {
		t.Fatal(err)
	}
	liveImg, liveVal, _ := db.Liveness(common.EntityImage)
	imgBitmap, _ := liveImg.MatchValues(liveVal)
	if imgBitmap.Contains(100) {
		t.Fatal("image 100 still live after delete")
	}
	if got := timeIdx.Search(nil, 999, 1_001); len(got) != 0 {
		t.Fatalf("time search after delete = %v", got)
	}

	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := index.Open(dir, index.WithDurability(common.DurabilityOS))
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	tags2, ok := reopened.Bitmap(6)
	if !ok {
		t.Fatal("tags unavailable after reopen")
	}
	cat2, err := tags2.MatchValues(bitmapidx.EnumStringValue("cat"))
	if err != nil || !cat2.Contains(100) {
		t.Fatalf("reopened tag = %v, %v", cat2, err)
	}
	liveImg2, liveVal2, _ := reopened.Liveness(common.EntityImage)
	img2, _ := liveImg2.MatchValues(liveVal2)
	if img2.Contains(100) {
		t.Fatal("image 100 live again after reopen")
	}
}

func TestEntityIsolationSameNumericID(t *testing.T) {
	dir := t.TempDir()
	db, err := index.Create(dir, index.Spec{Schema: testSchema(t)})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// Same numeric ID under two entity kinds.
	if err := db.Add(index.Entry{ValueID: 100, Times: []index.TimeValue{{Namespace: 1, Timestamp: 5}}}); err != nil {
		t.Fatal(err)
	}
	if err := db.Add(index.Entry{ValueID: 100, Times: []index.TimeValue{{Namespace: 7, Timestamp: 9}}}); err != nil {
		t.Fatal(err)
	}
	faceTime, _ := db.Time(7)
	if got := faceTime.Search(nil, 8, 10); !slices.Equal(got, []common.ValueID{100}) {
		t.Fatalf("face time = %v", got)
	}
	// Deleting the image must not affect the face.
	if err := db.Delete(common.EntityImage, 100); err != nil {
		t.Fatal(err)
	}
	faceLive, faceVal, _ := db.Liveness(common.EntityFace)
	fb, _ := faceLive.MatchValues(faceVal)
	if !fb.Contains(100) {
		t.Fatal("face 100 should remain live")
	}
	if got := faceTime.Search(nil, 8, 10); !slices.Equal(got, []common.ValueID{100}) {
		t.Fatalf("face time after image delete = %v", got)
	}
}

func TestPartialUpdateKeepsUnrelatedNamespaces(t *testing.T) {
	dir := t.TempDir()
	db, err := index.Create(dir, index.Spec{Schema: testSchema(t)})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if err := db.Add(index.Entry{
		ValueID: 1,
		Times:   []index.TimeValue{{Namespace: 1, Timestamp: 10}},
		Bitmaps: []index.BitmapValue{{Namespace: 6, Value: bitmapidx.EnumStringValue("a")}},
	}); err != nil {
		t.Fatal(err)
	}
	// Change the timestamp only; the tag must survive.
	if err := db.Update(index.Change{
		Before: index.Entry{ValueID: 1, Times: []index.TimeValue{{Namespace: 1, Timestamp: 10}}},
		After:  index.Entry{ValueID: 1, Times: []index.TimeValue{{Namespace: 1, Timestamp: 20}}},
	}); err != nil {
		t.Fatal(err)
	}
	timeIdx, _ := db.Time(1)
	if got := timeIdx.Search(nil, 19, 21); !slices.Equal(got, []common.ValueID{1}) {
		t.Fatalf("updated time = %v", got)
	}
	tags, _ := db.Bitmap(6)
	a, _ := tags.MatchValues(bitmapidx.EnumStringValue("a"))
	if !a.Contains(1) {
		t.Fatal("unrelated tag was removed by a time update")
	}

	// Explicit removal: drop the tag.
	if err := db.Update(index.Change{
		Before: index.Entry{ValueID: 1, Bitmaps: []index.BitmapValue{{Namespace: 6, Value: bitmapidx.EnumStringValue("a")}}},
	}); err != nil {
		t.Fatal(err)
	}
	a2, _ := tags.MatchValues(bitmapidx.EnumStringValue("a"))
	if a2.Contains(1) {
		t.Fatal("explicit tag removal failed")
	}
	// Timestamp is untouched by the tag-only change.
	if got := timeIdx.Search(nil, 19, 21); !slices.Equal(got, []common.ValueID{1}) {
		t.Fatalf("time changed by tag update = %v", got)
	}
}

func TestVectorValueIDTooLarge(t *testing.T) {
	dir := t.TempDir()
	db, err := index.Create(dir, index.Spec{Schema: testSchema(t)})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	vec := make([]float32, 512)
	err = db.Add(index.Entry{
		ValueID: uint64(1) << 33,
		Vectors: []index.VectorValue{{Namespace: 5, Element: vectoridx.ElementFloat32, Float32: vec}},
	})
	if !errors.Is(err, index.ErrValueIDTooLarge) {
		t.Fatalf("err = %v, want ErrValueIDTooLarge", err)
	}
}

func TestSearchEngineExcludesDeleted(t *testing.T) {
	dir := t.TempDir()
	db, err := index.Create(dir, index.Spec{Schema: testSchema(t)})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Add(index.Entry{
		ValueID: 100,
		Bitmaps: []index.BitmapValue{{Namespace: 6, Value: bitmapidx.EnumStringValue("cat")}},
	}); err != nil {
		t.Fatal(err)
	}
	eng, err := search.New(db)
	if err != nil {
		t.Fatal(err)
	}
	tag, err := eng.Values(6, bitmapidx.EnumStringValue("cat"))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	ws := search.NewWorkspace(64)
	plan, err := eng.Compile(common.EntityImage, tag)
	if err != nil {
		t.Fatal(err)
	}
	got, err := plan.AppendIDs(ctx, nil, ws)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(got, uint64(100)) {
		t.Fatalf("live entity missing: %v", got)
	}
	if err := db.Delete(common.EntityImage, 100); err != nil {
		t.Fatal(err)
	}
	plan2, err := eng.Compile(common.EntityImage, tag)
	if err != nil {
		t.Fatal(err)
	}
	got2, err := plan2.AppendIDs(ctx, nil, ws)
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(got2, uint64(100)) {
		t.Fatalf("deleted entity returned: %v", got2)
	}
}

func TestOpenMissingStore(t *testing.T) {
	if _, err := index.Open(t.TempDir()); err == nil {
		t.Fatal("opening a non-store directory succeeded")
	}
}

func TestDuplicateNamespaceRejected(t *testing.T) {
	schema := common.NewSchema()
	if err := schema.Register(common.NamespaceDef{Namespace: 1, Name: "a", Kind: common.KindTimestamp, Entity: common.EntityImage}); err != nil {
		t.Fatal(err)
	}
	// common.Schema rejects duplicates at registration; the unified layer also
	// rejects namespaces without an entity.
	schema2 := common.NewSchema()
	if err := schema2.Register(common.NamespaceDef{Namespace: 1, Name: "a", Kind: common.KindTimestamp}); err != nil {
		t.Fatal(err)
	}
	if _, err := index.Create(t.TempDir(), index.Spec{Schema: schema2}); !errors.Is(err, index.ErrEntityRequired) {
		t.Fatalf("err = %v, want ErrEntityRequired", err)
	}
}

func TestFilesShareOneRootDirectory(t *testing.T) {
	dir := t.TempDir()
	db, err := index.Create(dir, index.Spec{Schema: testSchema(t)})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) == 0 {
		t.Fatal("no files written")
	}
	// No subdirectories: everything lives in the one root.
	for _, e := range entries {
		if e.IsDir() {
			t.Fatalf("unexpected subdirectory %q", e.Name())
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "store.bin")); err != nil {
		t.Fatalf("store.bin missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "schema.bin")); err != nil {
		t.Fatalf("schema.bin missing: %v", err)
	}
}
