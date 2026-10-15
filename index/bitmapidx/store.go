package bitmapidx

import (
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"

	schema "github.com/evanoberholster/imageIdx/schema"
	"github.com/evanoberholster/imageIdx/internal/storage"
)

const namespaceManifestMagic uint32 = 0x424d464d

// Store persists a fixed registry of namespaces with one serialized WAL
// writer. Child Add/Apply/Move/ReplaceText calls automatically append to
// that WAL. Each batch is atomic within one namespace; cross-namespace
// transactions are not provided. Reads remain lock-free across writes and
// checkpoints. Close owns all child and mmap lifetimes.
//
// There is no automatic checkpoint trigger: the WAL grows with every write
// until the caller invokes Checkpoint or Vacuum, and the prior snapshot
// generation is retained until Vacuum retires it. Deployments must schedule
// maintenance, bound WAL growth, and monitor free space themselves; the
// package benchmark suite probes the checkpoint write stall and restart
// budgets.
type Store struct {
	dir        string
	namespaces map[schema.Namespace]*Index // immutable after construction
	order      []*Index
	config     NamespaceConfig
	durability schema.Durability
	storeID    [16]byte // binds manifest, snapshots, and WAL to one store
	writeMu    sync.Mutex
	closed     bool
	failed     error
	lock       *os.File
	wal        *storage.WAL
	generation uint64
	mapping    []byte
}

// newStoreID returns a fresh random identity. Persisting it in the manifest
// and every snapshot and WAL frame lets open detect files mixed between
// different stores after a backup or restore.
func newStoreID() ([16]byte, error) {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return [16]byte{}, err
	}
	return id, nil
}

// NewStore creates a new mmap-backed namespace store. The
// directory must be empty (besides the lock file). NamespaceSpec definitions are
// validated, persisted in the first checkpoint, and immutable after;
// config is runtime-only policy. Handles become store-owned on success
// and must be closed via the store.
func NewStore(dir string, namespaces []NamespaceSpec, config NamespaceConfig, durability schema.Durability) (*Store, error) {
	config, err := config.withDefaults()
	if err != nil {
		return nil, err
	}
	if !durability.Valid() {
		return nil, ErrInvalidConfig
	}
	specs := slices.Clone(namespaces)
	slices.SortFunc(specs, func(a, b NamespaceSpec) int { return int(a.Namespace) - int(b.Namespace) })
	if err := validateNamespaceSpecs(specs); err != nil {
		return nil, err
	}
	storeID, err := newStoreID()
	if err != nil {
		return nil, err
	}
	lock, err := storage.AcquireLock(dir, storeLockName, true)
	if err != nil {
		return nil, err
	}
	s := &Store{dir: dir, config: config, durability: durability, lock: lock, storeID: storeID, namespaces: make(map[schema.Namespace]*Index)}
	for _, spec := range specs {
		index, err := NewMemory(spec, config)
		if err != nil {
			s.Close()
			return nil, err
		}
		s.namespaces[spec.Namespace] = index
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

// OpenStore opens a checkpoint, replays its WAL, and repairs
// only an incomplete terminal frame before making namespace handles
// available. Generation, namespace count, segment checksums, and WAL offsets
// all validate; any other corruption fails the open for external recovery.
func OpenStore(dir string, config NamespaceConfig, durability schema.Durability) (*Store, error) {
	config, err := config.withDefaults()
	if err != nil {
		return nil, err
	}
	if !durability.Valid() {
		return nil, ErrInvalidConfig
	}
	lock, err := storage.AcquireLock(dir, storeLockName, false)
	if err != nil {
		return nil, err
	}
	s := &Store{dir: dir, config: config, durability: durability, lock: lock}
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
	if err := storage.VerifyManifest(data, 48, namespaceManifestMagic, 2, 44, 44); err != nil {
		return nil, ErrCorruptManifest
	}
	copy(s.storeID[:], data[24:40])
	if s.storeID == ([16]byte{}) {
		return nil, ErrCorruptManifest
	}
	s.generation = binary.LittleEndian.Uint64(data[8:16])
	if s.generation == 0 {
		return nil, ErrCorruptManifest
	}
	s.order, s.mapping, err = openNamespaceSegment(s.snapshotPath(s.generation), s.generation, config, s.storeID)
	if err != nil {
		return nil, err
	}
	if uint64(len(s.order)) != binary.LittleEndian.Uint64(data[16:24]) {
		return nil, ErrCorruptManifest
	}
	s.namespaces = make(map[schema.Namespace]*Index, len(s.order))
	for _, index := range s.order {
		s.namespaces[index.spec().Namespace] = index
	}
	walPath := s.walPath(s.generation)
	if _, err := os.Stat(walPath); err != nil {
		return nil, err
	}
	end, err := s.replay(walPath)
	if err != nil {
		return nil, err
	}
	s.wal, err = storage.OpenWAL(walPath, 0)
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

// Lookup returns the store-owned namespace for t. All mutations append to the
// store WAL. The handle must not be closed directly.
func (s *Store) Lookup(t schema.Namespace) (*Index, bool) {
	index, ok := s.namespaces[t]
	return index, ok
}

// Len returns the number of namespaces in physical storage order.
func (s *Store) Len() int { return len(s.order) }

// Namespaces returns store-owned handles in physical storage order. The slice
// is a copy; the handles are shared and must not be closed directly.
func (s *Store) Namespaces() []*Index { return append([]*Index(nil), s.order...) }

func (s *Store) writable() error {
	if s.closed {
		return ErrClosed
	}
	return s.failed
}

func (s *Store) snapshotPath(gen uint64) string {
	return filepath.Join(s.dir, fmt.Sprintf("namespaces-%020d.idx", gen))
}
func (s *Store) walPath(gen uint64) string {
	return filepath.Join(s.dir, fmt.Sprintf("namespaces-%020d.wal", gen))
}

// Checkpoint blocks writers, not readers. It syncs the WAL, writes and
// revalidates a new mmap snapshot, syncs an empty successor WAL and the
// directory, then publishes the manifest. Afterwards it swaps namespace
// roots, drains prior readers, and unmaps the old snapshot. A completed
// checkpoint bounds subsequent replay to its WAL.
func (s *Store) Checkpoint() error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.checkpointLocked(false)
}

// Vacuum checkpoints with dead dictionary entries dropped, then removes
// obsolete snapshot and WAL generations. Retirement serializes with
// manifest publication; only the live generation's files remain.
func (s *Store) Vacuum() error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if err := s.checkpointLocked(true); err != nil {
		return err
	}
	if err := storage.RemoveObsolete(s.dir, "namespaces-*.idx", s.snapshotPath(s.generation)); err != nil {
		return err
	}
	return storage.RemoveObsolete(s.dir, "namespaces-*.wal", s.walPath(s.generation))
}

// checkpointLocked writes, validates, and publishes one generation. Crash
// safety rests on the old manifest staying authoritative until the new
// one publishes: segment and successor WAL writes precede it, directory
// names sync before it, and an unpublished successor from a prior crash
// is safe to truncate while the lock is held. Dictionary generations are
// preserved on plain checkpoints so compiled queries keep their fast
// path; vacuum advances them because dead values may leave the set.
func (s *Store) checkpointLocked(vacuum bool) error {
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
	if err := writeNamespaceSegment(target+".tmp", gen, s.order, vacuum, s.storeID); err != nil {
		return err
	}
	if err := os.Rename(target+".tmp", target); err != nil {
		return err
	}
	loaded, mapping, err := openNamespaceSegment(target, gen, s.config, s.storeID)
	if err != nil {
		return err
	}
	keepMapping := false
	defer func() {
		if !keepMapping {
			storage.Unmap(mapping)
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
	nextWAL, err := storage.OpenWAL(s.walPath(gen), 0)
	if err != nil {
		return err
	}
	manifest := storage.BuildManifest(48, namespaceManifestMagic, 2, 44, 44, func(data []byte) {
		binary.LittleEndian.PutUint64(data[8:16], gen)
		binary.LittleEndian.PutUint64(data[16:24], uint64(len(s.order)))
		copy(data[24:40], s.storeID[:])
	})
	// Sync names before publishing their manifest reference.
	if err = storage.SyncDir(s.dir); err != nil {
		nextWAL.Close()
		return err
	}
	if err = storage.WriteManifest(s.dir, manifest); err != nil {
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
	return errors.Join(retireErr, storage.Unmap(oldMapping),
		storage.RemoveObsolete(s.dir, "namespaces-*.wal", s.walPath(s.generation)))
}

// Close marks the store closed, closes children without per-index
// locks, then closes the WAL, unmaps the snapshot, and releases the
// directory lock. It is idempotent. Commit loops stop before the write
// lock is taken: their final groups need it, and queued batches commit
// before Close returns.
func (s *Store) Close() error {
	for _, index := range s.order {
		index.commits.Stop()
	}
	for _, index := range s.order {
		index.commits.Wait()
	}
	for _, index := range s.order {
		index.workers.Wait()
	}
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
	return errors.Join(err, storage.Unmap(s.mapping), storage.ReleaseLock(s.lock))
}
