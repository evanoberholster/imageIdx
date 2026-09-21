package hashidx

import (
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"

	"github.com/evanoberholster/imageIdx/internal/indexutil"
	"github.com/evanoberholster/imageIdx/internal/persist"
)

const (
	perceptualDirName = "perceptual"
	exactDirName      = "exact"
	storeLockName     = ".hashidx.lock"

	storeManifestMagic   uint32 = 0x48534D46 // HSMF
	storeManifestVersion uint16 = 1
	storeManifestSize           = 32
)

// Joint manifest layout (little-endian): magic 0:4, version 4:6,
// reserved 6:8, perceptualGen 8:16, exactGen 16:24, CRC32 28:32 over
// [0:28). It anchors the last jointly checkpointed generation pair for
// operators and backup tools; the per-index manifests stay authoritative,
// so a missing file (pre-V1 stores) is normal and a corrupt one never
// blocks OpenStore.

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
	dir        string
	perceptual *Index
	exact      *ExactIndex
	lock       *os.File

	writeMu sync.Mutex
	closed  atomic.Bool
}

// writeStoreManifest atomically publishes the joint generation anchor at
// dir/manifest. It is advisory: backups and operators read it, opens
// ignore it.
func writeStoreManifest(dir string, perceptualGen, exactGen uint64) error {
	data := persist.BuildManifest(storeManifestSize, storeManifestMagic, storeManifestVersion, 28, 28, func(data []byte) {
		binary.LittleEndian.PutUint64(data[8:16], perceptualGen)
		binary.LittleEndian.PutUint64(data[16:24], exactGen)
	})
	return persist.WriteManifest(dir, data)
}

// ReadStoreManifest returns the last jointly checkpointed generation pair
// recorded at dir/manifest. It is the operator-facing read of the joint
// anchor: backup tools compare it against both sub-manifests instead of
// racing two independent files. A missing file (pre-V1 stores) surfaces
// the raw os error; recognized magic with an unknown version reports
// ErrUnsupported, and corrupt content reports ErrCorrupt.
func ReadStoreManifest(dir string) (perceptualGen, exactGen uint64, err error) {
	data, err := os.ReadFile(filepath.Join(dir, "manifest"))
	if err != nil {
		return 0, 0, err
	}
	if len(data) >= 6 && binary.LittleEndian.Uint32(data[0:4]) == storeManifestMagic &&
		binary.LittleEndian.Uint16(data[4:6]) != storeManifestVersion {
		return 0, 0, ErrUnsupported
	}
	if err := persist.VerifyManifest(data, storeManifestSize, storeManifestMagic, storeManifestVersion, 28, 28); err != nil {
		return 0, 0, ErrCorrupt
	}
	return binary.LittleEndian.Uint64(data[8:16]), binary.LittleEndian.Uint64(data[16:24]), nil
}

// publishJointManifest records the current generation pair after both
// indexes checkpointed. A write failure leaves the previous anchor in
// place; the next successful Checkpoint repairs it.
func (s *Store) publishJointManifest() error {
	return writeStoreManifest(s.dir, s.perceptual.Metadata().Generation, s.exact.Metadata().Generation)
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
	s := &Store{dir: dir, perceptual: perceptual, exact: exact, lock: lock}
	if err := s.publishJointManifest(); err != nil {
		_ = s.Close()
		_ = os.RemoveAll(perceptualDir)
		_ = os.RemoveAll(exactDir)
		_ = os.Remove(filepath.Join(dir, "manifest"))
		return nil, err
	}
	return s, nil
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
	// The joint anchor is advisory only: a missing file means a pre-V1
	// store and a corrupt one never blocks an otherwise healthy open.
	// Sub-manifests stay authoritative either way.
	return &Store{dir: dir, perceptual: perceptual, exact: exact, lock: lock}, nil
}

// Perceptual returns the store's perceptual-hash index. Reads and writes
// against it are lock-free for readers and serialized for the single writer.
func (s *Store) Perceptual() *Index { return s.perceptual }

// Exact returns the store's exact-content index.
func (s *Store) Exact() *ExactIndex { return s.exact }

// Generations returns the current published generation of each index:
// joint health without opening sub-manifests. Equal generations after a
// successful Checkpoint mean the joint anchor names this exact pair;
// divergence means a partial Checkpoint that the next one converges.
func (s *Store) Generations() (perceptual, exact uint64) {
	return s.perceptual.Metadata().Generation, s.exact.Metadata().Generation
}

// Checkpoint persists both indexes as new generations and publishes
// their manifests in order: perceptual first, then exact, then the joint
// anchor. Publication is atomic per index but not across indexes: an
// exact failure after a perceptual success leaves the generations
// diverged, which the next Checkpoint repairs while the stale anchor
// still names the last joint pair. Callers needing a joint snapshot
// should record both Metadata generations around the call.
func (s *Store) Checkpoint() error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if s.closed.Load() {
		return ErrClosed
	}
	if err := s.perceptual.Checkpoint(); err != nil {
		return err
	}
	if err := s.exact.Checkpoint(); err != nil {
		return err
	}
	return s.publishJointManifest()
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
	if err := s.exact.Vacuum(); err != nil {
		return err
	}
	return s.publishJointManifest()
}

// VerifyStore validates a store directory offline: the joint anchor when
// present (a missing file means a pre-V1 store and is not an error), both
// sub-manifests, generation headers, page/block CRCs and ordering, and the
// referenced WAL files' presence. Mappings are read-only and released
// before return; WALs are never replayed and no handles outlive the call.
// It reports the first defect encountered.
func VerifyStore(dir string) error {
	for _, name := range []string{perceptualDirName, exactDirName} {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil || !info.IsDir() {
			return ErrNotStore
		}
	}
	if _, _, err := ReadStoreManifest(dir); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := verifyPerceptualDir(filepath.Join(dir, perceptualDirName)); err != nil {
		return err
	}
	return verifyExactDir(filepath.Join(dir, exactDirName))
}

// verifyPerceptualDir mirrors openIndex validation without replaying the
// WAL or installing handles.
func verifyPerceptualDir(dir string) error {
	manifest, err := readManifest(filepath.Join(dir, "manifest"))
	if err != nil {
		return err
	}
	mapping, err := persist.MapFile(generationPath(dir, manifest.generation), ErrCorrupt)
	if err != nil {
		return err
	}
	defer func() { _ = persist.Unmap(mapping) }()
	view, header, err := decodeGeneration(mapping, Config{RecordsPerPage: manifest.recordsPerPage})
	if err != nil {
		return err
	}
	if header.generation != manifest.generation || view.count != manifest.recordCount ||
		header.pageCount != manifest.pageCount {
		return ErrCorrupt
	}
	if _, err := os.Stat(walPath(dir, manifest.walEpoch)); err != nil {
		return err
	}
	return nil
}

// verifyExactDir mirrors openExact validation without replaying the WAL
// or installing handles.
func verifyExactDir(dir string) error {
	manifest, err := readDiskManifest(filepath.Join(dir, "manifest"))
	if err != nil {
		return err
	}
	mapping, err := persist.MapFile(exactGenerationPath(dir, manifest.generation), ErrCorrupt)
	if err != nil {
		return err
	}
	defer func() { _ = persist.Unmap(mapping) }()
	view, header, err := decodeExactGeneration(mapping)
	if err != nil {
		return err
	}
	if header.generation != manifest.generation || header.indexID != manifest.indexID ||
		header.liveCount != manifest.liveCount || header.blockCount != manifest.blockCount ||
		header.algorithm != manifest.algorithm {
		return ErrCorrupt
	}
	if view.liveCount != manifest.liveCount || uint64(len(view.blocks)) != manifest.blockCount {
		return ErrCorrupt
	}
	if _, err := os.Stat(exactWALPath(dir, uint64(manifest.walEpoch))); err != nil {
		return err
	}
	return nil
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

// Directory locking lives in lock_unix.go (flock), lock_windows.go
// (LockFileEx), and lock_other.go (explicit unsupported error), selected
// by build tags. See those files for the per-platform contracts.
