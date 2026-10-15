package database

import (
	"encoding/binary"
	"errors"
	"hash/crc32"
	"os"
	"path/filepath"
	"testing"

	"github.com/cockroachdb/pebble/v2"
	"github.com/evanoberholster/imageIdx/index"
	"github.com/evanoberholster/imageIdx/schema"
	"github.com/evanoberholster/imageIdx/store"
)

func TestNextIDAbandonedAndReopen(t *testing.T) {
	data, idx := paths(t)
	db := openTest(t, data, idx)
	image, err := db.NextID(schema.EntityImage)
	if image != 1 || err != nil {
		t.Fatalf("image=%d: %v", image, err)
	}
	person, err := db.NextID(schema.EntityPerson)
	if person != 1 || err != nil {
		t.Fatalf("person=%d: %v", person, err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := db.NextID(schema.EntityImage); !errors.Is(err, schema.ErrClosed) {
		t.Fatal(err)
	}
	db = openTest(t, data, idx)
	defer db.Close()
	if id, err := db.NextID(schema.EntityImage); id != 2 || err != nil {
		t.Fatalf("reopen image=%d: %v", id, err)
	}
	if id, err := db.NextID(schema.EntityPerson); id != 2 || err != nil {
		t.Fatalf("reopen person=%d: %v", id, err)
	}
}

func TestCanonicalNamespaceIndependentOfEntityKind(t *testing.T) {
	data, idx := paths(t)
	var options Options
	options.CanonicalNamespaces[schema.EntityImage] = 100
	options.CanonicalNamespaces[schema.EntityFace] = 200
	db, err := Open(data, idx, decodeTest, options)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range []schema.EntityKind{schema.EntityImage, schema.EntityFace} {
		if id, err := db.NextID(e); id != 1 || err != nil {
			t.Fatalf("%v: %d %v", e, id, err)
		}
	}
	if _, err := db.Commit(Mutation{Entity: schema.EntityImage, ValueID: 1, Record: []byte("image"), Projection: projection(1, 'x')}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.values.Get(store.Key{Namespace: 100, ID: 1}); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = Open(data, idx, decodeTest, options)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := db.Get(schema.EntityImage, 1); err != nil || string(got) != "image" {
		t.Fatalf("reopen record: %q %v", got, err)
	}
	if def, ok := db.ids.Lookup(100); !ok || def.Entity != schema.EntityImage {
		t.Fatalf("canonical owner: %+v %v", def, ok)
	}
	if def, ok := db.ids.Lookup(101); !ok || def.Entity != schema.EntityImage {
		t.Fatalf("index owner: %+v %v", def, ok)
	}
	if id, err := db.NextID(schema.EntityImage); id != 2 || err != nil {
		t.Fatalf("reopen image: %d %v", id, err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = Open(data, idx, decodeTest, Options{})
	if err != nil {
		t.Fatalf("persisted canonical assignment not loaded: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	var wrong Options
	wrong.CanonicalNamespaces[schema.EntityImage] = 1
	if _, err := Open(data, idx, decodeTest, wrong); err == nil {
		t.Fatal("accepted reassigned canonical namespace")
	}
}

func TestOpenRejectsCollidingCanonicalAndIndexNamespaces(t *testing.T) {
	root := t.TempDir()
	data, idxPath := filepath.Join(root, "data"), filepath.Join(root, "index")
	defs := schema.NewDefinitions()
	if err := defs.Register(schema.NamespaceDef{Namespace: schema.Namespace(schema.EntityImage), Name: "secondary", Kind: schema.KindTimestamp, Entity: schema.EntityImage}); err != nil {
		t.Fatal(err)
	}
	idx, err := index.Create(idxPath, index.Spec{Schema: defs})
	if err != nil {
		t.Fatal(err)
	}
	if err := idx.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := Open(data, idxPath, decodeTest, Options{})
	if db != nil {
		_ = db.Close()
		t.Fatal("accepted colliding namespace")
	}
	if !errors.Is(err, schema.ErrNamespaceDuplicate) {
		t.Fatalf("collision error: %v", err)
	}
}

func TestPersistedCentralSchemaRejectsDifferentIndexCopy(t *testing.T) {
	data, idxPath := paths(t)
	db := openTest(t, data, idxPath)
	if _, err := db.NextID(schema.EntityImage); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	otherPath := filepath.Join(t.TempDir(), "different-index")
	defs := schema.NewDefinitions()
	if err := defs.Register(schema.NamespaceDef{Namespace: 101, Name: "different", Kind: schema.KindTimestamp, Entity: schema.EntityImage}); err != nil {
		t.Fatal(err)
	}
	other, err := index.Create(otherPath, index.Spec{Schema: defs})
	if err != nil {
		t.Fatal(err)
	}
	if err := other.Close(); err != nil {
		t.Fatal(err)
	}
	if db, err := Open(data, otherPath, decodeTest, Options{}); db != nil || !errors.Is(err, schema.ErrNamespace) {
		if db != nil {
			_ = db.Close()
		}
		t.Fatalf("accepted conflicting index schema: %v", err)
	}
	db = openTest(t, data, idxPath)
	defer db.Close()
	if id, err := db.NextID(schema.EntityImage); err != nil || id != 2 {
		t.Fatalf("original identity lost: %d %v", id, err)
	}
}

func TestBootstrapDoesNotPublishSchemaWithConflictingBackup(t *testing.T) {
	data, idxPath := paths(t)
	kv, err := store.Open(data, canonicalSpecs()...)
	if err != nil {
		t.Fatal(err)
	}
	if err := kv.Set(store.Key{Namespace: metadataNS, ID: schemaKeyID}, []byte("broken")); err != nil {
		t.Fatal(err)
	}
	if err := kv.Close(); err != nil {
		t.Fatal(err)
	}
	if db, err := Open(data, idxPath, decodeTest, Options{}); db != nil || !errors.Is(err, ErrRecoveryRequired) {
		if db != nil {
			_ = db.Close()
		}
		t.Fatalf("accepted invalid backup: %v", err)
	}
	if _, err := os.Stat(filepath.Join(data, "schema.dat")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("published central schema before validating backup: %v", err)
	}
}

func TestExplicitAuthoritativeDefinitionsOnCreation(t *testing.T) {
	data, idxPath := paths(t)
	idx, err := index.Open(idxPath)
	if err != nil {
		t.Fatal(err)
	}
	defs := schema.NewDefinitions()
	for _, spec := range canonicalSpecs() {
		if spec.Entity.Valid() {
			if err := defs.Register(spec.NamespaceDef()); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, def := range idx.Schema().Defs() {
		if err := defs.Register(def); err != nil {
			t.Fatal(err)
		}
	}
	if err := idx.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := Open(data, idxPath, decodeTest, Options{Definitions: defs})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db = openTest(t, data, idxPath)
	if _, ok := db.ids.Lookup(tagNS); !ok {
		t.Fatal("lost authoritative index namespace")
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	wrong := schema.NewDefinitions()
	_ = wrong.Register(schema.NamespaceDef{Namespace: 100, Name: "wrong", Kind: schema.KindKV, Entity: schema.EntityImage})
	if _, err := Open(data, idxPath, decodeTest, Options{Definitions: wrong}); !errors.Is(err, schema.ErrNamespace) {
		t.Fatalf("accepted different authoritative definitions: %v", err)
	}
}

func TestLegacyMigrationIncludesTombstones(t *testing.T) {
	data, idx := paths(t)
	values, err := store.Open(data, canonicalSpecs()...)
	if err != nil {
		t.Fatal(err)
	}
	if err := values.Set(store.Key{Namespace: schema.Namespace(schema.EntityImage), ID: 4099}, []byte{1, 0, 0, 0, 0}); err != nil {
		t.Fatal(err)
	}
	if err := values.Close(); err != nil {
		t.Fatal(err)
	}
	db := openTest(t, data, idx)
	defer db.Close()
	if id, err := db.NextID(schema.EntityImage); id != 4100 || err != nil {
		t.Fatalf("migrated image=%d: %v", id, err)
	}
}

func TestCommitReconcilesExternalID(t *testing.T) {
	data, idx := paths(t)
	db := openTest(t, data, idx)
	gen, err := db.Commit(Mutation{Entity: schema.EntityImage, ValueID: 5200, Projection: projection(5200, 'a')})
	if err != nil {
		t.Fatal(err)
	}
	wait(t, db, gen)
	if id, err := db.NextID(schema.EntityImage); id != 5201 || err != nil {
		t.Fatalf("issued committed external ID: %d, %v", id, err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db = openTest(t, data, idx)
	defer db.Close()
	if id, err := db.NextID(schema.EntityImage); id != 5202 || err != nil {
		t.Fatalf("reopen issued old ID: %d, %v", id, err)
	}
}

func TestExplicitIDCannotRevokePreviouslyIssuedID(t *testing.T) {
	data, idx := paths(t)
	db := openTest(t, data, idx)
	defer db.Close()
	issued := make(chan schema.ValueID, 1)
	go func() { id, _ := db.NextID(schema.EntityImage); issued <- id }()
	id := <-issued
	if id != 1 {
		t.Fatalf("first issued ID: %d", id)
	}
	// This is legal last-writer-wins record creation, but proves EnsureMinimum
	// cannot promise to prevent collision with an outstanding issued ID.
	if _, err := db.Commit(Mutation{Entity: schema.EntityImage, ValueID: id, Projection: projection(id, 'e')}); err != nil {
		t.Fatal(err)
	}
	if next, err := db.NextID(schema.EntityImage); err != nil || next != id+1 {
		t.Fatalf("future ID: %d %v", next, err)
	}
}

func TestPrepareImportBeforeAllocation(t *testing.T) {
	data, idx := paths(t)
	db := openTest(t, data, idx)
	if err := db.PrepareImport(schema.EntityImage, 5000); err != nil {
		t.Fatal(err)
	}
	if image, err := db.NextID(schema.EntityImage); err != nil || image != 5001 {
		t.Fatalf("import floor: %d %v", image, err)
	}
	if person, err := db.NextID(schema.EntityPerson); err != nil || person != 1 {
		t.Fatalf("independent entity: %d %v", person, err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db = openTest(t, data, idx)
	defer db.Close()
	if image, err := db.NextID(schema.EntityImage); err != nil || image != 5002 {
		t.Fatalf("reopen import floor: %d %v", image, err)
	}
}

func TestLegacyCanonicalSchemaExtendsForNewKinds(t *testing.T) {
	data, idx := paths(t)
	legacy := canonicalSpecs()[:12] // ten historical kinds and two internal namespaces
	for i := range legacy {
		legacy[i].Entity = schema.EntityInvalid // pre-entity KV schema
	}
	values, err := store.Open(data, legacy...)
	if err != nil {
		t.Fatal(err)
	}
	if err := values.Close(); err != nil {
		t.Fatal(err)
	}
	db := openTest(t, data, idx)
	for _, kind := range []schema.EntityKind{schema.EntityUser, schema.EntityLocation, schema.EntityTag} {
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
	if id, err := db.NextID(schema.EntityUser); id != 2 || err != nil {
		t.Fatalf("user reopen: %d, %v", id, err)
	}
}

func TestMigratesLegacyDurableHighWater(t *testing.T) {
	data, idx := paths(t)
	values, err := store.Open(data, canonicalSpecs()...)
	if err != nil {
		t.Fatal(err)
	}
	if err := values.Close(); err != nil {
		t.Fatal(err)
	}
	// Old allocator reserved 1..6000 although no image record committed.
	pdb, err := pebble.Open(data, nil)
	if err != nil {
		t.Fatal(err)
	}
	batch := pdb.NewBatch()
	witness := store.EncodeKey(store.Key{Namespace: 0, ID: 1})
	var key [11]byte
	copy(key[:], []byte{0, 0, 'i', 'd', 'a', 'l', 'l', 'o', 'c', '/'})
	key[10] = 0
	if err := batch.Set(key[:], []byte{2, byte(schema.EntityKindCount)}, nil); err != nil {
		t.Fatal(err)
	}
	if err := batch.Set(witness[:], []byte{2}, nil); err != nil {
		t.Fatal(err)
	}
	for n := 1; n <= schema.EntityKindCount; n++ {
		key[10] = byte(n)
		var raw [14]byte
		raw[0], raw[1] = 2, byte(n)
		if n == int(schema.EntityImage) {
			binary.BigEndian.PutUint64(raw[2:10], 6000)
		}
		binary.BigEndian.PutUint32(raw[10:], crc32.ChecksumIEEE(raw[:10]))
		if err := batch.Set(key[:], raw[:], nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := batch.Commit(pebble.Sync); err != nil {
		t.Fatal(err)
	}
	if err := batch.Close(); err != nil {
		t.Fatal(err)
	}
	if err := pdb.Close(); err != nil {
		t.Fatal(err)
	}
	db := openTest(t, data, idx)
	if id, err := db.NextID(schema.EntityImage); id != 6001 || err != nil {
		t.Fatalf("legacy reservation reused: %d %v", id, err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db = openTest(t, data, idx)
	defer db.Close()
	if id, err := db.NextID(schema.EntityImage); id != 6002 || err != nil {
		t.Fatalf("reopen: %d %v", id, err)
	}
	// Losing the authoritative single-file state must never reset from the
	// (now removed) legacy metadata or surviving canonical records.
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(data, "schema.dat")); err != nil {
		t.Fatal(err)
	}
	if reopened, err := Open(data, idx, decodeTest, Options{}); !errors.Is(err, schema.ErrCorrupt) {
		if reopened != nil {
			reopened.Close()
		}
		t.Fatalf("missing schema reset: %v", err)
	}
}
