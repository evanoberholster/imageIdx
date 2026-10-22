// Package store provides Pebble-backed key-value storage under registered
// namespaces. Keys are fixed-width [uint16 namespace][uint64 ID][uint16 flags]
// in big-endian order. Namespace zero is reserved for the persisted schema.
package store

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"github.com/cockroachdb/pebble/v2"
	schematypes "github.com/evanoberholster/imageIdx/schema"
)

var (
	ErrNotFound       = pebble.ErrNotFound
	ErrNamespace      = errors.New("store: namespace is not registered")
	ErrSchemaMismatch = errors.New("store: persisted schema differs from requested schema")
	ErrEmptySpecs     = errors.New("store: at least one namespace spec is required")
	ErrInvalidDir     = errors.New("store: directory is empty")
)

// Spec defines a KV collection. Its namespace number and name must be stable
// across reopenings of the same directory.
type Spec struct {
	Namespace schematypes.Namespace
	Name      string
	Entity    schematypes.EntityKind
}

// NamespaceDef registers the collection in the shared namespace vocabulary.
func (s Spec) NamespaceDef() schematypes.NamespaceDef {
	return schematypes.NamespaceDef{Namespace: s.Namespace, Name: s.Name, Kind: schematypes.KindKV, Entity: s.Entity}
}

// kvNamespaceWords covers the complete uint16 namespace space.
const kvNamespaceWords = 1 << 10

// kvNamespaceBits is an immutable-after-open membership set for KV namespaces.
// It replaces the former map on the Store's hot validation path. The type is
// intentionally unexported: callers register namespaces through schema
// definitions, never through a mutable Store API.
type kvNamespaceBits struct {
	words [kvNamespaceWords]uint64
}

func (b *kvNamespaceBits) add(ns schematypes.Namespace) {
	n := uint16(ns)
	b.words[n>>6] |= uint64(1) << (n & 63)
}

func (b *kvNamespaceBits) contains(ns schematypes.Namespace) bool {
	n := uint16(ns)
	return b.words[n>>6]&(uint64(1)<<(n&63)) != 0
}

// Store owns one Pebble database. Pebble permits concurrent reads and writes.
// Close must run after all callers have finished using the store.
type Store struct {
	db         *pebble.DB
	namespaces kvNamespaceBits
	stats      storeStats
}

var schemaKey = EncodeKey(Key{}) // obsolete registry, migration-only

// Open consumes the caller's authoritative KV definitions. It
// never reads or writes a serialized schema in Pebble. Migration-only callers
// must validate any legacy Pebble schema separately before using it.
func Open(dir string, definitions *schematypes.Definitions) (*Store, error) {
	if definitions == nil || definitions.Len() == 0 {
		return nil, ErrEmptySpecs
	}
	var namespaces kvNamespaceBits
	for _, def := range definitions.Defs() {
		if def.Kind != schematypes.KindKV {
			return nil, fmt.Errorf("store: namespace %d is not KV: %w", def.Namespace, ErrSchemaMismatch)
		}
		namespaces.add(def.Namespace)
	}
	return openPrepared(dir, nil, namespaces, false)
}

// OpenLegacy exists only for reading or constructing pre-schema.dat fixtures.
// It writes the historical Pebble schema key; never use it with schema.dat.
func OpenLegacy(dir string, specs ...Spec) (*Store, error) { return open(dir, false, specs...) }

// OpenLegacyWithExtension is the one-time append-only compatibility upgrade
// for old stores before schema.dat has been durably published.
func OpenLegacyWithExtension(dir string, specs ...Spec) (*Store, error) {
	return open(dir, true, specs...)
}

func open(dir string, extend bool, specs ...Spec) (*Store, error) {
	if dir == "" {
		return nil, ErrInvalidDir
	}
	if len(specs) == 0 {
		return nil, ErrEmptySpecs
	}
	schema := schematypes.NewDefinitions()
	var namespaces kvNamespaceBits
	for _, spec := range specs {
		if err := schema.Register(spec.NamespaceDef()); err != nil {
			return nil, fmt.Errorf("store: invalid spec: %w", err)
		}
		namespaces.add(spec.Namespace)
	}
	schemaBytes, err := schema.MarshalBinary()
	if err != nil {
		return nil, err
	}
	return openPrepared(dir, schemaBytes, namespaces, extend)
}

func openPrepared(dir string, schemaBytes []byte, namespaces kvNamespaceBits, extend bool) (*Store, error) {
	if dir == "" {
		return nil, ErrInvalidDir
	}
	db, err := pebble.Open(dir, nil)
	if err != nil {
		return nil, err
	}
	if schemaBytes != nil {
		if err := verifySchema(db, schemaBytes, extend); err != nil {
			_ = db.Close()
			return nil, err
		}
	}
	return &Store{db: db, namespaces: namespaces}, nil
}

// ValidateLegacyDefinitions is only for migrating a persisted Pebble schema
// copy. It never writes, and a missing copy is already migrated.
func (s *Store) ValidateLegacyDefinitions(expected *schematypes.Definitions) error {
	data, err := expected.MarshalBinary()
	if err != nil {
		return err
	}
	value, closer, err := s.db.Get(schemaKey[:])
	if errors.Is(err, pebble.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	matches := bytes.Equal(value, data)
	if !matches {
		var old, desired schematypes.Definitions
		if old.UnmarshalBinary(value) == nil && desired.UnmarshalBinary(data) == nil {
			before, after := old.Defs(), desired.Defs()
			matches = len(before) <= len(after)
			for i := 0; matches && i < len(before); i++ {
				a, b := before[i], after[i]
				ownerUpgrade := a.Entity == schematypes.EntityInvalid && b.Entity.Valid() &&
					b.Entity == schematypes.EntityKind(b.Namespace) && b.Name == "entity-"+b.Entity.String()
				matches = a.Namespace == b.Namespace && a.Name == b.Name && a.Kind == b.Kind &&
					(a.Entity == b.Entity || ownerUpgrade) && bytes.Equal(a.Config, b.Config)
			}
		}
	}
	if err := closer.Close(); err != nil {
		return err
	}
	if !matches {
		return mismatchDetail(value, data)
	}
	return nil
}

// mismatchDetail names the first namespace whose persisted and requested
// definitions disagree. It is errors.Is-compatible with ErrSchemaMismatch and
// falls back to the bare sentinel when either blob cannot be decoded.
func mismatchDetail(persisted, requested []byte) error {
	var old, desired schematypes.Definitions
	if old.UnmarshalBinary(persisted) != nil || desired.UnmarshalBinary(requested) != nil {
		return ErrSchemaMismatch
	}
	before, after := old.Defs(), desired.Defs()
	for i := 0; i < len(before) && i < len(after); i++ {
		a, b := before[i], after[i]
		if a.Namespace != b.Namespace {
			return fmt.Errorf("%w: namespace %d persisted as %d", ErrSchemaMismatch, a.Namespace, b.Namespace)
		}
		if a.Name != b.Name {
			return fmt.Errorf("%w: namespace %d name %q requested as %q", ErrSchemaMismatch, a.Namespace, a.Name, b.Name)
		}
		if a.Kind != b.Kind {
			return fmt.Errorf("%w: namespace %d kind %s requested as %s", ErrSchemaMismatch, a.Namespace, a.Kind, b.Kind)
		}
		ownerUpgrade := a.Entity == schematypes.EntityInvalid && b.Entity.Valid() &&
			b.Entity == schematypes.EntityKind(b.Namespace) && b.Name == "entity-"+b.Entity.String()
		if a.Entity != b.Entity && !ownerUpgrade {
			return fmt.Errorf("%w: namespace %d entity %s requested as %s", ErrSchemaMismatch, a.Namespace, a.Entity, b.Entity)
		}
		if !bytes.Equal(a.Config, b.Config) {
			return fmt.Errorf("%w: namespace %d configuration differs", ErrSchemaMismatch, a.Namespace)
		}
	}
	return fmt.Errorf("%w: persisted %d namespaces, requested %d", ErrSchemaMismatch, len(before), len(after))
}

// RemoveLegacyDefinitions retires the old registry only after the caller has
// durably installed schema.dat and validated the old copy. Repeating deletion
// after an interrupted cleanup is safe.
func (s *Store) RemoveLegacyDefinitions() error {
	_, closer, err := s.db.Get(schemaKey[:])
	if errors.Is(err, pebble.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := closer.Close(); err != nil {
		return err
	}
	return s.db.Delete(schemaKey[:], pebble.Sync)
}

func verifySchema(db *pebble.DB, expected []byte, extend bool) error {
	value, closer, err := db.Get(schemaKey[:])
	switch {
	case err == nil:
		matches := bytes.Equal(value, expected)
		canExtend := false
		if !matches && extend {
			var oldSchema, newSchema schematypes.Definitions
			if oldSchema.UnmarshalBinary(value) == nil && newSchema.UnmarshalBinary(expected) == nil {
				before, after := oldSchema.Defs(), newSchema.Defs()
				canExtend = len(before) <= len(after)
				changed := len(before) < len(after)
				for i := 0; canExtend && i < len(before); i++ {
					a, b := before[i], after[i]
					ownerUpgrade := a.Entity == schematypes.EntityInvalid && b.Entity.Valid() &&
						b.Entity == schematypes.EntityKind(b.Namespace) && b.Name == "entity-"+b.Entity.String()
					if ownerUpgrade {
						changed = true
					}
					canExtend = a.Namespace == b.Namespace && a.Name == b.Name && a.Kind == b.Kind &&
						(a.Entity == b.Entity || ownerUpgrade) && bytes.Equal(a.Config, b.Config)
				}
				canExtend = canExtend && changed
			}
		}
		if err := closer.Close(); err != nil {
			return err
		}
		if !matches && !canExtend {
			return mismatchDetail(value, expected)
		}
		if canExtend {
			return db.Set(schemaKey[:], expected, pebble.Sync)
		}
		return nil
	case !errors.Is(err, pebble.ErrNotFound):
		return err
	}
	iter, err := db.NewIter(nil)
	if err != nil {
		return err
	}
	nonempty := iter.First()
	iterErr := iter.Error()
	closeErr := iter.Close()
	if iterErr != nil {
		return iterErr
	}
	if closeErr != nil {
		return closeErr
	}
	if nonempty {
		return ErrSchemaMismatch
	}
	return db.Set(schemaKey[:], expected, pebble.Sync)
}

func (s *Store) check(key Key) error {
	if !s.namespaces.contains(key.Namespace) {
		return fmt.Errorf("%w: %d", ErrNamespace, key.Namespace)
	}
	return nil
}

// Set replaces the value for key. Pebble copies value before returning.
func (s *Store) Set(key Key, value []byte) error {
	if err := s.check(key); err != nil {
		return err
	}
	encoded := EncodeKey(key)
	if err := s.db.Set(encoded[:], value, pebble.Sync); err != nil {
		return err
	}
	s.stats.commits.Add(1)
	s.stats.sets.Add(1)
	s.stats.keyBytes.Add(KeySize)
	s.stats.valueBytes.Add(uint64(len(value)))
	return nil
}

// Get returns an owned copy of the value. A missing key returns ErrNotFound.
func (s *Store) Get(key Key) ([]byte, error) {
	if err := s.check(key); err != nil {
		return nil, err
	}
	encoded := EncodeKey(key)
	value, closer, err := s.db.Get(encoded[:])
	if err != nil {
		return nil, err
	}
	copyOfValue := append([]byte(nil), value...)
	if err := closer.Close(); err != nil {
		return nil, err
	}
	return copyOfValue, nil
}

// Delete removes key. Deleting a missing key succeeds.
func (s *Store) Delete(key Key) error {
	if err := s.check(key); err != nil {
		return err
	}
	encoded := EncodeKey(key)
	if err := s.db.Delete(encoded[:], pebble.Sync); err != nil {
		return err
	}
	s.stats.commits.Add(1)
	s.stats.deletes.Add(1)
	s.stats.keyBytes.Add(KeySize)
	return nil
}

// Scan visits a namespace in ascending ID and flags order. The value passed
// to visit remains valid only until visit returns; copy it to retain it.
func (s *Store) Scan(namespace schematypes.Namespace, visit func(Key, []byte) error) error {
	if err := s.check(Key{Namespace: namespace}); err != nil {
		return err
	}
	var lower [2]byte
	binary.BigEndian.PutUint16(lower[:], uint16(namespace))
	options := &pebble.IterOptions{LowerBound: lower[:]}
	if namespace < schematypes.Namespace(^uint16(0)) {
		var upper [2]byte
		binary.BigEndian.PutUint16(upper[:], uint16(namespace)+1)
		options.UpperBound = upper[:]
	}
	iter, err := s.db.NewIter(options)
	if err != nil {
		return err
	}
	for valid := iter.First(); valid; valid = iter.Next() {
		keyBytes := iter.Key()
		if len(keyBytes) < 2 || binary.BigEndian.Uint16(keyBytes[:2]) != uint16(namespace) {
			break
		}
		if len(keyBytes) != KeySize {
			return errors.Join(ErrInvalidKey, iter.Close())
		}
		key, _ := DecodeKey(keyBytes)
		value, err := iter.ValueAndErr()
		if err != nil {
			return errors.Join(err, iter.Close())
		}
		if err := visit(key, value); err != nil {
			return errors.Join(err, iter.Close())
		}
	}
	return errors.Join(iter.Error(), iter.Close())
}

// Close releases the database lock and flushes Pebble resources.
func (s *Store) Close() error { return s.db.Close() }

var _ io.Closer = (*Store)(nil)
