package database

import (
	"bytes"
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
		if err := db.values.Scan(db.canonical[kind], func(key store.Key, _ []byte) error {
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
	observed := schema.NewDefinitions()
	if db.ids != nil {
		observed = db.ids.Definitions()
		if err := validateIndexDefinitions(observed, db.indexes.Schema()); err != nil {
			return err
		}
	} else {
		// Only the one-time legacy bootstrap may reconstruct definitions from
		// historical index metadata. Normal startup loads schema.dat first.
		for _, spec := range canonicalSpecsFor(db.canonical) {
			if spec.Entity.Valid() {
				if err := observed.Register(spec.NamespaceDef()); err != nil {
					return err
				}
			}
		}
		for _, def := range db.indexes.Schema().Defs() {
			if def.Namespace == journalNS || def.Namespace == metadataNS || def.Namespace == 0xFFF0 {
				return fmt.Errorf("database: reserved namespace %d: %w", def.Namespace, schema.ErrNamespace)
			}
			if err := observed.Register(def); err != nil {
				return fmt.Errorf("database: global namespace ownership: %w", err)
			}
		}
	}
	entities := make([]schema.EntityConfig, 0, schema.EntityKindCount)
	for n := 1; n <= schema.EntityKindCount; n++ {
		kind := schema.EntityKind(n)
		entities = append(entities, schema.EntityConfig{Entity: kind, ReserveSize: 1000})
	}
	requested := db.options.Definitions
	if requested != nil {
		if err := validateDefinitions(requested, observed); err != nil {
			return err
		}
	}
	if !complete && requested == nil {
		requested = observed
	} // bootstrap old databases once
	if requested == nil {
		entities = nil
	}
	ids := db.ids
	if ids == nil {
		ids, err = schema.Open(path, schema.Options{Definitions: requested, Entities: entities})
		if err != nil {
			return err
		}
	}
	if err := validateDefinitions(ids.Definitions(), observed); err != nil {
		if db.ids == nil {
			_ = ids.Close()
		}
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

func validateDefinitions(authoritative, observed *schema.Definitions) error {
	if authoritative.Len() != observed.Len() {
		return fmt.Errorf("database: namespace count persisted=%d observed=%d: %w", authoritative.Len(), observed.Len(), schema.ErrNamespace)
	}
	for _, def := range observed.Defs() {
		want, ok := authoritative.Lookup(def.Namespace)
		if !ok || want.Name != def.Name || want.Kind != def.Kind || want.Entity != def.Entity || want.ProjectionVersion != def.ProjectionVersion || !bytes.Equal(want.Config, def.Config) {
			return fmt.Errorf("database: conflicting namespace %d: %w", def.Namespace, schema.ErrNamespace)
		}
	}
	return nil
}

func validateIndexDefinitions(authoritative, indexes *schema.Definitions) error {
	count := 0
	for _, def := range authoritative.Defs() {
		if def.Kind != schema.KindKV {
			count++
		}
	}
	if count != indexes.Len() {
		return schema.ErrNamespace
	}
	for _, def := range indexes.Defs() {
		want, ok := authoritative.Lookup(def.Namespace)
		if !ok || want.Kind != def.Kind || want.Name != def.Name || want.Entity != def.Entity || want.ProjectionVersion != def.ProjectionVersion || !bytes.Equal(want.Config, def.Config) {
			return fmt.Errorf("database: index namespace %d differs from central schema: %w", def.Namespace, schema.ErrNamespace)
		}
	}
	return nil
}

// validateLegacyIndexCopy permits a strictly additive target while an old
// Pebble backup awaits retirement. It never treats the backup as authority.
func validateLegacyIndexCopy(authoritative, legacy *schema.Definitions) error {
	active := schema.NewDefinitions()
	for _, def := range authoritative.Defs() {
		if def.Kind != schema.KindKV {
			if err := active.Register(def); err != nil {
				return err
			}
		}
	}
	if legacy.Len() > active.Len() {
		return schema.ErrNamespace
	}
	for i, def := range legacy.Defs() {
		want := active.Defs()[i]
		if def.Namespace != want.Namespace || def.Name != want.Name || def.Kind != want.Kind || def.Entity != want.Entity || def.ProjectionVersion != want.ProjectionVersion || !bytes.Equal(def.Config, want.Config) {
			return schema.ErrNamespace
		}
	}
	return nil
}
