package database

import (
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/evanoberholster/imageIdx/common"
	"github.com/evanoberholster/imageIdx/index"
	"github.com/evanoberholster/imageIdx/index/bitmapidx"
	"github.com/evanoberholster/imageIdx/index/radixhash"
	"github.com/evanoberholster/imageIdx/index/simhash"
	"github.com/evanoberholster/imageIdx/store"
)

func TestAutomaticRebuildSharedBitmapStore(t *testing.T) {
	data, dir := paths(t)
	db := openTest(t, data, dir)
	for id := uint64(1); id <= 4; id++ {
		g, err := db.Commit(Mutation{Entity: common.EntityImage, ValueID: id, Record: []byte("canonical"), Projection: projection(id, byte('a'+id))})
		if err != nil {
			t.Fatal(err)
		}
		wait(t, db, g)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	// bitmapidx persists all bitmap namespaces in one physical segment.
	// Losing its manifest makes both bitmap namespaces unavailable.
	if err := os.Remove(filepath.Join(dir, "manifest")); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(data, dir, decodeTest, Options{}); !errors.Is(err, ErrRecoveryRequired) {
		t.Fatalf("manual open: %v", err)
	}
	db, err := Open(data, dir, decodeTest, Options{Recovery: RecoveryAutomatic})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if db.IndexedGeneration() != 4 || db.Index().SourceGeneration() != 4 {
		t.Fatalf("generation %d / %d", db.IndexedGeneration(), db.Index().SourceGeneration())
	}
	for id := uint64(1); id <= 4; id++ {
		got, err := db.Get(common.EntityImage, id)
		if err != nil || string(got) != "canonical" {
			t.Fatalf("record %d: %q %v", id, got, err)
		}
		contains(t, db, id, byte('a'+id), true)
	}
	if len(db.Index().RecoveryNamespaces()) != 0 {
		t.Fatal("rebuild left recovery marker")
	}
	if _, err := os.Stat(activationPath(dir)); !os.IsNotExist(err) {
		t.Fatalf("activation marker left: %v", err)
	}
	if _, err := os.Stat(backupPath(dir)); !os.IsNotExist(err) {
		t.Fatalf("obsolete generation left: %v", err)
	}
}

func TestRebuildCopiesBorrowedProjectionBytesAcrossBatch(t *testing.T) {
	data, dir := paths(t)
	db := openTest(t, data, dir)
	mutations := make([]Mutation, 64)
	for i := range mutations {
		id := uint64(i + 1)
		mutations[i] = Mutation{Entity: common.EntityImage, ValueID: id, Projection: projection(id, byte(i+1))}
	}
	g, err := db.Commit(mutations...)
	if err != nil {
		t.Fatal(err)
	}
	wait(t, db, g)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "manifest")); err != nil {
		t.Fatal(err)
	}
	db, err = Open(data, dir, decodeTest, Options{Recovery: RecoveryAutomatic})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for i := range mutations {
		contains(t, db, uint64(i+1), byte(i+1), true)
	}
}

func TestAutomaticRebuildCorruptJournalIndependent(t *testing.T) {
	data, dir := paths(t)
	db := openTest(t, data, dir)
	g, err := db.Commit(Mutation{Entity: common.EntityImage, ValueID: 1, Projection: projection(1, 'a')})
	if err != nil {
		t.Fatal(err)
	}
	wait(t, db, g)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	// Recover from authoritative records even if acknowledged journal data is
	// already reclaimed. A corrupt/missing bitmap store cannot be replayed.
	if err := os.Remove(filepath.Join(dir, "manifest")); err != nil {
		t.Fatal(err)
	}
	db, err = Open(data, dir, decodeTest, Options{Recovery: RecoveryAutomatic})
	if err != nil {
		t.Fatal(err)
	}
	contains(t, db, 1, 'a', true)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db = openTest(t, data, dir)
	defer db.Close()
	contains(t, db, 1, 'a', true)
}

func TestRecoveryKeepsCanonicalWhenProjectionInvalid(t *testing.T) {
	data, dir := paths(t)
	db := openTest(t, data, dir)
	g, err := db.Commit(Mutation{Entity: common.EntityImage, ValueID: 1, Projection: projection(1, 'x'), Record: []byte("untouched")})
	if err != nil {
		t.Fatal(err)
	}
	wait(t, db, g)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "manifest")); err != nil {
		t.Fatal(err)
	}
	_, err = Open(data, dir, func([]byte) (index.Entry, error) { return index.Entry{}, ErrCorruptJournal }, Options{Recovery: RecoveryAutomatic})
	if !errors.Is(err, ErrRecoveryRequired) {
		t.Fatalf("invalid projection: %v", err)
	}
	// A partial staging directory is owned by the BUILDING marker, so the
	// next Open safely discards it and starts again.
	db, err = Open(data, dir, decodeTest, Options{Recovery: RecoveryAutomatic})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	got, err := db.Get(common.EntityImage, 1)
	if err != nil || string(got) != "untouched" {
		t.Fatalf("canonical record changed: %q %v", got, err)
	}
	contains(t, db, 1, 'x', true)
}

func TestRecoveryGenerationMismatch(t *testing.T) {
	data, dir := paths(t)
	db := openTest(t, data, dir)
	g, err := db.Commit(Mutation{Entity: common.EntityImage, ValueID: 1, Projection: projection(1, 'a')})
	if err != nil {
		t.Fatal(err)
	}
	wait(t, db, g)
	// Simulate an interrupted batch intent with no durable index generation
	// beyond the canonical generation; it is not safe to ignore until rebuilt.
	b := db.values.NewBatch()
	if err := putGeneration(b, 3, g); err != nil {
		t.Fatal(err)
	}
	if err := b.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	// The index watermark already proves this generation; no rebuild is needed.
	db, err = Open(data, dir, decodeTest, Options{Recovery: RecoveryAutomatic})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if db.IndexedGeneration() != g {
		t.Fatal("ack changed")
	}
}

func TestMissingIndexSchemaRequiresExplicitRecovery(t *testing.T) {
	data, dir := paths(t)
	db := openTest(t, data, dir)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	// Once the canonical schema has been persisted, recovery can reconstruct
	// even when the entire derived index directory is missing.
	if err := os.Rename(dir, dir+".removed-by-test"); err != nil {
		t.Fatal(err)
	}
	db, err := Open(data, dir, decodeTest, Options{Recovery: RecoveryAutomatic})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if db.Index().Schema().Len() != 2 {
		t.Fatal("missing reconstructed schema")
	}
	if _, err := os.Stat(dir + ".removed-by-test"); err != nil {
		t.Fatal(err)
	}
}

func TestCanonicalSchemaMismatchStopsActivation(t *testing.T) {
	data, dir := paths(t)
	db := openTest(t, data, dir)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	kvSpecs := canonicalSpecs()
	kv, err := store.Open(data, kvSpecs...)
	if err != nil {
		t.Fatal(err)
	}
	if err := kv.Set(store.Key{Namespace: metadataNS, ID: schemaKeyID}, []byte("broken")); err != nil {
		t.Fatal(err)
	}
	if err := kv.Close(); err != nil {
		t.Fatal(err)
	}
	_, err = Open(data, dir, decodeTest, Options{Recovery: RecoveryAutomatic})
	if !errors.Is(err, ErrRecoveryRequired) {
		t.Fatalf("invalid schema: %v", err)
	}
}

func TestSparseSimHashRebuildFailsExplicitly(t *testing.T) {
	root := t.TempDir()
	data, dir := filepath.Join(root, "data"), filepath.Join(root, "index")
	schema := common.NewSchema()
	if err := schema.Register(common.NamespaceDef{Namespace: 141, Name: "perceptual", Kind: common.KindHashPerceptual, Entity: common.EntityImage}); err != nil {
		t.Fatal(err)
	}
	idx, err := index.Create(dir, index.Spec{Schema: schema})
	if err != nil {
		t.Fatal(err)
	}
	if err := idx.Close(); err != nil {
		t.Fatal(err)
	}
	decode := func(b []byte) (index.Entry, error) {
		if len(b) != 1 {
			return index.Entry{}, ErrCorruptJournal
		}
		return index.Entry{ValueID: uint64(b[0]), Sims: []index.SimValue{{Namespace: 141, Record: simhash.Record{PHash: 42}}}}, nil
	}
	db, err := Open(data, dir, decode, Options{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Commit(Mutation{Entity: common.EntityImage, ValueID: 2, Projection: []byte{2}})
	if err != nil {
		t.Fatal(err)
	}
	// The canonical mutation is durable despite the index writer rejecting a
	// sparse first simhash ID. Rebuilding must not silently remap it.
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "sim-008d.manifest")); err != nil {
		t.Fatal(err)
	}
	_, err = Open(data, dir, decode, Options{Recovery: RecoveryAutomatic})
	if !errors.Is(err, ErrUnsupportedRebuild) {
		t.Fatalf("sparse simhash: %v", err)
	}
}

func TestRebuildPreservesEntityOwnershipForOverlappingIDs(t *testing.T) {
	root := t.TempDir()
	data, dir := filepath.Join(root, "data"), filepath.Join(root, "index")
	schema := common.NewSchema()
	for _, spec := range []struct {
		ns     common.Namespace
		entity common.EntityKind
	}{{151, common.EntityImage}, {152, common.EntityFace}} {
		def, err := index.BitmapDef(bitmapidx.NamespaceSpec{Namespace: spec.ns, Kind: common.KindBitmapEnum}, spec.entity.String()+"-tag", spec.entity)
		if err != nil {
			t.Fatal(err)
		}
		if err := schema.Register(def); err != nil {
			t.Fatal(err)
		}
	}
	idx, err := index.Create(dir, index.Spec{Schema: schema})
	if err != nil {
		t.Fatal(err)
	}
	if err := idx.Close(); err != nil {
		t.Fatal(err)
	}
	decode := func(p []byte) (index.Entry, error) {
		if len(p) != 11 {
			return index.Entry{}, ErrCorruptJournal
		}
		return index.Entry{ValueID: binary.BigEndian.Uint64(p[:8]), Bitmaps: []index.BitmapValue{{Namespace: common.Namespace(binary.BigEndian.Uint16(p[8:10])), Value: bitmapidx.EnumBytesValue(p[10:11])}}}, nil
	}
	project := func(ns common.Namespace, value byte) []byte {
		p := make([]byte, 11)
		binary.BigEndian.PutUint64(p[:8], 7)
		binary.BigEndian.PutUint16(p[8:10], uint16(ns))
		p[10] = value
		return p
	}
	db, err := Open(data, dir, decode, Options{})
	if err != nil {
		t.Fatal(err)
	}
	g, err := db.Commit(
		Mutation{Entity: common.EntityImage, ValueID: 7, Projection: project(151, 'i')},
		Mutation{Entity: common.EntityFace, ValueID: 7, Projection: project(152, 'f')},
	)
	if err != nil {
		t.Fatal(err)
	}
	wait(t, db, g)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "manifest")); err != nil {
		t.Fatal(err)
	}
	db, err = Open(data, dir, decode, Options{Recovery: RecoveryAutomatic})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, check := range []struct {
		ns              common.Namespace
		present, absent byte
	}{{151, 'i', 'f'}, {152, 'f', 'i'}} {
		idx, ok := db.Index().Bitmap(check.ns)
		if !ok {
			t.Fatalf("namespace %d unavailable", check.ns)
		}
		set, err := idx.MatchValues(bitmapidx.EnumBytesValue([]byte{check.present}))
		if err != nil || !set.Contains(7) {
			t.Fatalf("namespace %d lost entity id 7: %v", check.ns, err)
		}
		other, err := idx.MatchValues(bitmapidx.EnumBytesValue([]byte{check.absent}))
		if err != nil || other.Contains(7) {
			t.Fatalf("namespace %d mixed entity id 7: %v", check.ns, err)
		}
	}
}

func TestRebuildOmitsDeletedAndObsoleteHashAssociations(t *testing.T) {
	root := t.TempDir()
	data, dir := filepath.Join(root, "data"), filepath.Join(root, "index")
	schema := common.NewSchema()
	if err := schema.Register(common.NamespaceDef{Namespace: hashNS, Name: "hash", Kind: common.KindHashExact, Entity: common.EntityImage}); err != nil {
		t.Fatal(err)
	}
	idx, err := index.Create(dir, index.Spec{Schema: schema})
	if err != nil {
		t.Fatal(err)
	}
	if err := idx.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := Open(data, dir, decodeHash, Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, marker := range []byte{'a', 'b'} {
		g, err := db.Commit(Mutation{Entity: common.EntityImage, ValueID: 1, Projection: hashProjection(1, marker)})
		if err != nil {
			t.Fatal(err)
		}
		wait(t, db, g)
	}
	g, err := db.Commit(Mutation{Entity: common.EntityImage, ValueID: 1, Delete: true})
	if err != nil {
		t.Fatal(err)
	}
	wait(t, db, g)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "radix-0067.idx")); err != nil {
		t.Fatal(err)
	}
	db, err = Open(data, dir, decodeHash, Options{Recovery: RecoveryAutomatic})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	engine, ok := db.Index().Hash(hashNS)
	if !ok {
		t.Fatal("hash unavailable")
	}
	for _, marker := range []byte{'a', 'b'} {
		var hash radixhash.Hash
		hash[0] = marker
		if ids := engine.Lookup(nil, hash); len(ids) != 0 {
			t.Fatalf("obsolete hash %q survived rebuild: %v", marker, ids)
		}
	}
	if _, err := db.Get(common.EntityImage, 1); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("deletion lost: %v", err)
	}
}
