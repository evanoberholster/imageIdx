package database

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	"github.com/evanoberholster/imageIdx/index"
	"github.com/evanoberholster/imageIdx/index/simhash"
	schema "github.com/evanoberholster/imageIdx/schema"
)

func sparseSimPaths(t *testing.T) (string, string) {
	t.Helper()
	root := t.TempDir()
	data, idxPath := filepath.Join(root, "data"), filepath.Join(root, "index")
	defs := schema.NewDefinitions()
	if err := defs.Register(schema.NamespaceDef{
		Namespace: 300, Name: "perceptual", Kind: schema.KindHashPerceptual, Entity: schema.EntityImage,
	}); err != nil {
		t.Fatal(err)
	}
	idx, err := index.Create(idxPath, index.Spec{Schema: defs})
	if err != nil {
		t.Fatal(err)
	}
	if err := idx.Close(); err != nil {
		t.Fatal(err)
	}
	return data, idxPath
}

func decodeSparseSim(p []byte) (index.Entry, error) {
	if len(p) != 16 {
		return index.Entry{}, ErrCorruptJournal
	}
	return index.Entry{
		ValueID: binary.BigEndian.Uint64(p[:8]),
		Sims:    []index.SimValue{{Namespace: 300, Record: simhashRecordForTest(binary.BigEndian.Uint64(p[8:16]))}},
	}, nil
}

func simhashRecordForTest(pHash uint64) simhash.Record {
	return simhash.Record{PHash: pHash}
}

func TestSparseSimHashDatabaseReopenAndRebuild(t *testing.T) {
	data, idxPath := sparseSimPaths(t)
	projection := make([]byte, 16)
	binary.BigEndian.PutUint64(projection[:8], 1<<32+1)
	binary.BigEndian.PutUint64(projection[8:16], 77)

	db, err := Open(data, idxPath, decodeSparseSim, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Commit(Mutation{Entity: schema.EntityImage, ValueID: 1<<32 + 1, Projection: projection}); err != nil {
		t.Fatal(err)
	}
	if err := db.WaitIndexed(t.Context(), 1); err != nil {
		t.Fatal(err)
	}
	assertSparseSim(t, db)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	db, err = Open(data, idxPath, decodeSparseSim, Options{})
	if err != nil {
		t.Fatal(err)
	}
	assertSparseSim(t, db)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	if err := os.Remove(filepath.Join(idxPath, "manifest")); err != nil {
		t.Fatal(err)
	}
	db, err = Open(data, idxPath, decodeSparseSim, Options{Recovery: RecoveryAutomatic})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	assertSparseSim(t, db)
}

func assertSparseSim(t *testing.T, db *DB) {
	t.Helper()
	sim, ok := db.Index().Sim(300)
	if !ok {
		t.Fatal("simhash index missing")
	}
	got := sim.SearchPHash(nil, 77, 0)
	if len(got) != 1 || got[0].ImageID != 1<<32+1 {
		t.Fatalf("sparse simhash = %+v", got)
	}
}
