// Package schema_test verifies the shared schema vocabulary across index packages.
package schema_test

import (
	"errors"
	"testing"

	"github.com/evanoberholster/imageIdx/index"
	"github.com/evanoberholster/imageIdx/index/bitmapidx"
	"github.com/evanoberholster/imageIdx/index/timeidx"
	"github.com/evanoberholster/imageIdx/index/vectoridx"
	schematypes "github.com/evanoberholster/imageIdx/schema"
)

// Namespace numbers used by this test. They are intentionally outside the
// well-known bitmap range and unique within the test schema.
const (
	timelineNamespace   schematypes.Namespace = 90
	exactHashNamespace  schematypes.Namespace = 100
	perceptualNamespace schematypes.Namespace = 101
	vectorNamespace     schematypes.Namespace = 102
)

// integrationDefs builds one NamespaceDef per index kind: bitmap, timestamp,
// exact hash, perceptual hash, and vector.
func integrationDefs(t *testing.T) []schematypes.NamespaceDef {
	t.Helper()

	bitmapDef, err := bitmapidx.NamespaceSpec{
		Namespace: schematypes.NamespaceTag,
		Kind:      schematypes.KindBitmapEnum,
	}.NamespaceDef("tag")
	if err != nil {
		t.Fatalf("bitmap NamespaceDef: %v", err)
	}

	vectorDef := vectoridx.Config{Namespace: vectorNamespace}.NamespaceDef()
	vectorDef.Name = "embedding"

	return []schematypes.NamespaceDef{
		bitmapDef,
		timeidx.Spec{Namespace: timelineNamespace, Name: "timeline"}.NamespaceDef(),
		{Namespace: exactHashNamespace, Name: "exact-content", Kind: schematypes.KindHashExact, Entity: schematypes.EntityImage},
		{Namespace: perceptualNamespace, Name: "perceptual", Kind: schematypes.KindHashPerceptual, Entity: schematypes.EntityImage},
		vectorDef,
	}
}

// TestSchemaRegistersEveryIndexKind registers one namespace per index kind in
// a single schema, checks number and name uniqueness, and round-trips the
// schema through MarshalBinary/UnmarshalBinary.
func TestSchemaRegistersEveryIndexKind(t *testing.T) {
	defs := integrationDefs(t)
	schema := schematypes.NewDefinitions()
	if err := schema.RegisterAll(defs...); err != nil {
		t.Fatalf("RegisterAll: %v", err)
	}
	if schema.Len() != len(defs) {
		t.Fatalf("Len = %d, want %d", schema.Len(), len(defs))
	}

	// A repeated namespace number and a repeated name each fail with their
	// sentinel error.
	if err := schema.Register(schematypes.NamespaceDef{
		Namespace: schematypes.NamespaceTag, Name: "tag-duplicate", Kind: schematypes.KindBitmapEnum,
	}); !errors.Is(err, schematypes.ErrNamespaceDuplicate) {
		t.Fatalf("duplicate number err = %v, want ErrNamespaceDuplicate", err)
	}
	if err := schema.Register(schematypes.NamespaceDef{
		Namespace: 250, Name: "tag", Kind: schematypes.KindBitmapEnum,
	}); !errors.Is(err, schematypes.ErrNamespaceName) {
		t.Fatalf("duplicate name err = %v, want ErrNamespaceName", err)
	}

	// Round-trip the schema and resolve every definition by both number and
	// name.
	data, err := schema.MarshalBinary()
	if err != nil {
		t.Fatalf("MarshalBinary: %v", err)
	}
	var restored schematypes.Definitions
	if err := restored.UnmarshalBinary(data); err != nil {
		t.Fatalf("UnmarshalBinary: %v", err)
	}
	if restored.Len() != len(defs) {
		t.Fatalf("restored Len = %d, want %d", restored.Len(), len(defs))
	}
	for _, want := range defs {
		byID, ok := restored.Lookup(want.Namespace)
		if !ok || byID.Name != want.Name || byID.Kind != want.Kind {
			t.Fatalf("Lookup(%d) = %+v, %v; want name %q kind %v", want.Namespace, byID, ok, want.Name, want.Kind)
		}
		byName, ok := restored.LookupName(want.Name)
		if !ok || byName.Namespace != want.Namespace || byName.Kind != want.Kind {
			t.Fatalf("LookupName(%q) = %+v, %v; want namespace %d kind %v", want.Name, byName, ok, want.Namespace, want.Kind)
		}
	}

	// The bitmap definition survives the schema round-trip through its own
	// inverse converter, proving the opaque Config bytes stay kind-valid.
	bitmapDef, ok := restored.Lookup(schematypes.NamespaceTag)
	if !ok {
		t.Fatal("restored schema lost the tag namespace")
	}
	spec, err := bitmapidx.NamespaceSpecFromSchemaDef(bitmapDef)
	if err != nil {
		t.Fatalf("NamespaceSpecFromSchemaDef: %v", err)
	}
	if spec.Namespace != schematypes.NamespaceTag || spec.Kind != schematypes.KindBitmapEnum {
		t.Fatalf("restored bitmap spec = %+v", spec)
	}
}

// TestIndexesOpenWithSchemaNamespaces opens one in-memory index per package
// with its registered namespace and verifies Metadata/config reports it.
// The unified Store routes the exact and perceptual hash namespaces to the
// current radixhash and simhash engines.
func TestIndexesOpenWithSchemaNamespaces(t *testing.T) {
	tagSpec := bitmapidx.NamespaceSpec{
		Namespace: schematypes.NamespaceTag,
		Kind:      schematypes.KindBitmapEnum,
	}
	tags, err := bitmapidx.NewMemory(tagSpec, bitmapidx.NamespaceConfig{})
	if err != nil {
		t.Fatalf("NewMemory: %v", err)
	}
	defer func() {
		if err := tags.Close(); err != nil {
			t.Errorf("tags.Close: %v", err)
		}
	}()
	if got := tags.Metadata().Spec.Namespace; got != schematypes.NamespaceTag {
		t.Fatalf("bitmap Metadata namespace = %d, want %d", got, schematypes.NamespaceTag)
	}
	if got := tags.Spec().Namespace; got != schematypes.NamespaceTag {
		t.Fatalf("bitmap Spec namespace = %d, want %d", got, schematypes.NamespaceTag)
	}

	timeline, err := timeidx.NewMemory(timeidx.Spec{
		Namespace: timelineNamespace,
		Name:      "timeline",
		Entries:   []timeidx.Entry{{ImageID: 100, Timestamp: 1 << timeidx.TimestampShift}},
	}, timeidx.Config{})
	if err != nil {
		t.Fatalf("timeidx.NewMemory: %v", err)
	}
	defer func() {
		if err := timeline.Close(); err != nil {
			t.Errorf("timeline.Close: %v", err)
		}
	}()
	if got := timeline.Metadata().Namespace; got != timelineNamespace {
		t.Fatalf("timeidx Metadata namespace = %d, want %d", got, timelineNamespace)
	}

	hashSchema := schematypes.NewDefinitions()
	if err := hashSchema.RegisterAll(
		schematypes.NamespaceDef{Namespace: exactHashNamespace, Name: "exact-content", Kind: schematypes.KindHashExact, Entity: schematypes.EntityImage},
		schematypes.NamespaceDef{Namespace: perceptualNamespace, Name: "perceptual", Kind: schematypes.KindHashPerceptual, Entity: schematypes.EntityImage},
	); err != nil {
		t.Fatalf("register hash namespaces: %v", err)
	}
	hashStore, err := index.Create(t.TempDir(), index.Spec{Schema: hashSchema})
	if err != nil {
		t.Fatalf("index.Create: %v", err)
	}
	defer func() {
		if err := hashStore.Close(); err != nil {
			t.Errorf("hashStore.Close: %v", err)
		}
	}()
	if _, ok := hashStore.Sim(perceptualNamespace); !ok {
		t.Fatalf("simhash namespace %d unavailable", perceptualNamespace)
	}
	if _, ok := hashStore.Hash(exactHashNamespace); !ok {
		t.Fatalf("radixhash namespace %d unavailable", exactHashNamespace)
	}

	vectors, err := vectoridx.NewMemory(vectoridx.Config{
		Namespace:   vectorNamespace,
		Dimensions:  vectoridx.Dim512,
		ElementType: vectoridx.ElementFloat32,
	})
	if err != nil {
		t.Fatalf("vectoridx.NewMemory: %v", err)
	}
	defer func() {
		if err := vectors.Close(); err != nil {
			t.Errorf("vectors.Close: %v", err)
		}
	}()
	if got := vectors.Config().Namespace; got != vectorNamespace {
		t.Fatalf("vector Config namespace = %d, want %d", got, vectorNamespace)
	}
	if got := vectors.Metadata().Namespace; got != vectorNamespace {
		t.Fatalf("vector Metadata namespace = %d, want %d", got, vectorNamespace)
	}
}
