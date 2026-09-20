package hashidx

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"

	"github.com/evanoberholster/imageIdx/internal/indexutil"
)

const (
	perceptualDirName = "perceptual"
	exactDirName      = "exact"
	storeLockName     = ".hashidx.lock"
)

var (
	ErrStoreExists = errors.New("hashidx: store directory is not empty")
	ErrStoreBusy   = errors.New("hashidx: store directory already has a writer")
	ErrNotStore    = errors.New("hashidx: directory is not a hashidx store")
	ErrChannelFull = errors.New("hashidx: buffered writer channel full")
)

// StoreSpec describes the two indexes a new Store contains: one perceptual
// Index and one exact-content ExactIndex.
type StoreSpec struct {
	// Perceptual configures the perceptual index. The zero value selects
	// defaults.
	Perceptual Config
	// Exact describes the exact-content index's stable identity, algorithm,
	// and optional initial entries. Algorithm must be set.
	Exact ExactSpec
	// ExactConfig configures exact block sizing and write batching. The zero
	// value selects defaults.
	ExactConfig ExactConfig
}

// Store is the single controller for a hashidx directory. It owns exactly one
// mmap-backed perceptual Index and one mmap-backed ExactIndex, one shared
// reader tracker, and the directory lock. Reads go straight to the indexes
// without locks; Store serializes Checkpoint, Vacuum, and Close across both.
//
// The perceptual artifacts live under perceptual/ and the exact artifacts
// under exact/ inside dir.
type Store struct {
	perceptual *Index
	exact      *ExactIndex
	lock       *os.File

	writeMu sync.Mutex
	closed  atomic.Bool
}

// NewStore creates and locks a store in dir and checkpoints both indexes.
func NewStore(dir string, spec StoreSpec, durability Durability) (*Store, error) {
	if dir == "" {
		return nil, ErrInvalidConfig
	}
	if !durability.Valid() {
		return nil, ErrInvalidConfig
	}
	lock, err := lockStoreDir(dir, true)
	if err != nil {
		return nil, err
	}
	readers := &indexutil.ReaderTracker{}
	perceptualDir := filepath.Join(dir, perceptualDirName)
	exactDir := filepath.Join(dir, exactDirName)
	perceptual, err := newIndex(perceptualDir, spec.Perceptual, durability, readers)
	if err != nil {
		_ = os.RemoveAll(perceptualDir)
		_ = unlockStoreDir(lock)
		return nil, err
	}
	exact, err := newExact(exactDir, spec.Exact, spec.ExactConfig, durability, readers)
	if err != nil {
		_ = perceptual.Close()
		_ = os.RemoveAll(perceptualDir)
		_ = os.RemoveAll(exactDir)
		_ = unlockStoreDir(lock)
		return nil, err
	}
	return &Store{perceptual: perceptual, exact: exact, lock: lock}, nil
}

// OpenStore opens an existing persistent store and replays both WAL tails.
// The store's persisted sizing and algorithm are adopted.
func OpenStore(dir string, durability Durability) (*Store, error) {
	if !durability.Valid() {
		return nil, ErrInvalidConfig
	}
	lock, err := lockStoreDir(dir, false)
	if err != nil {
		return nil, err
	}
	for _, name := range []string{perceptualDirName, exactDirName} {
		info, statErr := os.Stat(filepath.Join(dir, name))
		if statErr != nil || !info.IsDir() {
			_ = unlockStoreDir(lock)
			return nil, ErrNotStore
		}
	}
	readers := &indexutil.ReaderTracker{}
	perceptual, err := openIndex(filepath.Join(dir, perceptualDirName), Config{}, durability, readers)
	if err != nil {
		_ = unlockStoreDir(lock)
		return nil, err
	}
	exact, err := openExact(filepath.Join(dir, exactDirName), ExactConfig{}, durability, readers)
	if err != nil {
		_ = perceptual.Close()
		_ = unlockStoreDir(lock)
		return nil, err
	}
	return &Store{perceptual: perceptual, exact: exact, lock: lock}, nil
}

// Perceptual returns the store's perceptual-hash index. Reads and writes
// against it are lock-free for readers and serialized for the single writer.
func (s *Store) Perceptual() *Index { return s.perceptual }

// Exact returns the store's exact-content index.
func (s *Store) Exact() *ExactIndex { return s.exact }

// Checkpoint persists both indexes as new generations and publishes
// their manifests in order: perceptual first, then exact. Publication is
// atomic per index but not across indexes: an exact failure after a
// perceptual success leaves the generations diverged by one, which the
// next Checkpoint repairs. Callers needing a joint snapshot should record
// both Metadata generations around the call.
func (s *Store) Checkpoint() error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if s.closed.Load() {
		return ErrClosed
	}
	if err := s.perceptual.Checkpoint(); err != nil {
		return err
	}
	return s.exact.Checkpoint()
}

// Vacuum repacks the perceptual index to records admitted by live (nil keeps
// every record) and the exact index to its currently live entries, then
// checkpoints and reclaims obsolete generations. Both indexes keep their
// logical contents.
func (s *Store) Vacuum(live CandidateSet) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if s.closed.Load() {
		return ErrClosed
	}
	if err := s.perceptual.Vacuum(live); err != nil {
		return err
	}
	return s.exact.Vacuum()
}

// Close closes both indexes, drains readers, and releases the directory lock.
// It is idempotent.
func (s *Store) Close() error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if s.closed.Swap(true) {
		return nil
	}
	var result error
	if s.perceptual != nil {
		result = errors.Join(result, s.perceptual.Close())
	}
	if s.exact != nil {
		result = errors.Join(result, s.exact.Close())
	}
	if s.lock != nil {
		result = errors.Join(result, unlockStoreDir(s.lock))
	}
	return result
}

// lockStoreDir exclusively locks the store directory with flock, so it is
// Unix-only and has no Windows fallback. The lock file is never unlinked:
// every opener must lock the same inode. When create is true the directory
// must otherwise be empty.
func lockStoreDir(dir string, create bool) (*os.File, error) {
	if create {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
	}
	f, err := os.OpenFile(filepath.Join(dir, storeLockName), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, errors.Join(ErrStoreBusy, err)
	}
	if create {
		entries, err := os.ReadDir(dir)
		if err != nil {
			_ = unlockStoreDir(f)
			return nil, err
		}
		for _, entry := range entries {
			if entry.Name() != storeLockName {
				_ = unlockStoreDir(f)
				return nil, ErrStoreExists
			}
		}
	}
	return f, nil
}

func unlockStoreDir(f *os.File) error {
	if f == nil {
		return nil
	}
	return errors.Join(syscall.Flock(int(f.Fd()), syscall.LOCK_UN), f.Close())
}
