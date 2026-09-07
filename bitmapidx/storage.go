package bitmapidx

import (
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"

	"github.com/evanoberholster/imageIdx/internal/indexutil"
	"github.com/evanoberholster/imageIdx/internal/persist"
)

const (
	bitmapManifestMagic   uint32 = 0x424d4d46 // BMMF
	bitmapManifestVersion uint16 = 1
	bitmapManifestSize           = 48
	bitmapWALFilename            = "bitmap.wal"
)

// Store owns a persistent collection of bitmap indexes backed by a directory.
//
// Durability model: Apply appends a WAL record and publishes to the in-memory
// index; Checkpoint serializes the current effective state into a new
// immutable segment file and atomically publishes a manifest pointing at it;
// OpenPersistent loads that segment and replays the WAL tail on top. Vacuum
// checkpoints and removes obsolete segment generations.
type Store struct {
	directory  *Directory
	dir        string
	config     Config
	durability Durability
	wal        *bitmapWAL
	segment    *segment
	readers    *indexutil.ReaderTracker
	generation uint64
	writeMu    sync.Mutex
	closed     atomic.Bool
}

type bitmapManifest struct {
	generation uint64
	walOffset  int64
	count      uint64
}

// NewPersistent creates and checkpoints a persistent bitmap store.
func NewPersistent(dir string, specs []Spec, config Config, durability Durability) (*Store, error) {
	config, err := config.withDefaults()
	if err != nil || !durability.Valid() {
		if err != nil {
			return nil, err
		}
		return nil, ErrInvalidConfig
	}
	if err = os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	readers := &indexutil.ReaderTracker{}
	indexes := make([]*Index, 0, len(specs))
	for _, spec := range specs {
		index, indexErr := newIndex(spec, config, readers)
		if indexErr != nil {
			return nil, indexErr
		}
		indexes = append(indexes, index)
	}
	directory, err := NewDirectory(indexes...)
	if err != nil {
		return nil, err
	}
	wal, err := openBitmapWAL(filepath.Join(dir, bitmapWALFilename))
	if err != nil {
		return nil, err
	}
	store := &Store{
		directory: directory, dir: dir, config: config, durability: durability,
		wal: wal, readers: readers,
	}
	if err = store.Checkpoint(); err != nil {
		_ = store.Close()
		return nil, err
	}
	return store, nil
}

// OpenPersistent opens a checkpoint and replays its WAL tail.
func OpenPersistent(dir string, config Config, durability Durability) (*Store, error) {
	config, err := config.withDefaults()
	if err != nil || !durability.Valid() {
		if err != nil {
			return nil, err
		}
		return nil, ErrInvalidConfig
	}
	manifest, err := readBitmapManifest(filepath.Join(dir, "manifest"))
	if err != nil {
		return nil, err
	}
	segment, err := openSegment(segmentPath(dir, manifest.generation), config)
	if err != nil {
		return nil, err
	}
	if uint64(segment.directory.Len()) != manifest.count || segment.generation != manifest.generation {
		_ = segment.close()
		return nil, ErrCorruptManifest
	}
	recovered, err := replayBitmapWAL(filepath.Join(dir, bitmapWALFilename), manifest.walOffset)
	if err != nil {
		_ = segment.close()
		return nil, err
	}
	for _, record := range recovered {
		index, ok := segment.directory.Lookup(record.key)
		if !ok || index.Metadata().IndexID != record.indexID {
			_ = segment.close()
			return nil, ErrCorruptWAL
		}
		if err = index.Apply(record.mutation); err != nil {
			_ = segment.close()
			return nil, err
		}
	}
	wal, err := openBitmapWAL(filepath.Join(dir, bitmapWALFilename))
	if err != nil {
		_ = segment.close()
		return nil, err
	}
	return &Store{
		directory: segment.directory, dir: dir, config: config, durability: durability,
		wal: wal, segment: segment, readers: segment.readers, generation: manifest.generation,
	}, nil
}

// Lookup returns an index by logical predicate key.
func (s *Store) Lookup(key Key) (*Index, bool) { return s.directory.Lookup(key) }

// LookupID returns an index by its stable numeric identifier.
func (s *Store) LookupID(indexID uint64) (*Index, bool) { return s.directory.LookupID(indexID) }

// Indexes returns indexes in physical storage order.
func (s *Store) Indexes() []*Index { return s.directory.Indexes() }

// Len returns the number of indexes in the store.
func (s *Store) Len() int { return s.directory.Len() }

// Apply appends a mutation to the WAL and publishes it to the selected index.
func (s *Store) Apply(key Key, mutation Mutation) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if s.closed.Load() {
		return ErrClosed
	}
	index, ok := s.directory.Lookup(key)
	if !ok {
		return ErrInvalidIndex
	}
	if err := s.wal.append(key, index.Metadata().IndexID, mutation, s.durability); err != nil {
		return err
	}
	return index.Apply(mutation)
}

func (s *Store) Add(key Key, ids []ImageID) error {
	return s.Apply(key, Mutation{Add: ids})
}

func (s *Store) Remove(key Key, ids []ImageID) error {
	return s.Apply(key, Mutation{Remove: ids})
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
	offset, err := s.wal.Offset()
	if err != nil {
		return err
	}
	generation := s.generation + 1
	finalPath := segmentPath(s.dir, generation)
	temporary := finalPath + ".tmp"
	if err = writeSegment(temporary, generation, s.directory.Indexes()); err != nil {
		return err
	}
	if err = os.Rename(temporary, finalPath); err != nil {
		return err
	}
	manifest := bitmapManifest{generation: generation, walOffset: offset, count: uint64(s.directory.Len())}
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
	s.readers.WaitForPriorReaders()
	return persist.RemoveObsolete(s.dir, "segment-*.idx", segmentPath(s.dir, s.generation))
}

func (s *Store) Close() error {
	s.writeMu.Lock()
	if s.closed.Swap(true) {
		s.writeMu.Unlock()
		return nil
	}
	var result error
	if s.wal != nil {
		result = s.wal.Close()
	}
	s.writeMu.Unlock()
	if s.segment != nil {
		if err := s.segment.close(); result == nil {
			result = err
		}
	} else {
		for _, index := range s.directory.Indexes() {
			_ = index.Close()
		}
	}
	return result
}

func segmentPath(dir string, generation uint64) string {
	return filepath.Join(dir, fmt.Sprintf("segment-%020d.idx", generation))
}

func writeBitmapManifest(dir string, manifest bitmapManifest) error {
	data := persist.BuildManifest(bitmapManifestSize, bitmapManifestMagic, bitmapManifestVersion, 44, 44, func(data []byte) {
		binary.LittleEndian.PutUint64(data[8:16], manifest.generation)
		binary.LittleEndian.PutUint64(data[16:24], uint64(manifest.walOffset))
		binary.LittleEndian.PutUint64(data[24:32], manifest.count)
	})
	return persist.WriteManifest(dir, data)
}

func readBitmapManifest(path string) (bitmapManifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return bitmapManifest{}, err
	}
	if err := persist.VerifyManifest(data, bitmapManifestSize, bitmapManifestMagic, bitmapManifestVersion, 44, 44); err != nil {
		return bitmapManifest{}, ErrCorruptManifest
	}
	return bitmapManifest{
		generation: binary.LittleEndian.Uint64(data[8:16]),
		walOffset:  int64(binary.LittleEndian.Uint64(data[16:24])),
		count:      binary.LittleEndian.Uint64(data[24:32]),
	}, nil
}
