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
