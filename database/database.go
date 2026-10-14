// Package database coordinates authoritative Pebble records with a derived
// unified index. Canonical commits are durable before their indexing completes.
package database

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"sync"
	"sync/atomic"

	"github.com/evanoberholster/imageIdx/index"
	"github.com/evanoberholster/imageIdx/schema"
	"github.com/evanoberholster/imageIdx/store"
)

const journalNS schema.Namespace = 65533
const metadataNS schema.Namespace = 65534

var (
	ErrRecoveryRequired = errors.New("database: index recovery required; journal retained")
	ErrCorruptJournal   = errors.New("database: corrupt mutation journal")
	ErrClosed           = errors.New("database: closed")
	ErrJournalFull      = errors.New("database: pending journal limit reached")
	ErrSequenceOverflow = errors.New("database: generation overflow")
	ErrInvalidMutation  = errors.New("database: invalid mutation")
)

// Decoder reconstructs an index projection from durable, application-encoded
// bytes. Its encoding must be stable across process restarts and contain every
// association required to remove or replace the previous indexed values.
// The database never interprets application-specific record contents.
type Decoder func([]byte) (index.Entry, error)

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
	boundary   func(string) // deterministic process-crash injection in package tests
}

type DB struct {
	mu        sync.Mutex // serializes sequence allocation with the Pebble commit
	values    *store.Store
	ids       *schema.Schema
	idsClosed atomic.Bool
	indexes   *index.Store
	decode    Decoder
	options   Options
	committed uint64
	indexed   uint64
	inflight  uint64
	err       error
	closed    bool
	wake      chan struct{}
	changed   chan struct{}
	done      chan struct{}
}

// Open opens the canonical Pebble directory and its derived index directory.
// The index must have been created with index.Create before the first Open.
// A reconciliation failure closes both stores and leaves the journal intact.
func Open(dir, indexDir string, decode Decoder, options Options) (*DB, error) {
	if decode == nil {
		return nil, ErrInvalidMutation
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
	values, err := store.OpenWithExtension(dir, canonicalSpecs()...)
	if err != nil {
		return nil, err
	}
	return openWithValues(values, dir, indexDir, decode, options)
}

func canonicalSpecs() []store.Spec {
	specs := make([]store.Spec, 0, schema.EntityKindCount+2)
	for e := schema.EntityImage; e <= schema.EntityStack; e++ {
		specs = append(specs, store.Spec{Namespace: schema.Namespace(e), Name: "entity-" + e.String(), Entity: e})
	}
	specs = append(specs, store.Spec{Namespace: journalNS, Name: "mutation-journal"}, store.Spec{Namespace: metadataNS, Name: "database-generations"})
	// Append new entity namespaces after the historical schema entries: the
	// explicit additive schema upgrade checks their stable original prefix.
	for e := schema.EntityUser; e.Valid(); e++ {
		specs = append(specs, store.Spec{Namespace: schema.Namespace(e), Name: "entity-" + e.String(), Entity: e})
	}
	return specs
}

func openWithValues(values *store.Store, dir, indexDir string, decode Decoder, options Options) (*DB, error) {
	// A prior activation must be completed before reading the active index.
	if err := resumeActivation(indexDir, values, options.boundary); err != nil {
		_ = values.Close()
		return nil, err
	}
	idx, err := index.Open(indexDir)
	openErr := err
	if err != nil && options.Recovery != RecoveryAutomatic {
		_ = values.Close()
		return nil, errors.Join(ErrRecoveryRequired, err)
	}
	db := &DB{values: values, indexes: idx, decode: decode, options: options,
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
	if err := db.persistSchema(); err != nil {
		return nil, err
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
		if !m.Delete {
			if _, err := db.decodeEntry(m.Projection, m.Entity, m.ValueID); err != nil {
				return 0, err
			}
		}
		key := store.Key{Namespace: schema.Namespace(m.Entity), ID: m.ValueID}
		prior, err := db.values.Get(key)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return 0, err
		}
		if err == nil && len(prior) < 5 {
			return 0, ErrCorruptJournal
		}
		var before []byte
		if err == nil {
			n := binary.BigEndian.Uint32(prior[1:5])
			if prior[0] > 1 || uint64(n) > uint64(len(prior)-5) {
				return 0, ErrCorruptJournal
			}
			before = prior[5 : 5+n]
		}
		if m.Delete {
			// Keep the old projection as a compact tombstone. A later insert
			// of this ID must invalidate old non-ID-authoritative hash and
			// bitmap associations before becoming live again.
			tombstone := make([]byte, 5+len(before))
			tombstone[0] = 1
			binary.BigEndian.PutUint32(tombstone[1:], uint32(len(before)))
			copy(tombstone[5:], before)
			if err := b.Set(key, tombstone); err != nil {
				return 0, err
			}
		} else {
			if len(m.Projection) > math.MaxUint32 {
				return 0, ErrInvalidMutation
			}
			record := make([]byte, 5+len(m.Projection)+len(m.Record))
			binary.BigEndian.PutUint32(record[1:], uint32(len(m.Projection)))
			copy(record[5:], m.Projection)
			copy(record[5+len(m.Projection):], m.Record)
			if err := b.Set(key, record); err != nil {
				return 0, err
			}
		}
		entry, err := encodeMutation(m, before)
		if err != nil {
			return 0, err
		}
		if err := b.Set(store.Key{Namespace: journalNS, ID: db.committed + uint64(i) + 1}, entry); err != nil {
			return 0, err
		}
	}
	// Legacy callers may provide IDs instead of obtaining them via NextID.
	// Reconcile them before publishing canonical records, including deletions,
	// so neither reopening nor future reservations can hand them out again.
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
	v, err := db.values.Get(store.Key{Namespace: schema.Namespace(entity), ID: id})
	if err != nil {
		return nil, err
	}
	if len(v) < 5 || v[0] > 1 {
		return nil, ErrCorruptJournal
	}
	if v[0] == 1 {
		return nil, store.ErrNotFound
	}
	n := binary.BigEndian.Uint32(v[1:5])
	if uint64(n) > uint64(len(v)-5) {
		return nil, ErrCorruptJournal
	}
	return v[5+n:], nil
}

func (db *DB) DatabaseGeneration() uint64 { db.mu.Lock(); defer db.mu.Unlock(); return db.committed }
func (db *DB) IndexedGeneration() uint64  { db.mu.Lock(); defer db.mu.Unlock(); return db.indexed }
func (db *DB) Index() *index.Store        { return db.indexes }

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
	return errors.Join(idsErr, db.indexes.Close(), db.values.Close())
}
