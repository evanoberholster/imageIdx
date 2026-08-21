package bitmapidx

import (
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"

	"github.com/evanoberholster/imageIdx/internal/indexutil"
)

const (
	bitmapManifestMagic   uint32 = 0x424d4d46 // BMMF
	bitmapManifestVersion uint16 = 1
	bitmapManifestSize           = 48
	bitmapWALFilename            = "bitmap.wal"
)

type Store struct {
	Directory *BitmapDirectory

	dir        string
	config     Config
	durability Durability
	wal        *bitmapWAL
	segment    *Segment
	generation uint64
	writeMu    sync.Mutex
	closed     atomic.Bool
}

type bitmapManifest struct {
	generation uint64
	walOffset  int64
	count      uint64
}

func NewPersistent(dir string, specs []IndexSpec, config Config, durability Durability) (*Store, error) {
	config, err := config.withDefaults()
	if err != nil || durability > DurabilitySync {
		if err != nil {
			return nil, err
		}
		return nil, ErrInvalidConfig
	}
	if err = os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	indexes := make([]*BitmapIndex, 0, len(specs))
	for _, spec := range specs {
		index, indexErr := NewBitmapIndex(spec.Key, spec.IndexID, spec.ImageIDs, config)
		if indexErr != nil {
			return nil, indexErr
		}
		view := index.view.Load()
		index.view.Store(&BitmapIndexView{
			Version: view.Version, IndexType: view.IndexType, Flags: spec.Flags,
			IndexID: view.IndexID, ValueID: view.ValueID, Cardinality: view.Cardinality,
			Generation: view.Generation, Base: view.Base,
		})
		indexes = append(indexes, index)
	}
	directory, err := NewBitmapDirectory(indexes...)
	if err != nil {
		return nil, err
	}
	wal, err := openBitmapWAL(filepath.Join(dir, bitmapWALFilename))
	if err != nil {
		return nil, err
	}
	store := &Store{Directory: directory, dir: dir, config: config, durability: durability, wal: wal}
	if err = store.Checkpoint(); err != nil {
		_ = store.Close()
		return nil, err
	}
	return store, nil
}

func OpenPersistent(dir string, config Config, durability Durability) (*Store, error) {
	config, err := config.withDefaults()
	if err != nil || durability > DurabilitySync {
		if err != nil {
			return nil, err
		}
		return nil, ErrInvalidConfig
	}
	manifest, err := readBitmapManifest(filepath.Join(dir, "manifest"))
	if err != nil {
		return nil, err
	}
	segment, err := OpenSegment(segmentPath(dir, manifest.generation), config)
	if err != nil {
		return nil, err
	}
	if uint64(segment.Directory.Len()) != manifest.count || segment.Generation != manifest.generation {
		_ = segment.Close()
		return nil, ErrCorruptManifest
	}
	recovered, err := replayBitmapWAL(filepath.Join(dir, bitmapWALFilename), manifest.walOffset)
	if err != nil {
		_ = segment.Close()
		return nil, err
	}
	for _, record := range recovered {
		index, ok := segment.Directory.Lookup(record.key)
		if !ok || index.IndexID() != record.indexID {
			_ = segment.Close()
			return nil, ErrCorruptWAL
		}
		if err = index.Write(record.mutation); err != nil {
			_ = segment.Close()
			return nil, err
		}
	}
	wal, err := openBitmapWAL(filepath.Join(dir, bitmapWALFilename))
	if err != nil {
		_ = segment.Close()
		return nil, err
	}
	return &Store{
		Directory: segment.Directory, dir: dir, config: config, durability: durability,
		wal: wal, segment: segment, generation: manifest.generation,
	}, nil
}

func (s *Store) Write(key BitmapKey, mutation BitmapMutation) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if s.closed.Load() {
		return ErrClosed
	}
	index, ok := s.Directory.Lookup(key)
	if !ok {
		return ErrInvalidIndex
	}
	if err := s.wal.append(key, index.IndexID(), mutation, s.durability); err != nil {
		return err
	}
	return index.Write(mutation)
}

func (s *Store) Add(key BitmapKey, ids []uint64) error {
	return s.Write(key, BitmapMutation{Additions: ids})
}

func (s *Store) Remove(key BitmapKey, ids []uint64) error {
	return s.Write(key, BitmapMutation{Removals: ids})
}

func (s *Store) Checkpoint() error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if s.closed.Load() {
		return ErrClosed
	}
	if s.wal == nil || s.dir == "" {
		return ErrNotPersistent
	}
	offset, err := s.wal.offset()
	if err != nil {
		return err
	}
	generation := s.generation + 1
	finalPath := segmentPath(s.dir, generation)
	temporary := finalPath + ".tmp"
	if err = WriteSegment(temporary, generation, s.Directory.Indexes()); err != nil {
		return err
	}
	if err = os.Rename(temporary, finalPath); err != nil {
		return err
	}
	manifest := bitmapManifest{generation: generation, walOffset: offset, count: uint64(s.Directory.Len())}
	if err = writeBitmapManifest(s.dir, manifest); err != nil {
		return err
	}
	s.generation = generation
	return nil
}

// Vacuum checkpoints live bases into physical key order and removes obsolete
// immutable segment files. Existing mmaps remain valid until Store.Close.
func (s *Store) Vacuum() error {
	if err := s.Checkpoint(); err != nil {
		return err
	}
	s.config.Epoch.Synchronize()
	paths, err := filepath.Glob(filepath.Join(s.dir, "segment-*.idx"))
	if err != nil {
		return err
	}
	current := segmentPath(s.dir, s.generation)
	for _, path := range paths {
		if path != current {
			if err = os.Remove(path); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
	}
	return nil
}

func (s *Store) Close() error {
	s.writeMu.Lock()
	if s.closed.Swap(true) {
		s.writeMu.Unlock()
		return nil
	}
	var result error
	if s.wal != nil {
		result = s.wal.close()
	}
	s.writeMu.Unlock()
	if s.segment != nil {
		if err := s.segment.Close(); result == nil {
			result = err
		}
	} else {
		for _, index := range s.Directory.Indexes() {
			_ = index.Close()
		}
	}
	return result
}

func segmentPath(dir string, generation uint64) string {
	return filepath.Join(dir, fmt.Sprintf("segment-%020d.idx", generation))
}

func writeBitmapManifest(dir string, manifest bitmapManifest) error {
	data := make([]byte, bitmapManifestSize)
	binary.LittleEndian.PutUint32(data[0:4], bitmapManifestMagic)
	binary.LittleEndian.PutUint16(data[4:6], bitmapManifestVersion)
	binary.LittleEndian.PutUint64(data[8:16], manifest.generation)
	binary.LittleEndian.PutUint64(data[16:24], uint64(manifest.walOffset))
	binary.LittleEndian.PutUint64(data[24:32], manifest.count)
	binary.LittleEndian.PutUint32(data[44:48], crc32.ChecksumIEEE(data[:44]))
	return indexutil.WriteFileAtomic(dir, "manifest.tmp", "manifest", data, 0o644)
}

func readBitmapManifest(path string) (bitmapManifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return bitmapManifest{}, err
	}
	if len(data) != bitmapManifestSize || binary.LittleEndian.Uint32(data[0:4]) != bitmapManifestMagic ||
		binary.LittleEndian.Uint16(data[4:6]) != bitmapManifestVersion ||
		binary.LittleEndian.Uint32(data[44:48]) != crc32.ChecksumIEEE(data[:44]) {
		return bitmapManifest{}, ErrCorruptManifest
	}
	return bitmapManifest{
		generation: binary.LittleEndian.Uint64(data[8:16]),
		walOffset:  int64(binary.LittleEndian.Uint64(data[16:24])),
		count:      binary.LittleEndian.Uint64(data[24:32]),
	}, nil
}
