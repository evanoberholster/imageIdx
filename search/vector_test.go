package search

import (
	"context"
	"math"
	"math/rand"
	"testing"

	"github.com/evanoberholster/imageIdx/index"
	"github.com/evanoberholster/imageIdx/index/bitmapidx"
	"github.com/evanoberholster/imageIdx/index/vectoridx"
	schematypes "github.com/evanoberholster/imageIdx/schema"
)

func TestEngineVectorAndRankVector(t *testing.T) {
	defs := schematypes.NewDefinitions()
	vectorDef, err := index.VectorDef(vectoridx.Config{
		Namespace:   50,
		Dimensions:  vectoridx.Dim512,
		ElementType: vectoridx.ElementFloat32,
		Metric:      vectoridx.MetricL2,
	}, "embedding", schematypes.EntityImage)
	if err != nil {
		t.Fatal(err)
	}
	tagDef, err := index.BitmapDef(bitmapidx.NamespaceSpec{
		Namespace: 51,
		Kind:      schematypes.KindBitmapEnum,
	}, "tags", schematypes.EntityImage)
	if err != nil {
		t.Fatal(err)
	}
	if err := defs.RegisterAll(vectorDef, tagDef); err != nil {
		t.Fatal(err)
	}
	db, err := index.Create(t.TempDir(), index.Spec{Schema: defs})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	rng := rand.New(rand.NewSource(401))
	vectors := make([][]float32, 20)
	for i := range vectors {
		vectors[i] = make([]float32, vectoridx.Dim512)
		for j := range vectors[i] {
			vectors[i][j] = rng.Float32()*4 - 2
		}
		entry := index.Entry{
			ValueID: schematypes.ValueID(i + 1),
			Vectors: []index.VectorValue{{
				Namespace: 50,
				Element:   vectoridx.ElementFloat32,
				Float32:   vectors[i],
			}},
		}
		if i < 5 {
			entry.Bitmaps = []index.BitmapValue{{
				Namespace: 51,
				Value:     bitmapidx.EnumStringValue("keep"),
			}}
		}
		if err := db.Add(entry); err != nil {
			t.Fatal(err)
		}
	}

	engine, err := New(db)
	if err != nil {
		t.Fatal(err)
	}
	vec, ok := engine.Vector(50)
	if !ok {
		t.Fatal("vector namespace unavailable")
	}
	clause, err := engine.Values(51, bitmapidx.EnumStringValue("keep"))
	if err != nil {
		t.Fatal(err)
	}
	plan, err := engine.Compile(schematypes.EntityImage, clause)
	if err != nil {
		t.Fatal(err)
	}
	ws := NewWorkspace(16)
	query := vectors[0]
	got, err := plan.RankVector(context.Background(), nil, ws, vec, query, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 5 {
		t.Fatalf("RankVector returned %d results, want 5", len(got))
	}
	for i, r := range got {
		if r.ImageID < 1 || r.ImageID > 5 {
			t.Fatalf("result %d image %d outside plan", i, r.ImageID)
		}
		if i > 0 && r.Distance < got[i-1].Distance {
			t.Fatalf("results not distance ordered: %+v", got)
		}
	}
	if got[0].ImageID != 1 || math.Abs(float64(got[0].Distance)) > 1e-6 {
		t.Fatalf("self result = %+v, want image 1 distance 0", got[0])
	}

	// Deleting an image must be reflected by the liveness stage.
	if err := db.Delete(schematypes.EntityImage, 3); err != nil {
		t.Fatal(err)
	}
	afterDelete, err := plan.RankVector(context.Background(), nil, ws, vec, query, 5)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range afterDelete {
		if r.ImageID == 3 {
			t.Fatalf("deleted image returned: %+v", afterDelete)
		}
	}

	// Force the ANN branch: cutoff 1 and no flat threshold.
	if err := vec.SetSearchParams(0, 0, 0, 0, 1, -1, 0); err != nil {
		t.Fatal(err)
	}
	ann, err := plan.RankVector(context.Background(), nil, ws, vec, query, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(ann) != 3 {
		t.Fatalf("ANN RankVector returned %d results, want 3", len(ann))
	}
	for _, r := range ann {
		if r.ImageID > 5 || r.ImageID == 3 {
			t.Fatalf("ANN result outside plan: %+v", ann)
		}
	}
}

func TestRankVectorInvalidOptions(t *testing.T) {
	var plan Plan
	if _, err := plan.RankVector(context.Background(), nil, nil, nil, nil, 1); err == nil {
		t.Fatal("nil vector index accepted")
	}
}

func TestRankVectorQueryInt8(t *testing.T) {
	defs := schematypes.NewDefinitions()
	vectorDef, err := index.VectorDef(vectoridx.Config{
		Namespace:   60,
		Dimensions:  vectoridx.Dim512,
		ElementType: vectoridx.ElementInt8,
		Metric:      vectoridx.MetricL2,
	}, "int8-embedding", schematypes.EntityImage)
	if err != nil {
		t.Fatal(err)
	}
	if err := defs.Register(vectorDef); err != nil {
		t.Fatal(err)
	}
	db, err := index.Create(t.TempDir(), index.Spec{Schema: defs})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	rng := rand.New(rand.NewSource(402))
	vectors := make([][]int8, 20)
	for i := range vectors {
		vectors[i] = make([]int8, vectoridx.Dim512)
		for j := range vectors[i] {
			vectors[i][j] = int8(rng.Intn(256) - 128)
		}
		if err := db.Add(index.Entry{
			ValueID: schematypes.ValueID(i + 1),
			Vectors: []index.VectorValue{{
				Namespace: 60,
				Element:   vectoridx.ElementInt8,
				Int8:      vectors[i],
			}},
		}); err != nil {
			t.Fatal(err)
		}
	}

	engine, err := New(db)
	if err != nil {
		t.Fatal(err)
	}
	vec, ok := engine.Vector(60)
	if !ok {
		t.Fatal("int8 vector namespace unavailable")
	}
	plan, err := engine.Compile(schematypes.EntityImage)
	if err != nil {
		t.Fatal(err)
	}
	ws := NewWorkspace(16)
	query := VectorQuery{Int8: vectors[0]}
	got, err := plan.RankVectorQuery(context.Background(), nil, ws, vec, query, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 5 {
		t.Fatalf("RankVectorQuery returned %d results, want 5", len(got))
	}
	if got[0].ImageID != 1 || got[0].Distance != 0 {
		t.Fatalf("self result = %+v, want image 1 distance 0", got[0])
	}
	for i := 1; i < len(got); i++ {
		if got[i].Distance < got[i-1].Distance {
			t.Fatalf("results not distance ordered: %+v", got)
		}
	}

	// Force the ANN branch with the int8 index.
	if err := vec.SetSearchParams(0, 0, 0, 0, 1, -1, 0); err != nil {
		t.Fatal(err)
	}
	ann, err := plan.RankVectorQuery(context.Background(), nil, ws, vec, query, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(ann) != 3 {
		t.Fatalf("ANN RankVectorQuery returned %d results, want 3", len(ann))
	}
	for _, r := range ann {
		if r.ImageID < 1 || r.ImageID > 20 {
			t.Fatalf("ANN result outside namespace: %+v", ann)
		}
	}
}
