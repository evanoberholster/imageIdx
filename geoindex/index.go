package geoindex

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

type GeoIndexView struct {
	Version    uint16
	Flags      uint32
	IndexID    uint64
	Generation uint64
	Sparse     GeoSparseRef
	Bitmap     GeoBitmapRef
}

func (v *GeoIndexView) Header() GeoIndexHeader {
	if v == nil {
		return GeoIndexHeader{}
	}
	return GeoIndexHeader{
		Version: v.Version, Flags: uint16(v.Flags), IndexID: v.IndexID, S2Level: GeoS2Level,
		SparseCellCount: uint64(v.Sparse.CellCount()), BitmapCellCount: uint64(v.Bitmap.CellCount()),
		Generation: v.Generation,
	}
}

type pendingMutation struct {
	key    S2Key
	remove bool
}

type GeoIndex struct {
	view   atomic.Pointer[GeoIndexView]
	config Config

	writerMu  sync.Mutex
	pending   map[uint64]pendingMutation
	locations map[uint64]S2Key
	closed    atomic.Bool
	stop      chan struct{}
	workers   sync.WaitGroup

	storeDir        string
	durability      Durability
	wal             *geoWAL
	mappings        [][]byte
	generation      uint64
	storeGeneration uint64
	persistenceMu   sync.Mutex
}

func New(indexID uint64, entries []GeoEntry, config Config) (*GeoIndex, error) {
	config, err := config.withDefaults()
	if err != nil {
		return nil, err
	}
	locations := make(map[uint64]S2Key, len(entries))
	for _, entry := range entries {
		if entry.ImageID == 0 {
			return nil, ErrInvalidImageID
		}
		if _, exists := locations[entry.ImageID]; exists {
			return nil, ErrDuplicateImageID
		}
		key, keyErr := KeyFromLatLng(entry.Latitude, entry.Longitude)
		if keyErr != nil {
			return nil, keyErr
		}
		locations[entry.ImageID] = key
	}
	return newGeoIndex(indexID, 0, 1, locations, config), nil
}

func NewFromCells(indexID uint64, entries []GeoCellEntry, config Config) (*GeoIndex, error) {
	config, err := config.withDefaults()
	if err != nil {
		return nil, err
	}
	locations := make(map[uint64]S2Key, len(entries))
	for _, entry := range entries {
		if entry.ImageID == 0 {
			return nil, ErrInvalidImageID
		}
		if _, exists := locations[entry.ImageID]; exists {
			return nil, ErrDuplicateImageID
		}
		if _, cellErr := CellIDFromS2Key(entry.Key); cellErr != nil {
			return nil, cellErr
		}
		locations[entry.ImageID] = entry.Key
	}
	return newGeoIndex(indexID, 0, 1, locations, config), nil
}

func newGeoIndex(indexID uint64, flags uint32, generation uint64, locations map[uint64]S2Key, config Config) *GeoIndex {
	cells := cellsFromLocations(locations)
	index := &GeoIndex{
		config: config, pending: make(map[uint64]pendingMutation), locations: locations,
		stop: make(chan struct{}), generation: generation,
	}
	index.view.Store(&GeoIndexView{
		Version: GeoFormatVersion, Flags: flags, IndexID: indexID, Generation: generation,
		Sparse: buildSparse(cells), Bitmap: buildBitmap(cells),
	})
	index.startPublisher()
	return index
}

func newGeoIndexFromView(view *GeoIndexView, config Config, mapping []byte) (*GeoIndex, error) {
	if view == nil || view.Version != GeoFormatVersion {
		return nil, ErrCorruptStorage
	}
	locations := locationsFromView(view)
	index := &GeoIndex{
		config: config, pending: make(map[uint64]pendingMutation), locations: locations,
		stop: make(chan struct{}), generation: view.Generation,
	}
	if mapping != nil {
		index.mappings = [][]byte{mapping}
	}
	index.view.Store(view)
	index.startPublisher()
	return index, nil
}

func (g *GeoIndex) startPublisher() {
	g.workers.Add(1)
	go func() {
		defer g.workers.Done()
		ticker := time.NewTicker(g.config.PublishInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				_ = g.Flush()
			case <-g.stop:
				return
			}
		}
	}()
}

func (g *GeoIndex) LoadView() *GeoIndexView { return g.view.Load() }
func (g *GeoIndex) IndexID() uint64         { return g.view.Load().IndexID }
func (g *GeoIndex) Generation() uint64      { return g.view.Load().Generation }
func (g *GeoIndex) Pending() int {
	g.writerMu.Lock()
	defer g.writerMu.Unlock()
	return len(g.pending)
}

func (g *GeoIndex) WaitIdle(ctx context.Context) error {
	ticker := time.NewTicker(100 * time.Microsecond)
	defer ticker.Stop()
	for {
		if g.Pending() == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func cellsFromLocations(locations map[uint64]S2Key) map[S2Key][]uint64 {
	cells := make(map[S2Key][]uint64)
	for id, key := range locations {
		cells[key] = append(cells[key], id)
	}
	return cells
}

func locationsFromView(view *GeoIndexView) map[uint64]S2Key {
	locations := make(map[uint64]S2Key)
	for keyIndex, key := range view.Sparse.keys {
		offset := keyIndex * GeoSparseSlots
		for _, id := range view.Sparse.imageIDs[offset : offset+GeoSparseSlots] {
			if id == 0 {
				break
			}
			locations[id] = key
		}
	}
	for keyIndex, key := range view.Bitmap.keys {
		for _, id := range view.Bitmap.bitmaps[keyIndex].ToArray() {
			locations[id] = key
		}
	}
	return locations
}

func (g *GeoIndex) Close() error {
	g.writerMu.Lock()
	if g.closed.Load() {
		g.writerMu.Unlock()
		return nil
	}
	_ = g.flushLocked()
	g.closed.Store(true)
	close(g.stop)
	var result error
	if g.wal != nil {
		result = g.wal.close()
	}
	g.writerMu.Unlock()
	g.workers.Wait()
	g.config.Epoch.Synchronize()
	for _, mapping := range g.mappings {
		if err := unmapFile(mapping); err != nil && result == nil {
			result = err
		}
	}
	g.mappings = nil
	return result
}
