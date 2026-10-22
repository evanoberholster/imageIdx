package database

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	schema "github.com/evanoberholster/imageIdx/schema"
)

func TestExternalCanonicalProjectionRoundTrip(t *testing.T) {
	data, idxPath := paths(t)
	project := func(record []byte) ([]byte, error) { return record, nil }
	open := func() *DB {
		db, err := Open(data, idxPath, decodePair, Options{
			BatchSize:           64,
			CanonicalProjection: CanonicalProjectionExternal,
			ProjectFromRecord:   project,
		})
		if err != nil {
			t.Fatal(err)
		}
		return db
	}

	db := open()
	first := pairProjection(1, 'C', '3')
	if _, err := db.Commit(Mutation{Entity: schema.EntityImage, ValueID: 1, Projection: first, Record: first}); err != nil {
		t.Fatal(err)
	}
	if err := db.WaitIndexed(t.Context(), 1); err != nil {
		t.Fatal(err)
	}
	if got, err := db.Get(schema.EntityImage, 1); err != nil || string(got) != string(first) {
		t.Fatalf("record = %x %v", got, err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	db = open()
	if got, err := db.Get(schema.EntityImage, 1); err != nil || string(got) != string(first) {
		t.Fatalf("reopened record = %x %v", got, err)
	}
	second := pairProjection(1, 'C', '4')
	if _, err := db.Commit(Mutation{Entity: schema.EntityImage, ValueID: 1, Projection: second, Record: second}); err != nil {
		t.Fatal(err)
	}
	if err := db.WaitIndexed(t.Context(), 2); err != nil {
		t.Fatal(err)
	}
	assertPairStateForID(t, db, 1, 'C', '4')
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	db = open()
	defer db.Close()
	assertPairStateForID(t, db, 1, 'C', '4')
}

func TestExternalCanonicalProjectionRequiresCallback(t *testing.T) {
	data, idxPath := paths(t)
	if _, err := Open(data, idxPath, decodePair, Options{CanonicalProjection: CanonicalProjectionExternal}); !errors.Is(err, ErrInvalidMutation) {
		t.Fatalf("external mode without callback: %v", err)
	}
}

func TestExternalCanonicalProjectionRecovery(t *testing.T) {
	data, idxPath := paths(t)
	project := func(record []byte) ([]byte, error) { return record, nil }
	db, err := Open(data, idxPath, decodePair, Options{
		BatchSize:           64,
		CanonicalProjection: CanonicalProjectionExternal,
		ProjectFromRecord:   project,
	})
	if err != nil {
		t.Fatal(err)
	}
	projection := pairProjection(1, 'C', '3')
	if _, err := db.Commit(Mutation{Entity: schema.EntityImage, ValueID: 1, Projection: projection, Record: projection}); err != nil {
		t.Fatal(err)
	}
	if err := db.WaitIndexed(t.Context(), 1); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(idxPath, "manifest")); err != nil {
		t.Fatal(err)
	}
	db, err = Open(data, idxPath, decodePair, Options{
		BatchSize:           64,
		Recovery:            RecoveryAutomatic,
		CanonicalProjection: CanonicalProjectionExternal,
		ProjectFromRecord:   project,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	assertPairStateForID(t, db, 1, 'C', '3')
}

func assertPairStateForID(t *testing.T, db *DB, id uint64, camera, rating byte) {
	t.Helper()
	cameraIdx, _ := db.indexes.Bitmap(tagNS)
	ratingIdx, _ := db.indexes.Bitmap(102)
	if cameraIdx == nil || ratingIdx == nil {
		t.Fatal("pair indexes missing")
	}
	if !containsTag(t, cameraIdx, id, camera) || !containsTag(t, ratingIdx, id, rating) {
		t.Fatalf("pair state for %d missing", id)
	}
}
