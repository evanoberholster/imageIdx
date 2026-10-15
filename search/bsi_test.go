package search

import (
	"context"
	"errors"
	"slices"
	"testing"

	schematypes "github.com/evanoberholster/imageIdx/schema"
	"github.com/evanoberholster/imageIdx/index"
	"github.com/evanoberholster/imageIdx/index/bitmapidx"
	"github.com/evanoberholster/imageIdx/index/radixhash"
)

func TestUnifiedBSIRange(t *testing.T) {
	schema := schematypes.NewDefinitions()
	quality, err := index.BitmapDef(bitmapidx.NamespaceSpec{Namespace: 40, Kind: schematypes.KindBitmapBSI, BSIWidth: bitmapidx.BSIWidth16}, "quality", schematypes.EntityImage)
	if err != nil {
		t.Fatal(err)
	}
	tags, err := index.BitmapDef(bitmapidx.NamespaceSpec{Namespace: 41, Kind: schematypes.KindBitmapEnum}, "tags", schematypes.EntityImage)
	if err != nil {
		t.Fatal(err)
	}
	if err := schema.RegisterAll(quality, tags); err != nil {
		t.Fatal(err)
	}
	db, err := index.Create(t.TempDir(), index.Spec{Schema: schema})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, entry := range []struct {
		id    schematypes.ValueID
		value uint16
	}{{1, 0}, {2, 12}, {3, 85}, {4, 85}} {
		if err := db.Add(index.Entry{ValueID: entry.id, Bitmaps: []index.BitmapValue{
			{Namespace: 40, Value: bitmapidx.Uint16Value(entry.value)},
			{Namespace: 41, Value: bitmapidx.EnumStringValue("favorite")},
		}}); err != nil {
			t.Fatal(err)
		}
	}
	eng, err := New(db)
	if err != nil {
		t.Fatal(err)
	}
	clause, err := eng.BSIRange(40, bitmapidx.ValuesBetween(12, 85))
	if err != nil {
		t.Fatal(err)
	}
	plan, err := eng.Compile(schematypes.EntityImage, clause)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	ws := NewWorkspace(8)
	want := []ImageID{2, 3, 4}
	got, err := plan.AppendIDs(ctx, nil, ws)
	if err != nil || !slices.Equal(got, want) {
		t.Fatalf("AppendIDs = %v, %v", got, err)
	}
	if n, err := plan.Count(ctx, ws); err != nil || n != 3 {
		t.Fatalf("Count = %d, %v", n, err)
	}
	if got, err := plan.FilterIDs(ctx, nil, []ImageID{4, 1, 3, 4, 2}, ws); err != nil || !slices.Equal(got, []ImageID{4, 3, 4, 2}) {
		t.Fatalf("FilterIDs = %v, %v", got, err)
	}
	if got, err := plan.AppendPage(ctx, nil, ws, Page{Limit: 2}); err != nil || !slices.Equal(got, []ImageID{2, 3}) {
		t.Fatalf("AppendPage = %v, %v", got, err)
	}
	if err := db.Delete(schematypes.EntityImage, 3); err != nil {
		t.Fatal(err)
	}
	refreshed, err := plan.Refresh(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := refreshed.AppendIDs(ctx, nil, ws); err != nil || !slices.Equal(got, []ImageID{2, 4}) {
		t.Fatalf("after deletion = %v, %v", got, err)
	}
	if _, err := Compile(NamespaceBSIRange(mustBitmap(t, db, 40), bitmapidx.ValuesBetween(0, 1<<16))); !errors.Is(err, bitmapidx.ErrBSIValueRange) {
		t.Fatalf("invalid width = %v", err)
	}
	if _, err := Compile(NamespaceBSIRange(mustBitmap(t, db, 41), bitmapidx.ValuesAtLeast(1))); !errors.Is(err, bitmapidx.ErrBSINotBSI) {
		t.Fatalf("wrong namespace = %v", err)
	}
}

func mustBitmap(t *testing.T, db *index.Store, ns schematypes.Namespace) *bitmapidx.Index {
	t.Helper()
	idx, ok := db.Bitmap(ns)
	if !ok {
		t.Fatalf("bitmap namespace %d unavailable", ns)
	}
	return idx
}

func TestEngineRejectsNilIndexesAndNonImageACL(t *testing.T) {
	schema := schematypes.NewDefinitions()
	def, err := index.BitmapDef(bitmapidx.NamespaceSpec{Namespace: 42, Kind: schematypes.KindBitmapEnum}, "tags", schematypes.EntityFace)
	if err != nil {
		t.Fatal(err)
	}
	if err := schema.Register(def); err != nil {
		t.Fatal(err)
	}
	db, err := index.Create(t.TempDir(), index.Spec{Schema: schema})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	eng, _ := New(db)
	for _, clause := range []Clause{AllValues(nil, bitmapidx.EnumStringValue("x")), During(nil, 0, 1), InCells(nil, 1)} {
		if _, err := eng.Compile(schematypes.EntityFace, clause); !errors.Is(err, ErrInvalidClause) {
			t.Fatalf("nil index: %v", err)
		}
	}
	if _, err := eng.Compile(schematypes.EntityFace, WithACL(1)); !errors.Is(err, ErrIncompatibleEntity) {
		t.Fatalf("face ACL: %v", err)
	}
}

func BenchmarkBSIPlan(b *testing.B) {
	idx, err := bitmapidx.NewMemory(bitmapidx.NamespaceSpec{
		Namespace: 40, Kind: schematypes.KindBitmapBSI, BSIWidth: bitmapidx.BSIWidth16,
	}, bitmapidx.NamespaceConfig{})
	if err != nil {
		b.Fatal(err)
	}
	defer idx.Close()
	for id := schematypes.ValueID(1); id <= 1024; id++ {
		if err := idx.Set(id, uint64(id%100)); err != nil {
			b.Fatal(err)
		}
	}
	plan, err := Compile(NamespaceBSIRange(idx, bitmapidx.ValuesBetween(40, 60)))
	if err != nil {
		b.Fatal(err)
	}
	ctx := context.Background()
	ws := NewWorkspace(256)
	candidates := []ImageID{1, 40, 50, 75, 140, 150}
	b.Run("FilterIDs", func(b *testing.B) {
		b.ReportAllocs()
		dst := make([]ImageID, 0, len(candidates))
		for i := 0; i < b.N; i++ {
			if _, err := plan.FilterIDs(ctx, dst[:0], candidates, ws); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("Count", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if _, err := plan.Count(ctx, ws); err != nil {
				b.Fatal(err)
			}
		}
	})
}

func TestUnifiedExactHash(t *testing.T) {
	schema := schematypes.NewDefinitions()
	if err := schema.Register(schematypes.NamespaceDef{Namespace: 50, Name: "digest", Kind: schematypes.KindHashExact, Entity: schematypes.EntityImage}); err != nil {
		t.Fatal(err)
	}
	db, err := index.Create(t.TempDir(), index.Spec{Schema: schema})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	digest := [32]byte{1, 2, 3}
	if err := db.Add(index.Entry{ValueID: 10, Hashes: []index.HashValue{{Namespace: 50, Hash: radixhash.Hash(digest)}}}); err != nil {
		t.Fatal(err)
	}
	eng, _ := New(db)
	clause, err := eng.ExactHash(50, HashSHA256, digest)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := eng.Compile(schematypes.EntityImage, clause)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := plan.AppendIDs(context.Background(), nil, nil); err != nil || !slices.Equal(got, []ImageID{10}) {
		t.Fatalf("hash query = %v, %v", got, err)
	}
	stages := plan.Stages()
	if len(stages) != 2 {
		t.Fatalf("stages = %+v", stages)
	}
	var bound bool
	for _, stage := range stages {
		bound = bound || stage.Kind == StageHash && stage.Namespace == 50
	}
	if !bound {
		t.Fatalf("hash namespace missing from %+v", stages)
	}
	idx, _ := db.Hash(50)
	if _, err := eng.Compile(schematypes.EntityImage, ExactHash(idx, HashSHA256, digest)); !errors.Is(err, ErrInvalidClause) {
		t.Fatalf("unbound hash clause = %v", err)
	}
	if err := db.Delete(schematypes.EntityImage, 10); err != nil {
		t.Fatal(err)
	}
	if got, err := plan.AppendIDs(context.Background(), nil, nil); err != nil || len(got) != 0 {
		t.Fatalf("deleted hash query = %v, %v", got, err)
	}
}
