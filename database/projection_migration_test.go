package database

import (
	"testing"

	schema "github.com/evanoberholster/imageIdx/schema"
)

func changedProjectionDefinitions(t *testing.T, source *schema.Definitions, namespace schema.Namespace, version uint16) *schema.Definitions {
	t.Helper()
	target := schema.NewDefinitions()
	for _, def := range source.Defs() {
		if def.Namespace == namespace {
			def.ProjectionVersion = version
		}
		if err := target.Register(def); err != nil {
			t.Fatal(err)
		}
	}
	return target
}

func TestProjectionVersionMigrationRebuilds(t *testing.T) {
	data, idxPath := paths(t)
	db := openTest(t, data, idxPath)
	projection := projection(1, 'a')
	if _, err := db.Commit(Mutation{
		Entity:     schema.EntityImage,
		ValueID:    1,
		Projection: projection,
		Record:     projection,
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.WaitIndexed(t.Context(), 1); err != nil {
		t.Fatal(err)
	}
	target := changedProjectionDefinitions(t, db.ids.Definitions(), tagNS, 2)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	if _, err := Open(data, idxPath, decodeTest, Options{Definitions: target}); err == nil {
		t.Fatal("projection version change without migration option was accepted")
	}
	db, err := Open(data, idxPath, decodeTest, Options{
		Definitions:         target,
		ProjectionMigration: true,
		ProjectFromRecord:   func(record []byte) ([]byte, error) { return record, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.WaitIndexed(t.Context(), 1); err != nil {
		db.Close()
		t.Fatal(err)
	}
	contains(t, db, 1, 'a', true)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	db, err = Open(data, idxPath, decodeTest, Options{Definitions: target})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	contains(t, db, 1, 'a', true)
}
