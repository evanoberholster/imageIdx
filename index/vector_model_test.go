package index

import (
	"math/rand"
	"path/filepath"
	"testing"

	"github.com/evanoberholster/imageIdx/index/vectoridx"
	schematypes "github.com/evanoberholster/imageIdx/schema"
)

func TestVectorModelReopen(t *testing.T) {
	const model = "siglip2-base-patch16-224"
	defs := schematypes.NewDefinitions()
	def, err := VectorDef(vectoridx.Config{
		Namespace:   50,
		Model:       model,
		Dimensions:  vectoridx.Dim512,
		ElementType: vectoridx.ElementFloat32,
	}, "embedding", schematypes.EntityImage)
	if err != nil {
		t.Fatal(err)
	}
	if err := defs.Register(def); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "index")
	store, err := Create(path, Spec{Schema: defs})
	if err != nil {
		t.Fatal(err)
	}
	rng := rand.New(rand.NewSource(1))
	vec := make([]float32, vectoridx.Dim512)
	for i := range vec {
		vec[i] = rng.Float32()
	}
	if err := store.Add(Entry{ValueID: 1, Vectors: []VectorValue{{
		Namespace: 50, Element: vectoridx.ElementFloat32, Float32: vec,
	}}}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	idx, ok := reopened.Vector(50)
	if !ok {
		t.Fatal("vector namespace unavailable")
	}
	if got := idx.Model(); got != model {
		t.Fatalf("reopened vector model = %q, want %q", got, model)
	}
}

func TestVectorDefModelRoundTrip(t *testing.T) {
	const model = "siglip2-base-patch16-224"
	def, err := VectorDef(vectoridx.Config{
		Namespace:   50,
		Model:       model,
		Dimensions:  vectoridx.Dim768,
		ElementType: vectoridx.ElementFloat32,
		Metric:      vectoridx.MetricCosine,
	}, "embedding", schematypes.EntityImage)
	if err != nil {
		t.Fatal(err)
	}
	got, err := decodeVectorConfig(def.Config)
	if err != nil {
		t.Fatal(err)
	}
	if got.Model != model || got.Dimensions != vectoridx.Dim768 || got.ElementType != vectoridx.ElementFloat32 || got.Metric != vectoridx.MetricCosine {
		t.Fatalf("decoded config = %+v", got)
	}

	legacy, err := decodeVectorConfig(def.Config[:4])
	if err != nil {
		t.Fatal(err)
	}
	if legacy.Model != "" || legacy.Dimensions != vectoridx.Dim768 {
		t.Fatalf("legacy config = %+v", legacy)
	}

	tooLong := vectoridx.Config{
		Namespace:   51,
		Model:       string(make([]byte, vectoridx.MaxModelBytes+1)),
		Dimensions:  vectoridx.Dim512,
		ElementType: vectoridx.ElementFloat32,
	}
	if _, err := VectorDef(tooLong, "too-long", schematypes.EntityImage); err == nil {
		t.Fatal("oversized model accepted")
	}
}
