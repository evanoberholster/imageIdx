package vectoridx

import (
	"errors"
	"math/rand"
	"testing"

	common "github.com/evanoberholster/imageIdx/common"
)

func TestConfigNamespaceValidation(t *testing.T) {
	base := testConfigFloat512()
	if _, err := New(base); err != nil {
		t.Fatalf("valid namespace config: %v", err)
	}
	base.Namespace = 0
	if _, err := New(base); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("zero namespace config err = %v, want ErrInvalidConfig", err)
	}
}

func TestConfigNamespaceDef(t *testing.T) {
	def := testConfigFloat512().NamespaceDef()
	if def.Namespace != testNamespace || def.Kind != common.KindVector || def.Name != "" {
		t.Fatalf("NamespaceDef() = %+v", def)
	}
}

func TestDimensionsValidation(t *testing.T) {
	for _, d := range []Dimensions{Dim512, Dim768, Dim1024} {
		if !d.Valid() {
			t.Fatalf("%d reported invalid", d)
		}
	}
	for _, d := range []Dimensions{0, 100, 513, 2048} {
		if d.Valid() {
			t.Fatalf("%d reported valid", d)
		}
	}
}

// TestNamespaceAndResultAcrossReopen pins the namespace in the header and
// the NodeID -> ImageID widening in results.
func TestNamespaceAndResultAcrossReopen(t *testing.T) {
	path := tempPath(t)
	namespace := common.Namespace(5)
	idx, err := Create(path, Config{Namespace: namespace, Dimensions: Dim512, ElementType: ElementFloat32}, DurabilitySync)
	if err != nil {
		t.Fatal(err)
	}
	vec := randFloatVec(rand.New(rand.NewSource(101)), Dim512)
	const id = NodeID(4000000000) // above 2^31, still within uint32
	if err := idx.AddFloat32(id, vec); err != nil {
		t.Fatal(err)
	}
	if err := idx.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	if err := idx.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(path, DurabilitySync)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	got := reopened.Config().Namespace
	if got != namespace {
		t.Fatalf("reopened namespace = %d, want %d", got, namespace)
	}
	results, err := reopened.SearchFloat32(vec, nil, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].ImageID != ImageID(id) || results[0].Score != 0 {
		t.Fatalf("result = %+v, want ImageID %d Score 0", results, id)
	}
}
