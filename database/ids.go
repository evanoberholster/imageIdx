package database

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/evanoberholster/imageIdx/schema"
	"github.com/evanoberholster/imageIdx/store"
)

func (db *DB) canonicalMaxima() ([schema.EntityKindCount + 1]uint64, error) {
	var maxima [schema.EntityKindCount + 1]uint64
	for n := 1; n <= schema.EntityKindCount; n++ {
		kind := schema.EntityKind(n)
		if err := db.values.Scan(schema.Namespace(kind), func(key store.Key, _ []byte) error {
			if key.Flags != 0 || key.ID == 0 {
				return fmt.Errorf("database: invalid canonical ID key for %s", kind)
			}
			if key.ID > maxima[n] {
				maxima[n] = key.ID
			}
			return nil
		}); err != nil {
			return maxima, err
		}
	}
	return maxima, nil
}

func (db *DB) openIDs(dir string) error {
	path := filepath.Join(dir, "schema.dat")
	complete, err := db.values.SchemaMigrationComplete()
	if err != nil {
		return err
	}
	if complete {
		if _, err := os.Stat(path); err != nil {
			return errors.Join(schema.ErrCorrupt, fmt.Errorf("database: missing allocation file after migration: %w", err))
		}
	}
	defs := schema.NewDefinitions()
	for _, spec := range canonicalSpecs() {
		if spec.Entity.Valid() {
			if err := defs.Register(spec.NamespaceDef()); err != nil {
				return err
			}
		}
	}
	for _, def := range db.indexes.Schema().Defs() {
		if err := defs.Register(def); err != nil {
			return fmt.Errorf("database: global namespace ownership: %w", err)
		}
	}
	entities := make([]schema.EntityConfig, 0, schema.EntityKindCount)
	for n := 1; n <= schema.EntityKindCount; n++ {
		kind := schema.EntityKind(n)
		entities = append(entities, schema.EntityConfig{Entity: kind, ReserveSize: 1000})
	}
	ids, err := schema.Open(path, schema.Options{Definitions: defs, Entities: entities})
	if err != nil {
		return err
	}
	db.ids = ids
	if complete {
		return nil
	}
	// The legacy Pebble high-water marks are authoritative even if the
	// corresponding record never committed or was later deleted. A canonical
	// scan only reconciles records beyond those marks on this one-time path.
	legacy, _, err := db.values.LegacyReservations()
	if err != nil {
		return err
	}
	maxima, err := db.canonicalMaxima()
	if err != nil {
		return err
	}
	for n := 1; n <= schema.EntityKindCount; n++ {
		floor := legacy[n]
		if maxima[n] > floor {
			floor = maxima[n]
		}
		if err := ids.EnsureMinimum(schema.EntityKind(n), floor); err != nil {
			return err
		}
	}
	// Atomic deletion of the legacy keys and publication of this marker is
	// LAST. A crash beforehand repeats the idempotent floor advancement; a
	// crash afterward recovers solely from the new single-file schema.
	return db.values.CompleteSchemaMigration()
}
