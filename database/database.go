// Package database coordinates authoritative Pebble records with a derived
// unified index. Canonical commits are durable before their indexing completes.
package database

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"

	"github.com/evanoberholster/imageIdx/index"
	"github.com/evanoberholster/imageIdx/internal/storage"
	"github.com/evanoberholster/imageIdx/schema"
	"github.com/evanoberholster/imageIdx/store"
)

const journalNS schema.Namespace = 65533
const metadataNS schema.Namespace = 65534

const (
	rootDataDir  = "data"
	rootIndexDir = "index"
)

var (
	ErrRecoveryRequired            = errors.New("database: index recovery required; journal retained")
	ErrCorruptJournal              = errors.New("database: corrupt mutation journal")
	ErrClosed                      = errors.New("database: closed")
	ErrJournalFull                 = errors.New("database: pending journal limit reached")
	ErrSequenceOverflow            = errors.New("database: generation overflow")
	ErrInvalidMutation             = errors.New("database: invalid mutation")
	ErrProjectionUnavailable       = errors.New("database: canonical projection is unavailable")
	ErrUnsupportedJournalVersion   = errors.New("database: unsupported journal version")
	ErrUnsupportedCanonicalVersion = errors.New("database: unsupported canonical record version")
	ErrCanonicalModeMismatch       = errors.New("database: canonical projection mode mismatch")
	ErrCorruptDatabaseConfig       = errors.New("database: corrupt canonical projection config")
)

// Decoder reconstructs an index projection from durable, application-encoded
// bytes. Its encoding must be stable across process restarts and contain every
// association required to remove or replace the previous indexed values.
// The database never interprets application-specific record contents.
type Decoder func([]byte) (index.Entry, error)

// DecodeScratch is optional decoder-owned transient storage. The database
// resets Bytes before each DecodeInto call. Decoded projections may borrow
// scratch.Bytes until the coordinator finishes the current journal batch; they
// must not retain it in index-owned state.
type DecodeScratch struct {
	Bytes []byte
}

// DecodeIntoFunc is the allocation-conscious decoder form. If configured it
// takes precedence over Decoder. dst is reset by the database before each call;
// implementations may append to dst's slices to reuse their capacity.
type DecodeIntoFunc func(encoded []byte, dst *index.Entry, scratch *DecodeScratch) error

// RecoveryPolicy controls startup handling of an unhealthy derived index.
// Automatic recovery is synchronous and builds a complete replacement before
// the DB becomes available; it never modifies the canonical records.
type RecoveryPolicy uint8

const (
	RecoveryManual RecoveryPolicy = iota
	RecoveryAutomatic
)

type Options struct {
	BatchSize  int    // defaults to 64; one publisher processes batches in order
	MaxPending uint64 // maximum unacknowledged mutations; zero means no limit
	Recovery   RecoveryPolicy
	// MaxProjectionBytes bounds the sum of copied before+after projection
	// payload bytes in one coordinator batch. Zero selects 1 MiB.
	MaxProjectionBytes int
	// OversizedRetentionBytes bounds the one reusable oversized projection
	// buffer retained by a coordinator. Zero selects 4 MiB.
	OversizedRetentionBytes int
	// DecodeInto, when non-nil, replaces Decoder for projections. It is
	// opt-in; existing callers can continue using Decoder.
	DecodeInto DecodeIntoFunc
	// CanonicalProjection selects inline (A) or external (B) canonical
	// projection storage. External requires ProjectFromRecord or ProjectInto.
	CanonicalProjection CanonicalProjectionMode
	// ProjectFromRecord re-derives complete projection bytes from an
	// application canonical record. It is required for external canonical
	// projections and for projection-semantic rebuilds.
	ProjectFromRecord func(record []byte) ([]byte, error)
	// ProjectInto is the destination-buffer form of ProjectFromRecord. When set
	// it is preferred over ProjectFromRecord for reprojection.
	ProjectInto func(dst, record []byte) ([]byte, error)
	// ProjectionMigration permits an explicit target schema to change
	// per-namespace ProjectionVersion and rebuild indexes from canonical
	// records using ProjectFromRecord or ProjectInto.
	ProjectionMigration bool
	// Definitions, if provided, is the authoritative complete namespace model.
	// Existing KV and index metadata must agree with it. When omitted on an
	// existing database, the persisted schema file is authoritative.
	Definitions *schema.Definitions
	// CanonicalNamespaces overrides the historical canonical KV namespace for
	// an entity. Zero retains its original assignment. Supply the same values
	// on every reopen; persisted definitions reject any reassignment.
	CanonicalNamespaces [schema.EntityKindCount + 1]schema.Namespace
	boundary            func(string) // deterministic process-crash injection in package tests
}

type DB struct {
	owner             *os.File   // serializes schema startup, including close/reopen transitions
	mu                sync.Mutex // serializes sequence allocation with the Pebble commit
	values            *store.Store
	ids               *schema.Schema
	canonical         [schema.EntityKindCount + 1]schema.Namespace
	idsClosed         atomic.Bool
	indexes           *index.Store
	decode            Decoder
	decodeInto        DecodeIntoFunc
	projectFromRecord func([]byte) ([]byte, error)
	projectInto       func([]byte, []byte) ([]byte, error)
	options           Options
	stats             dbStats
	commitScratch     projectionScratch
	journalScratch    []byte
	committed         uint64
	indexed           uint64
	inflight          uint64
	err               error
	closed            bool
	wake              chan struct{}
	changed           chan struct{}
	done              chan struct{}
}

// Open opens or creates the database rooted at root. Canonical Pebble data
// lives in root/data and every derived index lives in the shared root/index
// directory, including vector namespaces. On a new root, Options.Definitions
// is required and the index tree is created automatically; later opens reuse
// schema.dat and the existing index without supplying definitions.
func Open(root string, decode Decoder, options Options) (*DB, error) {
	if root == "" {
		return nil, store.ErrInvalidDir
	}
	root = filepath.Clean(root)
	if root == string(filepath.Separator) {
		return nil, fmt.Errorf("%w: unsafe database root", ErrInvalidMutation)
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, err
	}
	return OpenDirs(filepath.Join(root, rootDataDir), filepath.Join(root, rootIndexDir), decode, options)
}

// OpenDirs opens the canonical Pebble directory and its derived index directory
// explicitly. It is the advanced form of Open for applications that keep data
// and indexes at custom paths. On a fresh database, the index directory is
// created from Options.Definitions when provided; a pre-created index remains
// supported for legacy bootstrap. A reconciliation failure closes both stores
// and leaves the journal intact; ownership and recovery semantics are
// documented in database/README.md and database/FORMAT.md.
func OpenDirs(dir, indexDir string, decode Decoder, options Options) (*DB, error) {
	if decode == nil && options.DecodeInto == nil {
		return nil, ErrInvalidMutation
	}
	if dir == "" {
		return nil, store.ErrInvalidDir
	}
	if indexDir == "" || filepath.Clean(indexDir) == string(filepath.Separator) || filepath.Base(filepath.Clean(indexDir)) == "." {
		return nil, fmt.Errorf("%w: unsafe index directory", ErrInvalidMutation)
	}
	if options.BatchSize <= 0 {
		options.BatchSize = 64
	}
	if options.BatchSize > 64 {
		return nil, fmt.Errorf("%w: batch size > 64", ErrInvalidMutation)
	}
	if options.MaxProjectionBytes < 0 || options.OversizedRetentionBytes < 0 {
		return nil, fmt.Errorf("%w: negative projection limits", ErrInvalidMutation)
	}
	if options.MaxProjectionBytes == 0 {
		options.MaxProjectionBytes = defaultMaxProjectionBytes
	}
	if options.OversizedRetentionBytes == 0 {
		options.OversizedRetentionBytes = defaultOversizedRetentionBytes
	}
	if !options.CanonicalProjection.valid() {
		return nil, fmt.Errorf("%w: invalid canonical projection mode", ErrInvalidMutation)
	}
	if options.CanonicalProjection == CanonicalProjectionExternal && options.ProjectFromRecord == nil && options.ProjectInto == nil {
		return nil, fmt.Errorf("%w: external canonical projection requires ProjectFromRecord or ProjectInto", ErrInvalidMutation)
	}
	if options.ProjectionMigration && options.ProjectFromRecord == nil && options.ProjectInto == nil {
		return nil, fmt.Errorf("%w: projection migration requires ProjectFromRecord or ProjectInto", ErrInvalidMutation)
	}
	owner, err := storage.AcquireLock(filepath.Dir(dir), "."+filepath.Base(filepath.Clean(dir))+".database.lock", false)
	if err != nil {
		return nil, err
	}
	owned := true
	defer func() {
		if owned {
			_ = storage.ReleaseLock(owner)
		}
	}()
	var existing *schema.Schema
	var fresh bool
	path := filepath.Join(dir, "schema.dat")
	_, statErr := os.Stat(path)
	if errors.Is(statErr, os.ErrNotExist) {
		present, complete, err := store.InspectDatabase(dir)
		if err != nil {
			return nil, err
		}
		if complete {
			return nil, errors.Join(schema.ErrCorrupt, fmt.Errorf("database: missing authoritative schema.dat"))
		}
		fresh = !present
	} else if statErr != nil {
		return nil, statErr
	}
	if statErr == nil {
		var openErr error
		existing, openErr = schema.Open(path, schema.Options{})
		if openErr != nil {
			return nil, openErr
		}
		options, openErr = ensureCanonicalMode(existing, options)
		if openErr != nil {
			_ = existing.Close()
			return nil, openErr
		}
		if existing.PreparedDefinitions() != nil {
			if err := recoverPreparedSchema(existing, dir, indexDir, decode, options); err != nil {
				_ = existing.Close()
				return nil, err
			}
		}
	}
	if existing != nil && options.Definitions != nil && projectionVersionsDiffer(existing.Definitions(), options.Definitions) {
		if options.CanonicalProjection != CanonicalProjectionExternal || (options.ProjectFromRecord == nil && options.ProjectInto == nil) {
			_ = existing.Close()
			return nil, fmt.Errorf("%w: projection version change requires external canonical mode and a projector callback", ErrInvalidMutation)
		}
	}
	if existing != nil && options.Definitions != nil && validateDefinitions(existing.Definitions(), options.Definitions) != nil {
		var err error
		existing, err = prepareSchemaAddition(existing, path, dir, indexDir, options.Definitions, decode, options)
		if err != nil {
			if existing != nil {
				_ = existing.Close()
			}
			return nil, err
		}
	}
	canonical, err := canonicalNamespaces(options.CanonicalNamespaces)
	if existing != nil {
		canonical, err = persistedCanonical(existing.Definitions(), options.CanonicalNamespaces)
		if err == nil && options.Definitions != nil {
			err = validateDefinitions(existing.Definitions(), options.Definitions)
		}
	}
	if err != nil {
		if existing != nil {
			_ = existing.Close()
		}
		return nil, err
	}
	if fresh {
		// Create the shared index tree from the supplied definitions before
		// schema.dat exists. A pre-created legacy index is still accepted.
		if err := ensureIndexCreated(indexDir, options.Definitions); err != nil {
			return nil, err
		}
		// Validate the index before creating schema.dat. New Pebble directories
		// are opened only after the central file is durable; the fresh path
		// never writes a Pebble schema registry.
		var idx *index.Store
		if options.Definitions != nil {
			idx, err = index.OpenWithDefinitions(indexDir, options.Definitions)
		} else {
			idx, err = index.Open(indexDir)
		}
		if err != nil {
			return nil, err
		}
		defs := schema.NewDefinitions()
		for _, spec := range canonicalSpecsFor(canonical) {
			if spec.Entity.Valid() {
				if err = defs.Register(spec.NamespaceDef()); err != nil {
					break
				}
			}
		}
		if err == nil {
			for _, def := range idx.Schema().Defs() {
				if def.Namespace == journalNS || def.Namespace == metadataNS || def.Namespace == 0xFFF0 {
					err = schema.ErrNamespace
					break
				}
				if err = defs.Register(def); err != nil {
					break
				}
			}
		}
		if err == nil && options.Definitions != nil {
			err = validateIndexDefinitions(options.Definitions, idx.Schema())
		}
		closeErr := idx.Close()
		if err != nil {
			return nil, err
		}
		if closeErr != nil {
			return nil, closeErr
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
		entities := make([]schema.EntityConfig, 0, schema.EntityKindCount)
		for e := schema.EntityImage; e.Valid(); e++ {
			entities = append(entities, schema.EntityConfig{Entity: e, ReserveSize: 1000})
		}
		existing, err = schema.Open(path, schema.Options{Definitions: defs, Entities: entities})
		if err != nil {
			return nil, err
		}
		options, err = ensureCanonicalMode(existing, options)
		if err != nil {
			_ = existing.Close()
			return nil, err
		}
		// Definitions were consumed to create the central schema; the
		// persisted schema is authoritative from here on.
		options.Definitions = nil
	}
	var values *store.Store
	if existing != nil {
		if !fresh {
			present, _, err := store.InspectDatabase(dir)
			if err != nil {
				_ = existing.Close()
				return nil, err
			}
			if !present {
				_ = existing.Close()
				return nil, schema.ErrCorrupt
			}
		}
		kv, err := kvDefinitions(existing.Definitions())
		if err != nil {
			_ = existing.Close()
			return nil, err
		}
		values, err = store.Open(dir, kv)
		if err == nil && !existing.LegacyRetired() {
			err = values.ValidateLegacyDefinitions(kv)
			if err != nil {
				_ = values.Close()
			}
		}
	} else {
		values, err = store.OpenLegacyWithExtension(dir, canonicalSpecsFor(canonical)...)
	}
	if err != nil {
		if existing != nil {
			_ = existing.Close()
		}
		return nil, err
	}
	db, err := openWithValues(values, dir, indexDir, decode, options, existing, canonical)
	if err != nil {
		return nil, err
	}
	db.owner, owned = owner, false
	return db, nil
}

func ensureIndexCreated(indexDir string, definitions *schema.Definitions) error {
	if _, err := os.Stat(filepath.Join(indexDir, "store.bin")); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if definitions == nil {
		return fmt.Errorf("%w: a new database requires Options.Definitions", ErrInvalidMutation)
	}
	idx, err := index.CreateWithDefinitions(indexDir, definitions)
	if err != nil {
		return err
	}
	return idx.Close()
}

func canonicalSpecs() []store.Spec {
	canonical, _ := canonicalNamespaces([schema.EntityKindCount + 1]schema.Namespace{})
	return canonicalSpecsFor(canonical)
}

func canonicalNamespaces(overrides [schema.EntityKindCount + 1]schema.Namespace) ([schema.EntityKindCount + 1]schema.Namespace, error) {
	var canonical [schema.EntityKindCount + 1]schema.Namespace
	var seen [1 << 16]bool
	for e := schema.EntityImage; e.Valid(); e++ {
		ns := overrides[e]
		if ns == 0 {
			ns = schema.Namespace(e)
		} // historical persisted assignment
		if ns == journalNS || ns == metadataNS || ns == 0xFFF0 || seen[ns] {
			return canonical, fmt.Errorf("%w: invalid or duplicate canonical namespace %d", ErrInvalidMutation, ns)
		}
		seen[ns] = true
		canonical[e] = ns
	}
	return canonical, nil
}

func persistedCanonical(defs *schema.Definitions, overrides [schema.EntityKindCount + 1]schema.Namespace) ([schema.EntityKindCount + 1]schema.Namespace, error) {
	var canonical [schema.EntityKindCount + 1]schema.Namespace
	var seen [1 << 16]bool
	for _, def := range defs.DefsOfKind(schema.KindKV) {
		for e := schema.EntityImage; e.Valid(); e++ {
			if def.Name != "entity-"+e.String() {
				continue
			}
			if canonical[e] != 0 || seen[def.Namespace] || def.Entity != e {
				return canonical, schema.ErrNamespace
			}
			canonical[e], seen[def.Namespace] = def.Namespace, true
		}
	}
	for e := schema.EntityImage; e.Valid(); e++ {
		if canonical[e] == 0 || (overrides[e] != 0 && overrides[e] != canonical[e]) ||
			canonical[e] == journalNS || canonical[e] == metadataNS || canonical[e] == 0xFFF0 {
			return canonical, schema.ErrNamespace
		}
	}
	return canonical, nil
}

func canonicalSpecsFor(canonical [schema.EntityKindCount + 1]schema.Namespace) []store.Spec {
	specs := make([]store.Spec, 0, schema.EntityKindCount+2)
	for e := schema.EntityImage; e <= schema.EntityStack; e++ {
		specs = append(specs, store.Spec{Namespace: canonical[e], Name: "entity-" + e.String(), Entity: e})
	}
	specs = append(specs, store.Spec{Namespace: journalNS, Name: "mutation-journal"}, store.Spec{Namespace: metadataNS, Name: "database-generations"})
	// Append new entity namespaces after the historical schema entries: the
	// explicit additive schema upgrade checks their stable original prefix.
	for e := schema.EntityUser; e.Valid(); e++ {
		specs = append(specs, store.Spec{Namespace: canonical[e], Name: "entity-" + e.String(), Entity: e})
	}
	return specs
}

func openWithValues(values *store.Store, dir, indexDir string, decode Decoder, options Options, existing *schema.Schema, canonical [schema.EntityKindCount + 1]schema.Namespace) (*DB, error) {
	if existing != nil {
		var err error
		options, err = ensureCanonicalMode(existing, options)
		if err != nil {
			_ = existing.Close()
			_ = values.Close()
			return nil, err
		}
	}
	if existing != nil && !existing.LegacyRetired() {
		stored, err := values.Get(store.Key{Namespace: metadataNS, ID: schemaKeyID})
		if err == nil {
			backup := schema.NewDefinitions()
			if err = backup.UnmarshalBinary(stored); err == nil {
				err = validateLegacyIndexCopy(existing.Definitions(), backup)
			}
		} else if errors.Is(err, store.ErrNotFound) {
			err = nil
		}
		if err != nil {
			_ = existing.Close()
			_ = values.Close()
			return nil, errors.Join(ErrRecoveryRequired, err)
		}
	}
	// A prior activation must be completed before reading the active index.
	var authoritative *schema.Definitions
	if existing != nil {
		authoritative = existing.Definitions()
	}
	if err := resumeActivation(indexDir, values, authoritative, options.boundary); err != nil {
		if existing != nil {
			_ = existing.Close()
		}
		_ = values.Close()
		return nil, err
	}
	var idx *index.Store
	var err error
	if authoritative != nil {
		idx, err = index.OpenWithDefinitions(indexDir, authoritative)
	} else {
		idx, err = index.Open(indexDir) // one-time legacy bootstrap
	}
	openErr := err
	if err != nil && options.Recovery != RecoveryAutomatic {
		if existing != nil {
			_ = existing.Close()
		}
		_ = values.Close()
		return nil, errors.Join(ErrRecoveryRequired, err)
	}
	db := &DB{values: values, ids: existing, indexes: idx, canonical: canonical, decode: decode, decodeInto: options.DecodeInto, projectFromRecord: options.ProjectFromRecord, projectInto: options.ProjectInto, options: options,
		wake: make(chan struct{}, 1), changed: make(chan struct{}), done: make(chan struct{})}
	ready := false
	defer func() {
		if !ready {
			if db.ids != nil {
				_ = db.ids.Close()
			}
			if db.indexes != nil {
				_ = db.indexes.Close()
			}
			_ = values.Close()
		}
	}()
	if db.committed, err = db.readGeneration(1); err != nil {
		return nil, err
	}
	if db.indexed, err = db.readGeneration(2); err != nil {
		return nil, err
	}
	if db.inflight, err = db.readGeneration(3); err != nil {
		return nil, err
	}
	if options.Recovery == RecoveryAutomatic && (openErr != nil || db.needsRecovery()) {
		if err := db.rebuildOnOpen(indexDir); err != nil {
			return nil, err
		}
	}
	if db.indexes == nil {
		return nil, fmt.Errorf("%w: index cannot open: %v", ErrRecoveryRequired, openErr)
	}
	// On the bootstrap path, reject a conflicting legacy index backup before
	// creating the central file or publishing the allocator migration marker.
	hadSchema := db.ids != nil
	if !hadSchema {
		if err := db.persistSchema(); err != nil {
			return nil, err
		}
	}
	if err := db.openIDs(dir); err != nil {
		return nil, err
	}
	idx = db.indexes
	safe := idx.SourceGeneration()
	if db.indexed > db.committed || safe < db.indexed || len(idx.RecoveryNamespaces()) != 0 || len(idx.DegradedNamespaces()) != 0 {
		return nil, fmt.Errorf("%w: database=%d acknowledged=%d index=%d", ErrRecoveryRequired, db.committed, db.indexed, safe)
	}
	for _, h := range idx.Health() {
		if !h.Available {
			return nil, fmt.Errorf("%w: namespace %d: %v", ErrRecoveryRequired, h.Namespace, h.Err)
		}
	}
	// The index can be ahead of the Pebble acknowledgment if the process died
	// after its durable watermark but before the acknowledgment commit.
	if safe > db.indexed {
		if safe > db.committed {
			return nil, fmt.Errorf("%w: index ahead of database", ErrRecoveryRequired)
		}
		if err = db.acknowledge(safe); err != nil {
			return nil, err
		}
	}
	if db.inflight > db.indexed {
		// A partial index batch cannot be replayed blindly: some engines have
		// strict before-state preconditions and simhash has dense append IDs.
		return nil, fmt.Errorf("%w: interrupted index batch ending at %d", ErrRecoveryRequired, db.inflight)
	}
	if db.indexed != 0 {
		if err = db.cleanup(0, db.indexed); err != nil {
			return nil, err
		}
	}
	// The central allocator and logical schema are now durable, and the
	// database and index have passed consistency checks. Retire legacy copies
	// one at a time; an interrupted cleanup is safe to repeat on the next Open.
	if hadSchema == false {
		if err := db.indexes.Close(); err != nil {
			return nil, err
		}
		db.indexes = nil
		db.indexes, err = index.OpenWithDefinitions(indexDir, db.ids.Definitions())
		if err != nil {
			return nil, err
		}
	}
	if !db.ids.LegacyRetired() {
		if err := values.RemoveLegacyDefinitions(); err != nil {
			return nil, err
		}
		if options.boundary != nil {
			options.boundary("after legacy Pebble schema cleanup")
		}
		if _, err := values.Get(store.Key{Namespace: metadataNS, ID: schemaKeyID}); err == nil {
			if err := values.Delete(store.Key{Namespace: metadataNS, ID: schemaKeyID}); err != nil {
				return nil, err
			}
		} else if !errors.Is(err, store.ErrNotFound) {
			return nil, err
		}
		if options.boundary != nil {
			options.boundary("after legacy index backup cleanup")
		}
		if err := db.indexes.RetireLegacySchema(); err != nil {
			return nil, err
		}
		if options.boundary != nil {
			options.boundary("after legacy index schema cleanup")
		}
		if err := db.ids.MarkLegacyRetired(); err != nil {
			return nil, err
		}
		if options.boundary != nil {
			options.boundary("after legacy retirement sync")
		}
	}
	go db.run()
	if db.committed > db.indexed {
		db.signal()
	}
	ready = true
	return db, nil
}

func (db *DB) readGeneration(id uint64) (uint64, error) {
	v, err := db.values.Get(store.Key{Namespace: metadataNS, ID: id})
	if errors.Is(err, store.ErrNotFound) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	if len(v) != 8 {
		return 0, ErrCorruptJournal
	}
	return binary.BigEndian.Uint64(v), nil
}

func putGeneration(b *store.Batch, id, gen uint64) error {
	var v [8]byte
	binary.BigEndian.PutUint64(v[:], gen)
	return b.Set(store.Key{Namespace: metadataNS, ID: id}, v[:])
}

func (db *DB) signal() {
	select {
	case db.wake <- struct{}{}:
	default:
	}
}

func (db *DB) notify() { close(db.changed); db.changed = make(chan struct{}) }

// Mutation is one canonical record operation. Projection is the durable binary
// representation of the *complete* index.Entry for this entity after a Put;
// Delete ignores it. The caller owns Record and Projection until Commit returns.
type Mutation struct {
	Entity     schema.EntityKind
	ValueID    schema.ValueID
	Delete     bool
	Record     []byte
	Projection []byte
}

// Commit atomically writes canonical records, ordered journal entries, and the
// next sequence in one synced Pebble batch. Returns the last generation.
// Explicit ValueIDs are caller-owned: an existing ID updates that record, and
// a new ID creates it. EnsureMinimum prevents future allocator reuse but cannot
// prevent collision with IDs already issued to concurrent callers. Bulk import
// must be serialized by the caller before allowing NextID on that entity;
// runtime explicit inserts use last-writer-wins canonical record semantics.
// A failed commit does not consume a sequence. Search is eventually consistent;
// pass the returned generation to WaitIndexed before requiring fresh results.
func (db *DB) Commit(mutations ...Mutation) (uint64, error) {
	db.mu.Lock()
	defer db.mu.Unlock()
	if db.closed {
		return 0, ErrClosed
	}
	if db.err != nil {
		return 0, db.err
	}
	if len(mutations) == 0 {
		return db.committed, ErrInvalidMutation
	}
	if uint64(len(mutations)) > math.MaxUint64-db.committed {
		return 0, ErrSequenceOverflow
	}
	if db.options.MaxPending > 0 && (db.committed-db.indexed >= db.options.MaxPending || uint64(len(mutations)) > db.options.MaxPending-(db.committed-db.indexed)) {
		return 0, ErrJournalFull
	}
	b := db.values.NewBatch()
	defer b.Close()
	for i, m := range mutations {
		for j := 0; j < i; j++ {
			if mutations[j].Entity == m.Entity && mutations[j].ValueID == m.ValueID {
				return 0, ErrInvalidMutation // avoid reading a stale pre-batch projection
			}
		}
		if !m.Entity.Valid() || m.ValueID == 0 || (!m.Delete && len(m.Projection) == 0) {
			return 0, ErrInvalidMutation
		}
		var afterEntry index.Entry
		if !m.Delete {
			var err error
			afterEntry, err = db.decodeEntry(m.Projection, m.Entity, m.ValueID)
			if err != nil {
				return 0, err
			}
		}
		key := store.Key{Namespace: db.canonical[m.Entity], ID: m.ValueID}
		prior, err := db.values.Get(key)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return 0, err
		}
		priorExists := err == nil
		priorTombstone := false
		var before []byte
		var beforeRecord []byte
		if priorExists {
			value, err := decodeCanonicalValue(prior)
			if err != nil {
				return 0, err
			}
			priorTombstone = value.tombstone
			before, err = db.projectionForValue(value)
			if err != nil {
				return 0, err
			}
			beforeRecord = value.record
		}
		// A value written over a tombstone is semantically a reinsertion even
		// though the prior value still carries the old projection. It must add
		// liveness and remove associations that are absent from the new
		// projection.
		insert := !priorExists || priorTombstone
		if m.Delete {
			insert = false
		}
		var beforeEntry index.Entry
		if priorExists && !m.Delete {
			var err error
			beforeEntry, err = db.decodeEntry(before, m.Entity, m.ValueID)
			if err != nil {
				return 0, err
			}
		}
		if m.Delete {
			// Keep the old projection (inline) or old application record
			// (external) as a compact tombstone for correct reinsertion.
			tombstoneProjection, tombstoneRecord := before, beforeRecord
			if db.options.CanonicalProjection == CanonicalProjectionExternal {
				tombstoneProjection = nil
			}
			tombstone, err := encodeCanonicalValue(canonicalValue{
				tombstone:  true,
				version:    canonicalVersionForMode(db.options.CanonicalProjection),
				projection: tombstoneProjection,
				record:     tombstoneRecord,
			})
			if err != nil {
				return 0, err
			}
			if !bytes.Equal(tombstone, prior) {
				if err := b.Set(key, tombstone); err != nil {
					return 0, err
				}
			}
		} else {
			if len(m.Projection) > math.MaxUint32 {
				return 0, ErrInvalidMutation
			}
			projection := m.Projection
			if db.options.CanonicalProjection == CanonicalProjectionExternal {
				projection = nil
			}
			record, err := encodeCanonicalValue(canonicalValue{
				version:    canonicalVersionForMode(db.options.CanonicalProjection),
				projection: projection,
				record:     m.Record,
			})
			if err != nil {
				return 0, err
			}
			if !bytes.Equal(record, prior) {
				if err := b.Set(key, record); err != nil {
					return 0, err
				}
			}
		}
		deltaBefore, deltaAfter := beforeEntry, afterEntry
		if m.Delete {
			deltaBefore, deltaAfter = index.Entry{}, index.Entry{}
		} else if !insert {
			if bytes.Equal(before, m.Projection) {
				deltaBefore, deltaAfter = index.Entry{}, index.Entry{}
			} else {
				deltaBefore, deltaAfter = changedProjectionScratch(beforeEntry, afterEntry, &db.commitScratch)
			}
		}
		entry, err := appendMutation(db.journalScratch[:0], m, insert, deltaBefore, deltaAfter)
		db.journalScratch = entry
		db.commitScratch.reset()
		if err != nil {
			return 0, err
		}
		db.stats.journalEntries.Add(1)
		db.stats.journalBytes.Add(uint64(len(entry)))
		if err := b.Set(store.Key{Namespace: journalNS, ID: db.committed + uint64(i) + 1}, entry); err != nil {
			return 0, err
		}
	}
	// Reconcile explicit IDs before publishing canonical records, including
	// deletions. This protects *future* reservations only, not IDs already
	// issued to a concurrent caller (see Commit's conflict policy).
	for _, m := range mutations {
		if err := db.ids.EnsureMinimum(m.Entity, m.ValueID); err != nil {
			return 0, err
		}
	}
	gen := db.committed + uint64(len(mutations))
	if err := putGeneration(b, 1, gen); err != nil {
		return 0, err
	}
	if db.options.boundary != nil {
		db.options.boundary("before canonical commit")
	}
	if err := b.Commit(); err != nil {
		// A storage error can leave commit outcome ambiguous to the caller.
		// Do not reuse its sequence in this process; reopen and reconcile.
		db.err = fmt.Errorf("%w: canonical commit: %v", ErrRecoveryRequired, err)
		db.notify()
		return 0, db.err
	}
	if db.options.boundary != nil {
		db.options.boundary("after canonical commit")
	}
	db.committed = gen
	db.stats.canonicalCommits.Add(1)
	db.notify()
	db.signal()
	return gen, nil
}

// Get returns an owned copy of the authoritative record, without projection.
func (db *DB) Get(entity schema.EntityKind, id schema.ValueID) ([]byte, error) {
	if !entity.Valid() || id == 0 {
		return nil, ErrInvalidMutation
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	if db.closed {
		return nil, ErrClosed
	}
	v, err := db.values.Get(store.Key{Namespace: db.canonical[entity], ID: id})
	if err != nil {
		return nil, err
	}
	value, err := decodeCanonicalValue(v)
	if err != nil {
		return nil, err
	}
	if value.tombstone {
		return nil, store.ErrNotFound
	}
	return append([]byte(nil), value.record...), nil
}

// DatabaseGeneration returns the latest committed canonical/journal sequence.
func (db *DB) DatabaseGeneration() uint64 { db.mu.Lock(); defer db.mu.Unlock(); return db.committed }

// IndexedGeneration returns the last sequence durably acknowledged as indexed.
// It never exceeds DatabaseGeneration.
func (db *DB) IndexedGeneration() uint64 { db.mu.Lock(); defer db.mu.Unlock(); return db.indexed }

// Index returns the live derived index store. It is an advanced read accessor:
// do not write to or close the returned store while the database coordinator is
// running. Search plans bind with search.New(db.Index()).
func (db *DB) Index() *index.Store { return db.indexes }

// NextID reserves a unique entity ID before its canonical record is committed.
// Abandoned IDs are never recycled. Close rejects new calls and drains those
// already admitted before closing the backing Pebble database.
func (db *DB) NextID(entity schema.EntityKind) (schema.ValueID, error) {
	if !entity.Valid() {
		return 0, schema.ErrNamespace
	}
	if db.idsClosed.Load() {
		return 0, schema.ErrClosed
	}
	return db.ids.NextID(entity)
}

// PrepareImport establishes the maximum ID of a completed bulk import before
// concurrent allocation begins. Callers must exclude NextID/Commit for this
// entity while reconciling; this method does not revoke already issued IDs.
// It is safe to repeat after a crash, and never reduces the durable watermark.
func (db *DB) PrepareImport(entity schema.EntityKind, maximum schema.ValueID) error {
	if !entity.Valid() {
		return ErrInvalidMutation
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	if db.closed {
		return ErrClosed
	}
	return db.ids.EnsureMinimum(entity, maximum)
}

// WaitIndexed waits without polling or per-waiter goroutines. Permanent
// coordinator failures wake all waiters and preserve the unacknowledged journal.
func (db *DB) WaitIndexed(ctx context.Context, generation uint64) error {
	for {
		db.mu.Lock()
		if generation > db.committed {
			db.mu.Unlock()
			return ErrInvalidMutation
		}
		if db.indexed >= generation {
			db.mu.Unlock()
			return nil
		}
		if db.err != nil {
			err := db.err
			db.mu.Unlock()
			return err
		}
		if db.closed {
			db.mu.Unlock()
			return ErrClosed
		}
		ch := db.changed
		db.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// Sync waits for every generation committed before this call to be indexed.
func (db *DB) Sync(ctx context.Context) error { return db.WaitIndexed(ctx, db.DatabaseGeneration()) }

// Checkpoint publishes the current derived-index state and truncates engine
// journals where supported. It is an explicit maintenance hook; canonical
// Pebble data is already durable. Callers should run it during idle periods.
func (db *DB) Checkpoint() error {
	db.mu.Lock()
	defer db.mu.Unlock()
	if db.closed {
		return ErrClosed
	}
	if db.err != nil {
		return db.err
	}
	if db.indexes == nil {
		return ErrRecoveryRequired
	}
	return db.indexes.Checkpoint()
}

// Close stops new operations, drains the indexing coordinator, closes the
// derived index and canonical Pebble store, and releases database locks. Active
// search snapshots may delay index closure; drain long-running queries before
// calling Close. Close is idempotent.
func (db *DB) Close() error {
	db.mu.Lock()
	if db.closed {
		db.mu.Unlock()
		return nil
	}
	db.closed = true
	db.idsClosed.Store(true)
	db.notify()
	db.signal()
	db.mu.Unlock()
	idsErr := db.ids.Close()
	<-db.done
	return errors.Join(idsErr, db.indexes.Close(), db.values.Close(), storage.ReleaseLock(db.owner))
}
