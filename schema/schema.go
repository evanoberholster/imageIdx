// Package schema owns namespace definitions and crash-safe per-entity ValueIDs.
package schema

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"

	"github.com/evanoberholster/imageIdx/internal/storage"
)

type Backend uint8

const (
	BackendReadWrite Backend = iota
	BackendMmap
)

type EntityConfig struct {
	Entity      EntityKind
	ReserveSize uint64
}

type Options struct {
	Backend     Backend
	Definitions *Definitions
	Entities    []EntityConfig
	// Prepare publishes additive definitions without activating them. The
	// caller must verify dependent resources and call Activate before use.
	Prepare bool
	fault   func(string) error // deterministic crash/failure injection in package tests
}

type entityAllocator struct {
	id     Namespace
	name   string
	block  uint64
	offset uint64
	mu     sync.Mutex
	issued atomic.Uint64
	limit  atomic.Uint64
	record allocationRecord // guarded by mu; never read on the allocation fast path
}

// Schema owns immutable namespace definitions and entity-scoped allocation.
// The backing file is locked for its entire lifetime.
type Schema struct {
	f                 *os.File
	lock              *os.File
	mapped            *storage.MappedFile
	backend           Backend
	entries           []*entityAllocator
	owners            [EntityKindCount + 1]*entityAllocator
	definitions       *Definitions
	definitionsData   []byte
	activeDefinitions *Definitions // non-nil while a prepared target is pending
	prepared          bool
	legacyRetired     bool
	generation        uint64
	directoryOffset   uint64
	directoryEnd      uint64
	directory         []byte
	state             atomic.Uint64 // closing bit | admitted operations
	poisoned          atomic.Bool
	closeMu           sync.Mutex
	closeCond         *sync.Cond
	closeOnce         sync.Once
	closeErr          error
	fault             func(string) error
}

const closing uint64 = 1 << 63

func configs(opts Options) ([]directoryEntry, *Definitions, []byte, error) {
	if opts.Backend != BackendReadWrite && opts.Backend != BackendMmap {
		return nil, nil, nil, fmt.Errorf("schema: invalid backend %d", opts.Backend)
	}
	if opts.Definitions == nil {
		if len(opts.Entities) != 0 {
			return nil, nil, nil, ErrNamespaceEntity
		}
		return nil, nil, nil, nil // recover definitions from an existing file
	}
	if opts.Definitions.Len() == 0 {
		return nil, nil, nil, ErrNamespace
	}
	data, err := opts.Definitions.MarshalBinary()
	if err != nil {
		return nil, nil, nil, err
	}
	clone := NewDefinitions()
	if err := clone.UnmarshalBinary(data); err != nil {
		return nil, nil, nil, err
	}
	var used [EntityKindCount + 1]bool
	var configured [EntityKindCount + 1]bool
	var blocks [EntityKindCount + 1]uint64
	for _, def := range clone.Defs() {
		if !def.Entity.Valid() {
			return nil, nil, nil, ErrNamespaceEntity
		}
		used[def.Entity] = true
	}
	for _, cfg := range opts.Entities {
		if !cfg.Entity.Valid() || !used[cfg.Entity] || configured[cfg.Entity] {
			return nil, nil, nil, ErrNamespaceEntity
		}
		configured[cfg.Entity] = true
		blocks[cfg.Entity] = cfg.ReserveSize
	}
	entries := make([]directoryEntry, 0, EntityKindCount)
	for e := EntityImage; e.Valid(); e++ {
		if !used[e] {
			continue
		}
		block := blocks[e]
		if block == 0 {
			block = defaultReserve
		}
		entries = append(entries, directoryEntry{id: Namespace(e), name: e.String(), reserve: block})
	}
	return entries, clone, data, nil
}

// Open opens path as the single allocation file (not a directory). Existing
// definitions must match their stored prefix; new namespaces and reservation
// sizes publish a new checksummed directory before returning to callers.
func Open(path string, opts Options) (_ *Schema, err error) {
	if opts.Prepare && opts.Definitions == nil {
		return nil, ErrNamespace
	}
	defs, definitions, definitionsData, err := configs(opts)
	if err != nil {
		return nil, err
	}
	if path == "" || filepath.Base(path) == "." {
		return nil, ErrCorrupt
	}
	lock, err := storage.AcquireLock(filepath.Dir(path), filepath.Base(path)+".lock", false)
	if err != nil {
		if errors.Is(err, storage.ErrBusy) {
			return nil, errors.Join(ErrLocked, err)
		}
		return nil, err
	}
	s := &Schema{lock: lock, backend: opts.Backend, fault: opts.fault, definitions: definitions, definitionsData: definitionsData}
	s.closeCond = sync.NewCond(&s.closeMu)
	defer func() {
		if err != nil {
			if s.mapped != nil {
				_ = s.mapped.Close()
			}
			if s.f != nil {
				_ = s.f.Close()
			}
			_ = storage.ReleaseLock(lock)
		}
	}()
	var f *os.File
	var openErr error
	if definitions == nil || opts.Prepare {
		f, openErr = os.OpenFile(path, os.O_RDWR, 0)
	} else {
		f, openErr = os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	}
	created := definitions != nil && !opts.Prepare && openErr == nil
	if definitions != nil && errors.Is(openErr, os.ErrExist) {
		f, openErr = os.OpenFile(path, os.O_RDWR, 0)
	}
	if openErr != nil {
		return nil, openErr
	}
	s.f = f
	if created {
		err = s.create(defs)
	} else {
		err = s.recover(defs, opts.Prepare)
	}
	if err != nil {
		return nil, err
	}
	if opts.Backend == BackendMmap {
		s.mapped, err = storage.OpenMapped(path)
		if err != nil {
			return nil, err
		}
		if err = s.f.Close(); err != nil {
			return nil, err
		}
		s.f = nil
	}
	return s, nil
}

func (s *Schema) entry(entity EntityKind) *entityAllocator {
	if !entity.Valid() {
		return nil
	}
	return s.owners[entity]
}

// Lookup returns a persisted namespace definition and its owning entity.
func (s *Schema) Lookup(ns Namespace) (NamespaceDef, bool) {
	if s.prepared {
		return s.activeDefinitions.Lookup(ns)
	}
	return s.definitions.Lookup(ns)
}

// Definitions returns an independent copy of the persisted namespace model.
func (s *Schema) Definitions() *Definitions {
	d := NewDefinitions()
	source := s.definitions
	if s.prepared {
		source = s.activeDefinitions
	}
	_ = d.RegisterAll(source.Defs()...)
	return d
}

// PreparedDefinitions returns the durable target during startup migration.
// It is nil for an active schema. The returned definitions are independent.
func (s *Schema) PreparedDefinitions() *Definitions {
	if !s.prepared {
		return nil
	}
	d := NewDefinitions()
	_ = d.RegisterAll(s.definitions.Defs()...)
	return d
}

// Generation is the active logical schema generation (not an ID watermark).
func (s *Schema) Generation() uint64 {
	if s.prepared {
		return s.generation - 1
	}
	return s.generation
}

// ReservationSize reports an entity's durable reservation-block setting for
// startup schema evolution; zero means the entity is not yet defined.
func (s *Schema) ReservationSize(entity EntityKind) uint64 {
	if a := s.entry(entity); a != nil {
		return a.block
	}
	return 0
}

// LegacyRetired reports whether the old Pebble and index logical schema copies
// were durably retired. An active database with this marker skips legacy reads.
func (s *Schema) LegacyRetired() bool { return s.legacyRetired }

// MarkLegacyRetired publishes a synced marker only after migration-only code
// has checked and removed every old schema copy. Repeating it is harmless.
func (s *Schema) MarkLegacyRetired() error {
	if s.state.Load()&closing != 0 {
		return ErrClosed
	}
	if s.prepared || s.poisoned.Load() {
		return ErrNamespace
	}
	if s.legacyRetired {
		return nil
	}
	if s.generation == ^uint64(0) {
		return ErrExhausted
	}
	if err := s.commitDirectory(int(s.generation%2), s.generation+1, s.directoryOffset, s.directory, s.definitionsData, s.directoryEnd, false, true); err != nil {
		s.poisoned.Store(true)
		return err
	}
	s.generation++
	s.legacyRetired = true
	return nil
}

// Activate publishes the prepared definitions after the coordinator has
// verified dependent resources. It is only valid during startup on a
// ReadAt/WriteAt opener; mmap readers may reopen after activation. A failed
// publication must be resolved by reopening before the database is exposed.
func (s *Schema) Activate() error {
	if s.state.Load()&closing != 0 {
		return ErrClosed
	}
	if !s.prepared || s.poisoned.Load() {
		return ErrNamespace
	}
	if s.generation == ^uint64(0) {
		return ErrExhausted
	}
	if err := s.commitDirectory(int(s.generation%2), s.generation+1, s.directoryOffset, s.directory, s.definitionsData, s.directoryEnd, false, s.legacyRetired); err != nil {
		s.poisoned.Store(true)
		return err
	}
	s.generation++
	s.prepared, s.activeDefinitions = false, nil
	return nil
}

func (s *Schema) admit() error {
	for {
		state := s.state.Load()
		if state&closing != 0 {
			return ErrClosed
		}
		if s.state.CompareAndSwap(state, state+1) {
			break
		}
	}
	if s.poisoned.Load() {
		s.release()
		return ErrCorrupt
	}
	return nil
}

func (s *Schema) release() {
	if s.state.Add(^uint64(0)) == closing {
		s.closeMu.Lock()
		s.closeCond.Broadcast()
		s.closeMu.Unlock()
	}
}

func (s *Schema) read(dst []byte, off uint64) error {
	if s.mapped != nil {
		data := s.mapped.Bytes()
		if off > uint64(len(data)) || uint64(len(dst)) > uint64(len(data))-off {
			return ErrCorrupt
		}
		copy(dst, data[off:off+uint64(len(dst))])
		return nil
	}
	n, err := s.f.ReadAt(dst, int64(off))
	if err != nil {
		return err
	}
	if n != len(dst) {
		return io.ErrUnexpectedEOF
	}
	return nil
}

func (s *Schema) write(src []byte, off uint64) error {
	if s.mapped != nil {
		data := s.mapped.Bytes()
		if off > uint64(len(data)) || uint64(len(src)) > uint64(len(data))-off {
			return ErrCorrupt
		}
		copy(data[off:off+uint64(len(src))], src)
		return nil
	}
	n, err := s.f.WriteAt(src, int64(off))
	if err != nil {
		return err
	}
	if n != len(src) {
		return io.ErrShortWrite
	}
	return nil
}

func (s *Schema) sync() error {
	if s.mapped != nil {
		return s.mapped.Sync()
	}
	return s.f.Sync()
}

func (s *Schema) Close() error {
	s.closeOnce.Do(func() {
		s.closeMu.Lock()
		for {
			old := s.state.Load()
			if s.state.CompareAndSwap(old, old|closing) {
				break
			}
		}
		for s.state.Load() != closing {
			s.closeCond.Wait()
		}
		s.closeMu.Unlock()
		if !s.poisoned.Load() {
			for _, a := range s.entries {
				a.mu.Lock()
				err := s.persist(a, allocationRecord{a.record.generation + 1, a.limit.Load(), a.issued.Load(), true})
				a.mu.Unlock()
				if err != nil {
					s.closeErr = err
					break
				}
			}
		} else {
			s.closeErr = ErrCorrupt
		}
		if s.mapped != nil {
			s.closeErr = errors.Join(s.closeErr, s.mapped.Close())
		}
		if s.f != nil {
			s.closeErr = errors.Join(s.closeErr, s.f.Close())
		}
		s.closeErr = errors.Join(s.closeErr, storage.ReleaseLock(s.lock))
	})
	return s.closeErr
}
