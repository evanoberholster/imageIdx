package database

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"os"
	"path/filepath"

	"github.com/evanoberholster/imageIdx/index"
	schematypes "github.com/evanoberholster/imageIdx/schema"
	"github.com/evanoberholster/imageIdx/store"
)

var ErrUnsupportedRebuild = errors.New("database: namespace cannot be rebuilt with its current ID model")

// RecoveryState describes one namespace at the instant of inspection.
type RecoveryState uint8

const (
	RecoveryHealthy RecoveryState = iota
	RecoveryBehind
	RecoveryRequired
	RecoveryFailed
)

type RecoveryStatus struct {
	Namespace  schematypes.Namespace
	State      RecoveryState
	Generation uint64
	Err        error
}

// RecoveryStatus reports the persisted namespace health and the database's
// acknowledged watermark. It returns a copy, not mutable internal state.
func (db *DB) RecoveryStatus() []RecoveryStatus {
	db.mu.Lock()
	defer db.mu.Unlock()
	if db.indexes == nil {
		return nil
	}
	health := db.indexes.Health()
	status := make([]RecoveryStatus, 0, len(health))
	for _, h := range health {
		if !h.Kind.Valid() {
			continue
		}
		generation, _ := db.indexes.NamespaceSourceGeneration(h.Namespace)
		state := RecoveryHealthy
		if !h.Available || h.Stale || h.NeedsRebuild || h.Corrupt {
			state = RecoveryRequired
		} else if db.err != nil {
			state = RecoveryFailed
		} else if generation < db.committed {
			state = RecoveryBehind
		}
		statusErr := h.Err
		if state == RecoveryFailed {
			statusErr = db.err
		}
		status = append(status, RecoveryStatus{Namespace: h.Namespace, State: state,
			Generation: generation, Err: statusErr})
	}
	return status
}

func (db *DB) needsRecovery() bool {
	idx := db.indexes
	if idx == nil || db.indexed > db.committed || idx.SourceGeneration() < db.indexed ||
		idx.SourceGeneration() > db.committed || len(idx.RecoveryNamespaces()) != 0 ||
		len(idx.DegradedNamespaces()) != 0 || db.inflight > idx.SourceGeneration() {
		return true
	}
	for _, h := range idx.Health() {
		if !h.Available {
			return true
		}
	}
	return false
}

const schemaKeyID uint64 = 4

func (db *DB) persistedSchema() (*schematypes.Definitions, error) {
	data, err := db.values.Get(store.Key{Namespace: metadataNS, ID: schemaKeyID})
	if err != nil {
		return nil, err
	}
	schema := schematypes.NewDefinitions()
	if err := schema.UnmarshalBinary(data); err != nil {
		return nil, fmt.Errorf("%w: canonical schema: %v", ErrRecoveryRequired, err)
	}
	return schema, nil
}

func (db *DB) persistSchema() error {
	schema := db.indexes.Schema()
	data, err := schema.MarshalBinary()
	if err != nil {
		return err
	}
	stored, err := db.values.Get(store.Key{Namespace: metadataNS, ID: schemaKeyID})
	if errors.Is(err, store.ErrNotFound) {
		return db.values.Set(store.Key{Namespace: metadataNS, ID: schemaKeyID}, data)
	}
	if err != nil {
		return err
	}
	if !bytes.Equal(data, stored) {
		return fmt.Errorf("%w: canonical and index schemas disagree", ErrRecoveryRequired)
	}
	return nil
}

// rebuildOnOpen is a synchronous, full-physical-store replacement. It runs
// before the DB is exposed to callers: neither the canonical writer nor any
// search plan from this DB can be active during activation. A shared bitmap
// store cannot independently replace a corrupt namespace in the current
// single-directory format, so every declared namespace is reconstructed.
func (db *DB) rebuildOnOpen(dir string) error {
	schema, err := db.persistedSchema()
	if errors.Is(err, store.ErrNotFound) && db.indexes != nil {
		schema = db.indexes.Schema()
		data, e := schema.MarshalBinary()
		if e != nil {
			return e
		}
		if e = db.values.Set(store.Key{Namespace: metadataNS, ID: schemaKeyID}, data); e != nil {
			return e
		}
		err = nil
	}
	if err != nil {
		return fmt.Errorf("%w: no authoritative schema: %v", ErrRecoveryRequired, err)
	}
	marker := activationPath(dir)
	if _, err := os.Stat(marker); err == nil {
		return fmt.Errorf("%w: unresolved activation", ErrRecoveryRequired)
	} else if !os.IsNotExist(err) {
		return err
	}
	if _, err := os.Stat(stagePath(dir)); err == nil {
		return fmt.Errorf("%w: unowned staging directory", ErrRecoveryRequired)
	} else if !os.IsNotExist(err) {
		return err
	}
	if _, err := os.Stat(backupPath(dir)); err == nil {
		return fmt.Errorf("%w: unowned previous directory", ErrRecoveryRequired)
	} else if !os.IsNotExist(err) {
		return err
	}
	schemaBytes, err := schema.MarshalBinary()
	if err != nil {
		return err
	}
	fingerprint := crc32.ChecksumIEEE(schemaBytes)
	if err := writeActivation(dir, activation{state: activationBuilding, generation: db.committed, schemaCRC: fingerprint}); err != nil {
		return err
	}
	if db.options.boundary != nil {
		db.options.boundary("before rebuild")
	}
	stage, err := index.Create(stagePath(dir), index.Spec{Schema: schema})
	if err != nil {
		return err
	}
	batch := stage.NewBatch(db.options.BatchSize)
	for entity := schematypes.EntityImage; entity.Valid(); entity++ {
		if err = db.scanRebuild(stage, schema, batch, entity); err != nil {
			break
		}
	}
	if err == nil && db.committed != 0 {
		if err = batch.Reset(); err == nil {
			err = stage.ApplyWithGenerationAll(batch, db.committed)
		}
	}
	if err == nil && db.options.boundary != nil {
		db.options.boundary("before rebuild verify")
	}
	if err == nil {
		err = stage.Verify()
	}
	closeErr := stage.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return errors.Join(ErrRecoveryRequired, fmt.Errorf("staging: %w", err))
	}
	if err := verifyReplacement(stagePath(dir), db.committed, fingerprint); err != nil {
		return err
	}
	if err := syncDirectory(stagePath(dir)); err != nil {
		return err
	}
	if err := writeActivation(dir, activation{state: activationVerified, generation: db.committed, schemaCRC: fingerprint}); err != nil {
		return err
	}
	if db.options.boundary != nil {
		db.options.boundary("after rebuild verified")
	}
	if db.indexes != nil {
		if err := db.indexes.Close(); err != nil {
			return err
		}
		db.indexes = nil
	}
	if err := resumeActivation(dir, db.values, db.options.boundary); err != nil {
		return err
	}
	db.indexes, err = index.Open(dir)
	if err != nil {
		return err
	}
	if db.indexes.SourceGeneration() < db.committed {
		return ErrRecoveryRequired
	}
	return nil
}

func (db *DB) scanRebuild(stage *index.Store, schema *schematypes.Definitions, batch *index.Batch, entity schematypes.EntityKind) error {
	// Decoders may return slices borrowing their input. Pebble iterator values
	// are valid only until the next step, so keep one reusable projection
	// buffer per staged record until Apply has finished consuming the batch.
	projections := make([][]byte, db.options.BatchSize)
	used := 0
	flush := func() error {
		if batch.Len() == 0 {
			return nil
		}
		if err := stage.Apply(batch); err != nil {
			if errors.Is(err, index.ErrNamespaceIDModel) {
				return fmt.Errorf("%w: %v", ErrUnsupportedRebuild, err)
			}
			return err
		}
		if db.options.boundary != nil {
			db.options.boundary("after rebuild batch")
		}
		if err := batch.Reset(); err != nil {
			return err
		}
		used = 0
		return nil
	}
	err := db.values.ScanFrom(schematypes.Namespace(entity), 0, func(key store.Key, raw []byte) (bool, error) {
		if db.options.boundary != nil {
			db.options.boundary("during rebuild scan")
		}
		if len(raw) < 5 || raw[0] > 1 {
			return false, ErrCorruptJournal
		}
		n := uint64(binary.BigEndian.Uint32(raw[1:5]))
		if n > uint64(len(raw)-5) {
			return false, ErrCorruptJournal
		}
		if raw[0] == 1 {
			return true, nil
		}
		projections[used] = append(projections[used][:0], raw[5:5+n]...)
		entry, err := db.decode(projections[used])
		if err != nil || entry.ValueID != key.ID {
			return false, fmt.Errorf("%w: projection at entity %d id %d: %v", ErrCorruptJournal, entity, key.ID, err)
		}
		if err := validateProjection(schema, entity, entry); err != nil {
			return false, err
		}
		if err := batch.Add(entry); err != nil {
			return false, err
		}
		used++
		if batch.Len() >= db.options.BatchSize {
			if err := flush(); err != nil {
				return false, err
			}
		}
		return true, nil
	})
	if err != nil {
		return err
	}
	return flush()
}

func validateProjection(schema *schematypes.Definitions, entity schematypes.EntityKind, entry index.Entry) error {
	check := func(ns schematypes.Namespace) error {
		def, ok := schema.Lookup(ns)
		if !ok || def.Entity != entity {
			return fmt.Errorf("%w: namespace %d does not belong to entity %d", ErrCorruptJournal, ns, entity)
		}
		return nil
	}
	for _, v := range entry.Times {
		if err := check(v.Namespace); err != nil {
			return err
		}
	}
	for _, v := range entry.Geos {
		if err := check(v.Namespace); err != nil {
			return err
		}
	}
	for _, v := range entry.Sims {
		if err := check(v.Namespace); err != nil {
			return err
		}
	}
	for _, v := range entry.Hashes {
		if err := check(v.Namespace); err != nil {
			return err
		}
	}
	for _, v := range entry.Vectors {
		if err := check(v.Namespace); err != nil {
			return err
		}
	}
	for _, v := range entry.Bitmaps {
		if err := check(v.Namespace); err != nil {
			return err
		}
	}
	return nil
}

// Recovery artifact paths are siblings, never children of the active mmap
// directory. Only this package's checked activation marker authorizes cleanup.
func stagePath(dir string) string      { return dir + ".rebuild" }
func backupPath(dir string) string     { return dir + ".previous" }
func activationPath(dir string) string { return dir + ".activation" }

const (
	activationBuilding byte = 1
	activationVerified byte = 2
)

type activation struct {
	state      byte
	generation uint64
	schemaCRC  uint32
}

func writeActivation(dir string, a activation) error {
	var data [22]byte
	copy(data[:4], "IRCV")
	data[4], data[5] = 2, a.state
	binary.BigEndian.PutUint64(data[6:14], a.generation)
	binary.BigEndian.PutUint32(data[14:18], a.schemaCRC)
	binary.BigEndian.PutUint32(data[18:22], crc32.ChecksumIEEE(data[:18]))
	parent := filepath.Dir(dir)
	f, err := os.CreateTemp(parent, ".imageidx-activation-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(data[:]); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(f.Name(), activationPath(dir)); err != nil {
		return err
	}
	return syncDirectory(parent)
}

func readActivation(dir string) (activation, error) {
	raw, err := os.ReadFile(activationPath(dir))
	if err != nil {
		return activation{}, err
	}
	if len(raw) != 22 || string(raw[:4]) != "IRCV" || raw[4] != 2 ||
		(raw[5] != activationBuilding && raw[5] != activationVerified) ||
		binary.BigEndian.Uint32(raw[18:22]) != crc32.ChecksumIEEE(raw[:18]) {
		return activation{}, ErrRecoveryRequired
	}
	return activation{state: raw[5], generation: binary.BigEndian.Uint64(raw[6:14]), schemaCRC: binary.BigEndian.Uint32(raw[14:18])}, nil
}

func syncDirectory(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	return errors.Join(f.Sync(), f.Close())
}

func exists(path string) (bool, error) {
	_, err := os.Lstat(path)
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, err
}

func verifyReplacement(path string, generation uint64, schemaCRC uint32) error {
	idx, err := index.Open(path)
	if err != nil {
		return err
	}
	defer idx.Close()
	actualSchema, err := idx.Schema().MarshalBinary()
	if err != nil || crc32.ChecksumIEEE(actualSchema) != schemaCRC {
		return ErrRecoveryRequired
	}
	for _, h := range idx.Health() {
		if !h.Available {
			return fmt.Errorf("%w: namespace %d: %v", ErrRecoveryRequired, h.Namespace, h.Err)
		}
	}
	if idx.SourceGeneration() < generation || len(idx.RecoveryNamespaces()) != 0 {
		return ErrRecoveryRequired
	}
	return idx.Verify()
}

// resumeActivation is called only during Open, before publishing any index
// pointers. The verified marker is the atomic *authorization* to publish;
// directory renames are retried until indexDir names the verified generation.
func resumeActivation(dir string, values *store.Store, boundary func(string)) error {
	if _, err := os.Stat(activationPath(dir)); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return err
	}
	a, err := readActivation(dir)
	if err != nil {
		return err
	}
	canonicalSchema, err := values.Get(store.Key{Namespace: metadataNS, ID: schemaKeyID})
	if err != nil || crc32.ChecksumIEEE(canonicalSchema) != a.schemaCRC {
		return ErrRecoveryRequired
	}
	stage, err := exists(stagePath(dir))
	if err != nil {
		return err
	}
	active, err := exists(dir)
	if err != nil {
		return err
	}
	backup, err := exists(backupPath(dir))
	if err != nil {
		return err
	}
	parent := filepath.Dir(dir)
	if a.state == activationBuilding {
		if backup {
			return ErrRecoveryRequired
		}
		if stage {
			if err := os.RemoveAll(stagePath(dir)); err != nil {
				return err
			}
			if boundary != nil {
				boundary("during staging cleanup")
			}
		}
		if err := os.Remove(activationPath(dir)); err != nil {
			return err
		}
		return syncDirectory(parent)
	}
	if stage {
		if err := verifyReplacement(stagePath(dir), a.generation, a.schemaCRC); err != nil {
			return err
		}
		if active {
			if backup {
				return ErrRecoveryRequired
			}
			if err := os.Rename(dir, backupPath(dir)); err != nil {
				return err
			}
			if err := syncDirectory(parent); err != nil {
				return err
			}
			if boundary != nil {
				boundary("after old index rename")
			}
		}
		if err := os.Rename(stagePath(dir), dir); err != nil {
			return err
		}
		if err := syncDirectory(parent); err != nil {
			return err
		}
		if boundary != nil {
			boundary("after replacement rename")
		}
	} else if !active {
		// A missing verified staging generation cannot be published. Restore
		// the old directory if possible and keep the marker for diagnosis.
		if backup {
			_ = os.Rename(backupPath(dir), dir)
			_ = syncDirectory(parent)
		}
		return ErrRecoveryRequired
	}
	if err := verifyReplacement(dir, a.generation, a.schemaCRC); err != nil {
		return err
	}
	backup, err = exists(backupPath(dir))
	if err != nil {
		return err
	}
	if backup {
		if err := os.RemoveAll(backupPath(dir)); err != nil {
			return err
		}
		if boundary != nil {
			boundary("during old index cleanup")
		}
	}
	if err := syncDirectory(parent); err != nil {
		return err
	}
	if err := os.Remove(activationPath(dir)); err != nil {
		return err
	}
	return syncDirectory(parent)
}
