package geoidx

import (
	"os"
	"path/filepath"
	"sync"

	"github.com/evanoberholster/imageIdx/internal/persist"
)

type persistentState struct {
	mu                sync.Mutex
	dir               string
	durability        Durability
	wal               *geoWAL
	storageGeneration uint64
}

func NewPersistent(dir string, spec Spec, config Config, durability Durability) (*Index, error) {
	if !durability.Valid() {
		return nil, ErrInvalidConfig
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	index, err := New(spec, config)
	if err != nil {
		return nil, err
	}
	wal, err := openGeoWAL(filepath.Join(dir, geoWALFilename))
	if err != nil {
		_ = index.Close()
		return nil, err
	}
	index.persistence = &persistentState{dir: dir, durability: durability, wal: wal}
	if err = index.Checkpoint(); err != nil {
		_ = index.Close()
		return nil, err
	}
	return index, nil
}

func OpenPersistent(dir string, config Config, durability Durability) (*Index, error) {
	if !durability.Valid() {
		return nil, ErrInvalidConfig
	}
	config, err := config.withDefaults()
	if err != nil {
		return nil, err
	}
	manifest, err := readGeoManifest(filepath.Join(dir, "manifest"))
	if err != nil {
		return nil, err
	}
	index, storageGeneration, err := readGeoGeneration(geoGenerationPath(dir, manifest.storageGeneration), config)
	if err != nil {
		return nil, err
	}
	if storageGeneration != manifest.storageGeneration || index.Metadata().IndexID != manifest.indexID {
		_ = index.Close()
		return nil, ErrCorruptManifest
	}
	batches, err := replayGeoWAL(filepath.Join(dir, geoWALFilename), manifest.walOffset)
	if err != nil {
		_ = index.Close()
		return nil, err
	}
	for _, batch := range batches {
		if err = index.Apply(batch); err != nil {
			_ = index.Close()
			return nil, ErrCorruptWAL
		}
	}
	err = index.Flush()
	if err != nil {
		_ = index.Close()
		return nil, err
	}
	wal, err := openGeoWAL(filepath.Join(dir, geoWALFilename))
	if err != nil {
		_ = index.Close()
		return nil, err
	}
	index.persistence = &persistentState{
		dir: dir, durability: durability, wal: wal, storageGeneration: storageGeneration,
	}
	return index, nil
}

func (g *Index) Checkpoint() error {
	if g.persistence == nil {
		return ErrNotPersistent
	}
	g.persistence.mu.Lock()
	defer g.persistence.mu.Unlock()
	g.writerMu.Lock()
	defer g.writerMu.Unlock()
	if g.closed.Load() {
		return ErrClosed
	}
	if g.persistence.wal == nil || g.persistence.dir == "" {
		return ErrNotPersistent
	}
	if err := g.flushLocked(); err != nil {
		return err
	}
	offset, err := g.persistence.wal.Offset()
	if err != nil {
		return err
	}
	storageGeneration := g.persistence.storageGeneration + 1
	finalPath := geoGenerationPath(g.persistence.dir, storageGeneration)
	temporary := finalPath + ".tmp"
	token := g.readers.Acquire()
	err = writeGeoGeneration(temporary, storageGeneration, g.view.Load())
	g.readers.Release(token)
	if err != nil {
		return err
	}
	if err = os.Rename(temporary, finalPath); err != nil {
		return err
	}
	if err = writeGeoManifest(g.persistence.dir, geoManifest{
		storageGeneration: storageGeneration, walOffset: offset, indexID: g.Metadata().IndexID,
	}); err != nil {
		return err
	}
	g.persistence.storageGeneration = storageGeneration
	return nil
}

func (g *Index) Vacuum() error {
	if err := g.Checkpoint(); err != nil {
		return err
	}
	g.readers.WaitForPriorReaders()
	return persist.RemoveObsolete(g.persistence.dir, "geo-*.base", geoGenerationPath(g.persistence.dir, g.persistence.storageGeneration))
}
