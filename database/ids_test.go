package database

import (
	"errors"
	"testing"

	"github.com/evanoberholster/imageIdx/common"
	"github.com/evanoberholster/imageIdx/index/idalloc"
	"github.com/evanoberholster/imageIdx/store"
)

func TestNextIDAbandonedAndReopen(t *testing.T) {
	data, idx := paths(t)
	db := openTest(t, data, idx)
	image, err := db.NextID(common.EntityImage)
	if image != 1 || err != nil {
		t.Fatalf("image=%d: %v", image, err)
	}
	person, err := db.NextID(common.EntityPerson)
	if person != 1 || err != nil {
		t.Fatalf("person=%d: %v", person, err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := db.NextID(common.EntityImage); !errors.Is(err, idalloc.ErrClosed) {
		t.Fatal(err)
	}
	db = openTest(t, data, idx)
	defer db.Close()
	if id, err := db.NextID(common.EntityImage); id != 1001 || err != nil {
		t.Fatalf("reopen image=%d: %v", id, err)
	}
	if id, err := db.NextID(common.EntityPerson); id != 1001 || err != nil {
		t.Fatalf("reopen person=%d: %v", id, err)
	}
}

func TestLegacyMigrationIncludesTombstones(t *testing.T) {
	data, idx := paths(t)
	values, err := store.Open(data, canonicalSpecs()...)
	if err != nil {
		t.Fatal(err)
	}
	if err := values.Set(store.Key{Namespace: common.Namespace(common.EntityImage), ID: 4099}, []byte{1, 0, 0, 0, 0}); err != nil {
		t.Fatal(err)
	}
	if err := values.Close(); err != nil {
		t.Fatal(err)
	}
	db := openTest(t, data, idx)
	defer db.Close()
	if id, err := db.NextID(common.EntityImage); id != 4100 || err != nil {
		t.Fatalf("migrated image=%d: %v", id, err)
	}
}

func TestCommitReconcilesExternalID(t *testing.T) {
	data, idx := paths(t)
	db := openTest(t, data, idx)
	gen, err := db.Commit(Mutation{Entity: common.EntityImage, ValueID: 5200, Projection: projection(5200, 'a')})
	if err != nil {
		t.Fatal(err)
	}
	wait(t, db, gen)
	if id, err := db.NextID(common.EntityImage); id != 5201 || err != nil {
		t.Fatalf("issued committed external ID: %d, %v", id, err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db = openTest(t, data, idx)
	defer db.Close()
	if id, err := db.NextID(common.EntityImage); id != 6201 || err != nil {
		t.Fatalf("reopen issued old ID: %d, %v", id, err)
	}
}

func TestLegacyCanonicalSchemaExtendsForNewKinds(t *testing.T) {
	data, idx := paths(t)
	legacy := canonicalSpecs()[:12] // ten historical kinds and two internal namespaces
	for i := range legacy {
		legacy[i].Entity = common.EntityInvalid // pre-entity KV schema
	}
	values, err := store.Open(data, legacy...)
	if err != nil {
		t.Fatal(err)
	}
	if err := values.Close(); err != nil {
		t.Fatal(err)
	}
	db := openTest(t, data, idx)
	for _, kind := range []common.EntityKind{common.EntityUser, common.EntityLocation, common.EntityTag} {
		id, err := db.NextID(kind)
		if id != 1 || err != nil {
			t.Fatalf("%s: %d, %v", kind, id, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db = openTest(t, data, idx)
	defer db.Close()
	if id, err := db.NextID(common.EntityUser); id != 1001 || err != nil {
		t.Fatalf("user reopen: %d, %v", id, err)
	}
}
