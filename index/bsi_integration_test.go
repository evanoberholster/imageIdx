package index_test

import (
	"slices"
	"testing"

	schematypes "github.com/evanoberholster/imageIdx/schema"
	"github.com/evanoberholster/imageIdx/index"
	"github.com/evanoberholster/imageIdx/index/bitmapidx"
)

// bsiSchema declares one BSI16 namespace alongside an enum namespace so the
// unified Store routes both through the shared bitmap engine.
func bsiSchema(t testing.TB) *schematypes.Definitions {
	t.Helper()
	schema := schematypes.NewDefinitions()
	bsiDef, err := index.BitmapDef(bitmapidx.NamespaceSpec{
		Namespace: 40, Kind: schematypes.KindBitmapBSI, BSIWidth: bitmapidx.BSIWidth16,
	}, "quality", schematypes.EntityImage)
	if err != nil {
		t.Fatal(err)
	}
	tagDef, err := index.BitmapDef(bitmapidx.NamespaceSpec{
		Namespace: 41, Kind: schematypes.KindBitmapEnum,
		Normalizer: bitmapidx.CodecSpec{Code: bitmapidx.NormalizerIdentity, Version: 1},
	}, "tags", schematypes.EntityImage)
	if err != nil {
		t.Fatal(err)
	}
	if err := schema.RegisterAll(bsiDef, tagDef); err != nil {
		t.Fatal(err)
	}
	return schema
}

func TestUnifiedBSINamespace(t *testing.T) {
	dir := t.TempDir()
	store, err := index.Create(dir, index.Spec{Schema: bsiSchema(t)})
	if err != nil {
		t.Fatal(err)
	}
	bsi, ok := store.Bitmap(40)
	if !ok {
		t.Fatal("BSI namespace missing")
	}
	for id, value := range map[schematypes.ValueID]uint64{100: 85, 200: 12, 300: 85, 400: 0} {
		if err := bsi.Set(id, value); err != nil {
			t.Fatalf("Set(%d,%d): %v", id, value, err)
		}
	}
	equal, err := bsi.Equal(85)
	if err != nil {
		t.Fatal(err)
	}
	if got := equal.AppendTo(nil); !slices.Equal(got, []schematypes.ValueID{100, 300}) {
		t.Fatalf("Equal(85) = %v", got)
	}
	inRange, err := bsi.BSIRange(1, 100)
	if err != nil {
		t.Fatal(err)
	}
	if got := inRange.AppendTo(nil); !slices.Equal(got, []schematypes.ValueID{100, 200, 300}) {
		t.Fatalf("Range(1,100) = %v", got)
	}

	if err := store.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := index.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	rbsi, ok := reopened.Bitmap(40)
	if !ok {
		t.Fatal("BSI namespace missing after reopen")
	}
	if rbsi.Spec().BSIWidth != bitmapidx.BSIWidth16 {
		t.Fatalf("width after reopen = %d", rbsi.Spec().BSIWidth)
	}
	if v, ok, err := rbsi.Get(100); err != nil || !ok || v != 85 {
		t.Fatalf("Get(100) after reopen = %d,%v,%v", v, ok, err)
	}
	// The sibling enum namespace still works alongside the BSI namespace.
	rTags, _ := reopened.Bitmap(41)
	batch := rTags.NewBatch()
	if err := batch.Add(bitmapidx.Entry{ImageID: 100, Value: bitmapidx.EnumStringValue("cat")}); err != nil {
		t.Fatal(err)
	}
	if err := batch.Apply(); err != nil {
		t.Fatal(err)
	}
	if err := rTags.Barrier(); err != nil {
		t.Fatal(err)
	}
	matched, err := rTags.MatchValues(bitmapidx.EnumStringValue("cat"))
	if err != nil {
		t.Fatal(err)
	}
	if got := matched.AppendTo(nil); !slices.Equal(got, []schematypes.ValueID{100}) {
		t.Fatalf("tag match = %v", got)
	}
}
