package database

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/evanoberholster/imageIdx/common"
	"github.com/evanoberholster/imageIdx/schema"
	"github.com/evanoberholster/imageIdx/store"
)

func (db *DB) canonicalMaxima() ([common.EntityKindCount + 1]uint64, error) {
	var maxima [common.EntityKindCount + 1]uint64
	for n := 1; n <= common.EntityKindCount; n++ {
		kind := common.EntityKind(n)
		if err := db.values.Scan(common.Namespace(kind), func(key store.Key, _ []byte) error {
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
	defs := make([]schema.NamespaceConfig, 0, common.EntityKindCount)
	for n := 1; n <= common.EntityKindCount; n++ {
		kind := common.EntityKind(n)
		// The existing database has exactly one canonical namespace per
		// entity. Its other index namespaces continue to share that ID.
		defs = append(defs, schema.NamespaceConfig{ID: common.Namespace(kind), Name: "entity-" + kind.String(), ReserveSize: 1000})
	}
	ids, err := schema.Open(path, schema.Options{Namespaces: defs})
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
	for n := 1; n <= common.EntityKindCount; n++ {
		floor := legacy[n]
		if maxima[n] > floor {
			floor = maxima[n]
		}
		if err := ids.EnsureMinimum(common.Namespace(n), floor); err != nil {
			return err
		}
	}
	// Atomic deletion of the legacy keys and publication of this marker is
	// LAST. A crash beforehand repeats the idempotent floor advancement; a
	// crash afterward recovers solely from the new single-file schema.
	return db.values.CompleteSchemaMigration()
}
