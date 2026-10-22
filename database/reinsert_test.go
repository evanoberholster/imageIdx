package database

import (
	"testing"

	schema "github.com/evanoberholster/imageIdx/schema"
)

func TestReinsertAfterDeleteRestoresLiveness(t *testing.T) {
	data, idxPath := paths(t)
	db := openTest(t, data, idxPath)
	defer db.Close()

	if _, err := db.Commit(Mutation{Entity: schema.EntityImage, ValueID: 1, Projection: projection(1, 'a')}); err != nil {
		t.Fatal(err)
	}
	if err := db.WaitIndexed(t.Context(), 1); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Commit(Mutation{Entity: schema.EntityImage, ValueID: 1, Delete: true}); err != nil {
		t.Fatal(err)
	}
	if err := db.WaitIndexed(t.Context(), 2); err != nil {
		t.Fatal(err)
	}
	// Reinsert the same projection. The tombstone's old projection must not
	// make this look like a no-op update that skips liveness.
	if _, err := db.Commit(Mutation{Entity: schema.EntityImage, ValueID: 1, Projection: projection(1, 'a')}); err != nil {
		t.Fatal(err)
	}
	if err := db.WaitIndexed(t.Context(), 3); err != nil {
		t.Fatal(err)
	}
	if !isLive(t, db, schema.EntityImage, 1) {
		t.Fatal("reinserted entity is not live")
	}
	idx, _ := db.indexes.Bitmap(tagNS)
	if !containsTag(t, idx, 1, 'a') {
		t.Fatal("reinserted association missing")
	}
}
