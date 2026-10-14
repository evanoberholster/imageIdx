package database

import (
	"encoding/binary"
	"path/filepath"
	"testing"

	"github.com/evanoberholster/imageIdx/index"
	"github.com/evanoberholster/imageIdx/index/radixhash"
	schematypes "github.com/evanoberholster/imageIdx/schema"
)

const hashNS schematypes.Namespace = 103

func hashProjection(id uint64, marker byte) []byte {
	b := make([]byte, 40)
	binary.BigEndian.PutUint64(b[:8], id)
	b[8] = marker
	return b
}

func decodeHash(b []byte) (index.Entry, error) {
	if len(b) != 40 {
		return index.Entry{}, ErrCorruptJournal
	}
	var hash radixhash.Hash
	copy(hash[:], b[8:])
	return index.Entry{ValueID: binary.BigEndian.Uint64(b[:8]), Hashes: []index.HashValue{{Namespace: hashNS, Hash: hash}}}, nil
}

func TestHashReplacementAndTombstoneReuse(t *testing.T) {
	root := t.TempDir()
	data, idxPath := filepath.Join(root, "data"), filepath.Join(root, "idx")
	schema := schematypes.NewDefinitions()
	if err := schema.Register(schematypes.NamespaceDef{Namespace: hashNS, Name: "digest", Kind: schematypes.KindHashExact, Entity: schematypes.EntityImage}); err != nil {
		t.Fatal(err)
	}
	idx, err := index.Create(idxPath, index.Spec{Schema: schema})
	if err != nil {
		t.Fatal(err)
	}
	if err := idx.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := Open(data, idxPath, decodeHash, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	lookup := func(marker byte) []uint64 {
		var h radixhash.Hash
		h[0] = marker
		engine, ok := db.Index().Hash(hashNS)
		if !ok {
			t.Fatal("hash unavailable")
		}
		return engine.Lookup(nil, h)
	}
	for _, marker := range []byte{'a', 'b'} {
		g, err := db.Commit(Mutation{Entity: schematypes.EntityImage, ValueID: 1, Projection: hashProjection(1, marker)})
		if err != nil {
			t.Fatal(err)
		}
		wait(t, db, g)
	}
	if len(lookup('a')) != 0 || len(lookup('b')) != 1 {
		t.Fatal("old hash survived replacement")
	}
	g, err := db.Commit(Mutation{Entity: schematypes.EntityImage, ValueID: 1, Delete: true})
	if err != nil {
		t.Fatal(err)
	}
	wait(t, db, g)
	g, err = db.Commit(Mutation{Entity: schematypes.EntityImage, ValueID: 1, Projection: hashProjection(1, 'c')})
	if err != nil {
		t.Fatal(err)
	}
	wait(t, db, g)
	if len(lookup('b')) != 0 || len(lookup('c')) != 1 {
		t.Fatal("stale hash survived reinsert")
	}
}
