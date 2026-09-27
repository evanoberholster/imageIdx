// Package common_test verifies that the shared identifier aliases and the
// common.Schema namespace vocabulary line up across every index package. It
// is an external test package so the dependency arrow stays common →
// (index packages) → common without an import cycle.
package common_test

import (
	"errors"
	"testing"

	common "github.com/evanoberholster/imageIdx/common"
	"github.com/evanoberholster/imageIdx/index/bitmapidx"
	hashidx "github.com/evanoberholster/imageIdx/index/hashIdx"
	"github.com/evanoberholster/imageIdx/index/timeidx"
	"github.com/evanoberholster/imageIdx/index/vectoridx"
)

// Namespace numbers used by this test. They are intentionally outside the
// well-known bitmap range and unique within the test schema.
const (
	timelineNamespace   common.Namespace = 90
	exactHashNamespace  common.Namespace = 100
	perceptualNamespace common.Namespace = 101
	vectorNamespace     common.Namespace = 102
)

// integrationDefs builds one NamespaceDef per index kind: bitmap, timestamp,
// exact hash, perceptual hash, and vector.
func integrationDefs(t *testing.T) []common.NamespaceDef {
	t.Helper()

	bitmapDef, err := bitmapidx.NamespaceSpec{
		Namespace: common.NamespaceTag,
		Name:      "tag",
		Kind:      common.KindBitmapEnum,
		Storage:   bitmapidx.StoragePerType,
	}.SchemaDef()
	if err != nil {
		t.Fatalf("bitmap SchemaDef: %v", err)
	}

	vectorDef := vectoridx.Config{Namespace: vectorNamespace}.NamespaceDef()
	vectorDef.Name = "embedding"

	return []common.NamespaceDef{
		bitmapDef,
		timeidx.Spec{Namespace: timelineNamespace, Name: "timeline"}.NamespaceDef(),
		hashidx.ExactSpec{Namespace: exactHashNamespace, Name: "exact-content"}.NamespaceDef(),
		hashidx.PerceptualSpec{Namespace: perceptualNamespace, Name: "perceptual"}.NamespaceDef(),
		vectorDef,
	}
}

// TestSchemaRegistersEveryIndexKind registers one namespace per index kind in
// a single schema, checks number and name uniqueness, and round-trips the
// schema through MarshalBinary/UnmarshalBinary.
func TestSchemaRegistersEveryIndexKind(t *testing.T) {
	defs := integrationDefs(t)
	schema := common.NewSchema()
	if err := schema.RegisterAll(defs...); err != nil {
		t.Fatalf("RegisterAll: %v", err)
	}
	if schema.Len() != len(defs) {
		t.Fatalf("Len = %d, want %d", schema.Len(), len(defs))
	}

	// A repeated namespace number and a repeated name each fail with their
	// sentinel error.
	if err := schema.Register(common.NamespaceDef{
		Namespace: common.NamespaceTag, Name: "tag-duplicate", Kind: common.KindBitmapEnum,
	}); !errors.Is(err, common.ErrNamespaceDuplicate) {
		t.Fatalf("duplicate number err = %v, want ErrNamespaceDuplicate", err)
	}
	if err := schema.Register(common.NamespaceDef{
		Namespace: 250, Name: "tag", Kind: common.KindBitmapEnum,
	}); !errors.Is(err, common.ErrNamespaceName) {
		t.Fatalf("duplicate name err = %v, want ErrNamespaceName", err)
	}

	// Round-trip the schema and resolve every definition by both number and
	// name.
	data, err := schema.MarshalBinary()
	if err != nil {
		t.Fatalf("MarshalBinary: %v", err)
	}
	var restored common.Schema
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
	bitmapDef, ok := restored.Lookup(common.NamespaceTag)
	if !ok {
		t.Fatal("restored schema lost the tag namespace")
	}
	spec, err := bitmapidx.NamespaceSpecFromSchemaDef(bitmapDef)
	if err != nil {
		t.Fatalf("NamespaceSpecFromSchemaDef: %v", err)
	}
	if spec.Namespace != common.NamespaceTag || spec.Kind != common.KindBitmapEnum || spec.Storage != bitmapidx.StoragePerType {
		t.Fatalf("restored bitmap spec = %+v", spec)
	}
}

// TestIndexesOpenWithSchemaNamespaces opens one in-memory index per package
// with its registered namespace and verifies Metadata/config reports it.
// hashidx has no exported in-memory constructor, so its Store is opened in a
// t.TempDir; nothing else touches disk.
func TestIndexesOpenWithSchemaNamespaces(t *testing.T) {
	tagSpec := bitmapidx.NamespaceSpec{
		Namespace: common.NamespaceTag,
		Name:      "tag",
		Kind:      common.KindBitmapEnum,
		Storage:   bitmapidx.StoragePerType,
	}
	tags, err := bitmapidx.NewNamespaceIndex(tagSpec, bitmapidx.NamespaceConfig{})
	if err != nil {
		t.Fatalf("NewNamespaceIndex: %v", err)
	}
	defer func() {
		if err := tags.Close(); err != nil {
			t.Errorf("tags.Close: %v", err)
		}
	}()
	if got := tags.Metadata().Spec.Namespace; got != common.NamespaceTag {
		t.Fatalf("bitmap Metadata namespace = %d, want %d", got, common.NamespaceTag)
	}
	if got := tags.Spec().Namespace; got != common.NamespaceTag {
		t.Fatalf("bitmap Spec namespace = %d, want %d", got, common.NamespaceTag)
	}

	timeline, err := timeidx.New(timeidx.Spec{
		Namespace: timelineNamespace,
		Name:      "timeline",
		Entries:   []timeidx.Entry{{ImageID: 100, Timestamp: 1 << timeidx.TimestampShift}},
	}, timeidx.Config{})
	if err != nil {
		t.Fatalf("timeidx.New: %v", err)
	}
	defer func() {
		if err := timeline.Close(); err != nil {
			t.Errorf("timeline.Close: %v", err)
		}
	}()
	if got := timeline.Metadata().Namespace; got != timelineNamespace {
		t.Fatalf("timeidx Metadata namespace = %d, want %d", got, timelineNamespace)
	}

	hashStore, err := hashidx.NewStore(t.TempDir(), hashidx.StoreSpec{
		Perceptual: hashidx.PerceptualSpec{Namespace: perceptualNamespace, Name: "perceptual"},
		Exact:      hashidx.ExactSpec{Namespace: exactHashNamespace, Name: "exact-content", Algorithm: hashidx.AlgorithmBLAKE3},
	}, common.DurabilityOS)
	if err != nil {
		t.Fatalf("hashidx.NewStore: %v", err)
	}
	defer func() {
		if err := hashStore.Close(); err != nil {
			t.Errorf("hashStore.Close: %v", err)
		}
	}()
	if got := hashStore.Perceptual().Metadata().Namespace; got != perceptualNamespace {
		t.Fatalf("hashidx perceptual namespace = %d, want %d", got, perceptualNamespace)
	}
	if got := hashStore.Exact().Metadata().Namespace; got != exactHashNamespace {
		t.Fatalf("hashidx exact namespace = %d, want %d", got, exactHashNamespace)
	}

	vectors, err := vectoridx.New(vectoridx.Config{
		Namespace:   vectorNamespace,
		Dimensions:  vectoridx.Dim512,
		ElementType: vectoridx.ElementFloat32,
	})
	if err != nil {
		t.Fatalf("vectoridx.New: %v", err)
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
