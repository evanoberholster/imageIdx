package geoindex

import (
	"os"
	"path/filepath"
)

func NewPersistent(dir string, indexID uint64, entries []GeoEntry, config Config, durability Durability) (*GeoIndex, error) {
	if durability > DurabilitySync {
		return nil, ErrInvalidConfig
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	index, err := New(indexID, entries, config)
	if err != nil {
		return nil, err
	}
	wal, err := openGeoWAL(filepath.Join(dir, geoWALFilename))
	if err != nil {
		_ = index.Close()
		return nil, err
	}
	index.storeDir = dir
	index.durability = durability
	index.wal = wal
	if err = index.Checkpoint(); err != nil {
		_ = index.Close()
		return nil, err
	}
	return index, nil
}

func OpenPersistent(dir string, config Config, durability Durability) (*GeoIndex, error) {
	if durability > DurabilitySync {
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
	if storageGeneration != manifest.storageGeneration || index.IndexID() != manifest.indexID {
		_ = index.Close()
		return nil, ErrCorruptManifest
	}
	batches, err := replayGeoWAL(filepath.Join(dir, geoWALFilename), manifest.walOffset)
	if err != nil {
		_ = index.Close()
		return nil, err
	}
	index.writerMu.Lock()
	for _, batch := range batches {
		for id, mutation := range batch {
			index.pending[id] = mutation
		}
	}
	err = index.flushLocked()
	index.writerMu.Unlock()
	if err != nil {
		_ = index.Close()
		return nil, err
	}
	wal, err := openGeoWAL(filepath.Join(dir, geoWALFilename))
	if err != nil {
		_ = index.Close()
		return nil, err
	}
	index.storeDir = dir
	index.durability = durability
	index.wal = wal
	index.storeGeneration = storageGeneration
	return index, nil
}

func (g *GeoIndex) Checkpoint() error {
	g.persistenceMu.Lock()
	defer g.persistenceMu.Unlock()
	g.writerMu.Lock()
	defer g.writerMu.Unlock()
	if g.closed.Load() {
		return ErrClosed
	}
	if g.wal == nil || g.storeDir == "" {
		return ErrNotPersistent
	}
	if err := g.flushLocked(); err != nil {
		return err
	}
	offset, err := g.wal.offset()
	if err != nil {
		return err
	}
	storageGeneration := g.storeGeneration + 1
	finalPath := geoGenerationPath(g.storeDir, storageGeneration)
	temporary := finalPath + ".tmp"
	guard := g.config.Epoch.Enter()
	err = writeGeoGeneration(temporary, storageGeneration, g.view.Load())
	guard.Exit()
	if err != nil {
		return err
	}
	if err = os.Rename(temporary, finalPath); err != nil {
		return err
	}
	if err = writeGeoManifest(g.storeDir, geoManifest{
		storageGeneration: storageGeneration, walOffset: offset, indexID: g.IndexID(),
	}); err != nil {
		return err
	}
	g.storeGeneration = storageGeneration
	return nil
}

func (g *GeoIndex) Vacuum() error {
	if err := g.Checkpoint(); err != nil {
		return err
	}
	g.config.Epoch.Synchronize()
	paths, err := filepath.Glob(filepath.Join(g.storeDir, "geo-*.base"))
	if err != nil {
		return err
	}
	current := geoGenerationPath(g.storeDir, g.storeGeneration)
	for _, path := range paths {
		if path != current {
			if err = os.Remove(path); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
	}
	return nil
}
