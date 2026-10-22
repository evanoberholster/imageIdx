package database

import (
	"bytes"
	"errors"
	"fmt"

	"github.com/evanoberholster/imageIdx/index"
	"github.com/evanoberholster/imageIdx/schema"
	"github.com/evanoberholster/imageIdx/store"
)

// validateAddition permits only an append of completely new logical
// namespaces. Both snapshots are owned by schema; existing identities and
// configs are never rewritten to make a physical index open.
func projectionVersionsDiffer(active, target *schema.Definitions) bool {
	before, after := active.Defs(), target.Defs()
	for i := 0; i < len(before) && i < len(after); i++ {
		if before[i].ProjectionVersion != after[i].ProjectionVersion {
			return true
		}
	}
	return false
}

func validateAddition(active, target *schema.Definitions, allowProjectionChange bool) error {
	before, after := active.Defs(), target.Defs()
	if len(after) < len(before) {
		return fmt.Errorf("%w: requested %d namespaces, active %d", schema.ErrNamespace, len(after), len(before))
	}
	projectionChanged := false
	for i, old := range before {
		new := after[i]
		switch {
		case old.Namespace != new.Namespace:
			return fmt.Errorf("%w: namespace %d requested as %d", schema.ErrNamespace, old.Namespace, new.Namespace)
		case old.Name != new.Name:
			return fmt.Errorf("%w: namespace %d renamed %q to %q", schema.ErrNamespace, old.Namespace, old.Name, new.Name)
		case old.Kind != new.Kind:
			return fmt.Errorf("%w: namespace %d kind changed %s to %s", schema.ErrNamespace, old.Namespace, old.Kind, new.Kind)
		case old.Entity != new.Entity:
			return fmt.Errorf("%w: namespace %d entity changed %s to %s", schema.ErrNamespace, old.Namespace, old.Entity, new.Entity)
		case old.ProjectionVersion != new.ProjectionVersion:
			if !allowProjectionChange {
				return fmt.Errorf("%w: namespace %d projection version changed %d to %d", schema.ErrNamespace, old.Namespace, old.ProjectionVersion, new.ProjectionVersion)
			}
			projectionChanged = true
		case !bytes.Equal(old.Config, new.Config):
			return fmt.Errorf("%w: namespace %d configuration changed", schema.ErrNamespace, old.Namespace)
		}
	}
	if len(after) == len(before) {
		if !projectionChanged {
			return fmt.Errorf("%w: no schema additions or projection changes", schema.ErrNamespace)
		}
		return index.ValidateDefinitions(target)
	}
	for _, def := range after[len(before):] {
		if def.Namespace == journalNS || def.Namespace == metadataNS || def.Namespace == 0xFFF0 {
			return fmt.Errorf("%w: namespace %d is reserved", schema.ErrNamespace, def.Namespace)
		}
	}
	return index.ValidateDefinitions(target)
}

func kvDefinitions(defs *schema.Definitions) (*schema.Definitions, error) {
	kv := schema.NewDefinitions()
	canonical, err := persistedCanonical(defs, [schema.EntityKindCount + 1]schema.Namespace{})
	if err != nil {
		return nil, err
	}
	for _, spec := range canonicalSpecsFor(canonical) {
		def := spec.NamespaceDef()
		if spec.Entity.Valid() {
			var ok bool
			def, ok = defs.Lookup(spec.Namespace)
			if !ok || def.Kind != schema.KindKV || def.Name != spec.Name || def.Entity != spec.Entity {
				return nil, schema.ErrNamespace
			}
		}
		if err := kv.Register(def); err != nil {
			return nil, err
		}
	}
	for _, def := range defs.Defs() {
		if def.Kind == schema.KindKV {
			if _, ok := kv.Lookup(def.Namespace); ok {
				continue
			}
			if err := kv.Register(def); err != nil {
				return nil, err
			}
		}
	}
	return kv, nil
}

// checkActiveSchema runs before PREPARED is published. It does not extend
// either store: a mismatch leaves the active schema fully usable.
func checkActiveSchema(dir, indexDir string, active *schema.Definitions, retired bool) error {
	kv, err := kvDefinitions(active)
	if err != nil {
		return err
	}
	values, err := store.Open(dir, kv)
	if err != nil {
		return err
	}
	defer func() {
		if values != nil {
			_ = values.Close()
		}
	}()
	if !retired {
		if err := values.ValidateLegacyDefinitions(kv); err != nil {
			return err
		}
		if data, err := values.Get(store.Key{Namespace: metadataNS, ID: schemaKeyID}); err == nil {
			backup := schema.NewDefinitions()
			if err := backup.UnmarshalBinary(data); err != nil {
				return err
			}
			if err := validateIndexDefinitions(active, backup); err != nil {
				return err
			}
		} else if !errors.Is(err, store.ErrNotFound) {
			return err
		}
	}
	idx, err := index.OpenWithDefinitions(indexDir, active)
	if err != nil {
		return err
	}
	for _, h := range idx.Health() {
		if !h.Available {
			_ = idx.Close()
			return errors.Join(ErrRecoveryRequired, h.Err)
		}
	}
	err = idx.Verify()
	return errors.Join(err, idx.Close())
}

// recoverPreparedSchema is called only while the database ownership lock and
// schema lock are held. All physical work is repeatable. The prepared target
// is loaded from schema.dat, never reconstructed from the caller's options.
func recoverPreparedSchema(ids *schema.Schema, dir, indexDir string, decode Decoder, options Options) error {
	target := ids.PreparedDefinitions()
	if target == nil {
		return nil
	}
	active := ids.Definitions()
	if err := validateAddition(active, target, true); err != nil {
		return errors.Join(ErrRecoveryRequired, err)
	}
	canonical, err := persistedCanonical(active, options.CanonicalNamespaces)
	if err != nil {
		return err
	}
	kv, err := kvDefinitions(active)
	if err != nil {
		return err
	}
	values, err := store.Open(dir, kv)
	if err != nil {
		return err
	}
	defer func() {
		if values != nil {
			_ = values.Close()
		}
	}()
	if !ids.LegacyRetired() {
		if err := values.ValidateLegacyDefinitions(kv); err != nil {
			return err
		}
	}
	if err := resumeActivation(indexDir, values, target, options.boundary); err != nil {
		return err
	}
	forceReproject := options.ProjectionMigration || projectionVersionsDiffer(active, target)
	temp := &DB{values: values, ids: ids, canonical: canonical, decode: decode, decodeInto: options.DecodeInto, projectFromRecord: options.ProjectFromRecord, forceReproject: forceReproject, options: options}
	if temp.committed, err = temp.readGeneration(1); err != nil {
		return err
	}
	if options.boundary != nil {
		options.boundary("before schema index initialization")
	}
	// A previous attempt may already have published a complete target index
	// before the schema ACTIVE superblock was synchronized.
	ready, openErr := index.OpenWithDefinitions(indexDir, target)
	if openErr == nil {
		if ready.SourceGeneration() != temp.committed || len(ready.RecoveryNamespaces()) != 0 {
			openErr = ErrRecoveryRequired
		} else {
			for _, h := range ready.Health() {
				if !h.Available {
					openErr = ErrRecoveryRequired
					break
				}
			}
			if openErr == nil {
				openErr = ready.Verify()
			}
		}
		closeErr := ready.Close()
		if openErr == nil {
			openErr = closeErr
		}
	}
	if openErr != nil {
		if forceReproject && options.ProjectFromRecord == nil {
			return errors.Join(ErrRecoveryRequired, ErrProjectionUnavailable)
		}
		// Rebuild under the prepared target. Existing healthy physical files are
		// left untouched until a verified staged replacement is activated.
		// rebuildWithDefinitions may publish temp.indexes before a later check
		// fails, so always release it on error to avoid leaking the index lock.
		if err := temp.rebuildWithDefinitions(indexDir, target); err != nil {
			if temp.indexes != nil {
				_ = temp.indexes.Close()
				temp.indexes = nil
			}
			return errors.Join(ErrRecoveryRequired, err)
		}
		if err := temp.indexes.Close(); err != nil {
			return err
		}
		temp.indexes = nil
	}
	if options.boundary != nil {
		options.boundary("after schema index initialization")
	}
	closeErr := values.Close()
	values = nil
	if closeErr != nil {
		return closeErr
	}
	if options.boundary != nil {
		options.boundary("before schema activation")
	}
	if err := ids.Activate(); err != nil {
		return fmt.Errorf("database: schema activation: %w", err)
	}
	if options.boundary != nil {
		options.boundary("after schema activation")
	}
	return nil
}

func prepareSchemaAddition(ids *schema.Schema, path, dir, indexDir string, target *schema.Definitions, decode Decoder, options Options) (*schema.Schema, error) {
	active := ids.Definitions()
	if err := validateAddition(active, target, options.ProjectionMigration); err != nil {
		return ids, err
	}
	if err := checkActiveSchema(dir, indexDir, active, ids.LegacyRetired()); err != nil {
		return ids, err
	}
	if options.boundary != nil {
		options.boundary("before schema prepare")
	}
	var entities []schema.EntityConfig
	for e := schema.EntityImage; e.Valid(); e++ {
		if size := ids.ReservationSize(e); size != 0 {
			entities = append(entities, schema.EntityConfig{Entity: e, ReserveSize: size})
		}
	}
	if err := ids.Close(); err != nil {
		return nil, err
	}
	prepared, err := schema.Open(path, schema.Options{Definitions: target, Entities: entities, Prepare: true})
	if err != nil {
		return nil, err
	}
	if options.boundary != nil {
		options.boundary("after schema prepare")
	}
	if err := recoverPreparedSchema(prepared, dir, indexDir, decode, options); err != nil {
		_ = prepared.Close()
		return nil, err
	}
	return prepared, nil
}
