package bitmapidx

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/evanoberholster/imageIdx/internal/persist"
)

const fieldManifestMagic uint32 = 0x424d464d

// FieldStore persists a fixed registry of fields with one serialized WAL
// writer. Child Add/Apply/Move/ReplaceText calls automatically append to
// that WAL. Each batch is atomic within one field; cross-field
// transactions are not provided. Reads remain lock-free across writes and
// checkpoints. Close owns all child and mmap lifetimes.
type FieldStore struct {
	dir        string
	fields     map[IndexType]*FieldIndex // immutable after construction
	order      []*FieldIndex
	config     FieldConfig
	durability Durability
	writeMu    sync.Mutex
	closed     bool
	failed     error
	lock       *os.File
	wal        *persist.WAL
	generation uint64
	mapping    []byte
}

// NewPersistentFields creates a new mmap-backed field store. The
// directory must be empty (besides the lock file). Field definitions are
// validated, persisted in the first checkpoint, and immutable after;
// config is runtime-only policy. Handles become store-owned on success
// and must be closed via the store.
func NewPersistentFields(dir string, fields []Field, config FieldConfig, durability Durability) (*FieldStore, error) {
	config, err := config.withDefaults()
	if err != nil {
		return nil, err
	}
	if !durability.Valid() {
		return nil, ErrInvalidConfig
	}
	registry := NewRegistry()
	if err := registry.Register(fields...); err != nil {
		return nil, err
	}
	lock, err := lockStore(dir, true)
	if err != nil {
		return nil, err
	}
	s := &FieldStore{dir: dir, config: config, durability: durability, lock: lock, fields: make(map[IndexType]*FieldIndex)}
	for _, field := range registry.Fields() {
		index, err := NewFieldIndex(field, config)
		if err != nil {
			s.Close()
			return nil, err
		}
		s.fields[field.Type] = index
		s.order = append(s.order, index)
	}
	if err := s.Checkpoint(); err != nil {
		s.Close()
		return nil, err
	}
	for _, index := range s.order {
		index.owner = s
	}
	return s, nil
}

// OpenPersistentFields opens a checkpoint, replays its WAL, and repairs
// only an incomplete terminal frame before making field handles
// available. Generation, field count, segment checksums, and WAL offsets
// all validate; any other corruption fails the open for external recovery.
func OpenPersistentFields(dir string, config FieldConfig, durability Durability) (*FieldStore, error) {
	config, err := config.withDefaults()
	if err != nil {
		return nil, err
	}
	if !durability.Valid() {
		return nil, ErrInvalidConfig
	}
	lock, err := lockStore(dir, false)
	if err != nil {
		return nil, err
	}
	s := &FieldStore{dir: dir, config: config, durability: durability, lock: lock}
	ok := false
	defer func() {
		if !ok {
			s.Close()
		}
	}()
	data, err := os.ReadFile(filepath.Join(dir, "manifest"))
	if err != nil {
		return nil, err
	}
	if err := persist.VerifyManifest(data, 48, fieldManifestMagic, 1, 44, 44); err != nil {
		return nil, ErrCorruptManifest
	}
	s.generation = binary.LittleEndian.Uint64(data[8:16])
	if s.generation == 0 {
		return nil, ErrCorruptManifest
	}
	s.order, s.mapping, err = openFieldSegment(s.snapshotPath(s.generation), s.generation, config)
	if err != nil {
		return nil, err
	}
	if uint64(len(s.order)) != binary.LittleEndian.Uint64(data[16:24]) {
		return nil, ErrCorruptManifest
	}
	s.fields = make(map[IndexType]*FieldIndex, len(s.order))
	for _, index := range s.order {
		s.fields[index.Field().Type] = index
	}
	walPath := s.walPath(s.generation)
	if _, err := os.Stat(walPath); err != nil {
		return nil, err
	}
	end, err := s.replay(walPath)
	if err != nil {
		return nil, err
	}
	s.wal, err = persist.OpenWAL(walPath, 0)
	if err != nil {
		return nil, err
	}
	if err := s.wal.RepairTail(end); err != nil {
		return nil, err
	}
	for _, index := range s.order {
		index.owner = s
	}
	ok = true
	return s, nil
}

// Lookup returns the store-owned field for t. All mutations append to the
// store WAL. The handle must not be closed directly.
func (s *FieldStore) Lookup(t IndexType) (*FieldIndex, bool) {
	index, ok := s.fields[t]
	return index, ok
}
// Len returns the number of fields in physical storage order.
func (s *FieldStore) Len() int { return len(s.order) }

// Fields returns store-owned handles in physical storage order. The slice
// is a copy; the handles are shared and must not be closed directly.
func (s *FieldStore) Fields() []*FieldIndex { return append([]*FieldIndex(nil), s.order...) }

func (s *FieldStore) writable() error {
	if s.closed {
		return ErrClosed
	}
	return s.failed
}

func (s *FieldStore) snapshotPath(gen uint64) string {
	return filepath.Join(s.dir, fmt.Sprintf("fields-%020d.idx", gen))
}
func (s *FieldStore) walPath(gen uint64) string {
	return filepath.Join(s.dir, fmt.Sprintf("fields-%020d.wal", gen))
}

// Checkpoint blocks writers, not readers. It syncs the WAL, writes and
// revalidates a new mmap snapshot, syncs an empty successor WAL and the
// directory, then publishes the manifest. Afterwards it swaps field
// roots, drains prior readers, and unmaps the old snapshot. A completed
// checkpoint bounds subsequent replay to its WAL.
func (s *FieldStore) Checkpoint() error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.checkpointLocked(false)
}

// Vacuum checkpoints with dead dictionary entries dropped, then removes
// obsolete snapshot and WAL generations. Retirement serializes with
// manifest publication; only the live generation's files remain.
func (s *FieldStore) Vacuum() error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if err := s.checkpointLocked(true); err != nil {
		return err
	}
	if err := persist.RemoveObsolete(s.dir, "fields-*.idx", s.snapshotPath(s.generation)); err != nil {
		return err
	}
	return persist.RemoveObsolete(s.dir, "fields-*.wal", s.walPath(s.generation))
}

// checkpointLocked writes, validates, and publishes one generation. Crash
// safety rests on the old manifest staying authoritative until the new
// one publishes: segment and successor WAL writes precede it, directory
// names sync before it, and an unpublished successor from a prior crash
// is safe to truncate while the lock is held. Dictionary generations are
// preserved on plain checkpoints so compiled queries keep their fast
// path; vacuum advances them because dead values may leave the set.
func (s *FieldStore) checkpointLocked(vacuum bool) error {
	if err := s.writable(); err != nil {
		return err
	}
	if s.generation == ^uint64(0) {
		return ErrInvalidConfig
	}
	if s.wal != nil {
		if err := s.wal.Sync(); err != nil {
			s.failed = err
			return err
		}
	}
	for _, index := range s.order {
		index.writerMu.Lock()
	}
	locked := true
	defer func() {
		if locked {
			for _, index := range s.order {
				index.writerMu.Unlock()
			}
		}
	}()
	gen := s.generation + 1
	target := s.snapshotPath(gen)
	if err := writeFieldSegment(target+".tmp", gen, s.order, vacuum); err != nil {
		return err
	}
	if err := os.Rename(target+".tmp", target); err != nil {
		return err
	}
	loaded, mapping, err := openFieldSegment(target, gen, s.config)
	if err != nil {
		return err
	}
	keepMapping := false
	defer func() {
		if !keepMapping {
			persist.Unmap(mapping)
		}
	}()
	// An unpublished successor left by a crash is safe to replace while we
	// exclusively own the directory and the old manifest remains authoritative.
	wf, err := os.OpenFile(s.walPath(gen), os.O_CREATE|os.O_TRUNC|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	err = wf.Sync()
	closeErr := wf.Close()
	if err = errors.Join(err, closeErr); err != nil {
		return err
	}
	nextWAL, err := persist.OpenWAL(s.walPath(gen), 0)
	if err != nil {
		return err
	}
	manifest := persist.BuildManifest(48, fieldManifestMagic, 1, 44, 44, func(data []byte) {
		binary.LittleEndian.PutUint64(data[8:16], gen)
		binary.LittleEndian.PutUint64(data[16:24], uint64(len(s.order)))
	})
	// Sync names before publishing their manifest reference.
	if err = syncStoreDir(s.dir); err != nil {
		nextWAL.Close()
		return err
	}
	if err = persist.WriteManifest(s.dir, manifest); err != nil {
		nextWAL.Close()
		s.failed = errors.Join(ErrRecoveryRequired, err)
		return s.failed
	}
	oldMapping, oldWAL := s.mapping, s.wal
	for n, index := range s.order {
		old := index.view.Load()
		next := loaded[n].view.Load()
		next.generation = old.generation + 1
		index.dict = loaded[n].dict
		if vacuum {
			index.dict.generation = old.dictGen + 1
		} else {
			index.dict.generation = old.dictGen
		}
		next.dictGen = index.dict.generation
		index.view.Store(next)
	}
	s.mapping, s.wal, s.generation = mapping, nextWAL, gen
	keepMapping = true
	for _, index := range s.order {
		index.writerMu.Unlock()
	}
	locked = false
	for _, index := range s.order {
		index.readers.WaitForPriorReaders()
	}
	var retireErr error
	if oldWAL != nil {
		retireErr = oldWAL.Close()
	}
	return errors.Join(retireErr, persist.Unmap(oldMapping),
		persist.RemoveObsolete(s.dir, "fields-*.wal", s.walPath(s.generation)))
}

// syncStoreDir fsyncs a directory so newly renamed segment names and the
// truncated successor WAL are durable before the manifest publishes them.
func syncStoreDir(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	return errors.Join(f.Sync(), f.Close())
}

// Close marks the store closed, closes children without per-index
// locks, then closes the WAL, unmaps the snapshot, and releases the
// directory lock. It is idempotent.
func (s *FieldStore) Close() error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	for _, index := range s.order {
		index.closeInternal()
	}
	var err error
	if s.wal != nil {
		err = s.wal.Close()
	}
	return errors.Join(err, persist.Unmap(s.mapping), unlockStore(s.lock))
}
