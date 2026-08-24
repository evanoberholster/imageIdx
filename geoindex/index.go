package geoindex

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/evanoberholster/imageIdx/internal/indexutil"
)

const (
	geoShardBits  = 8
	geoShardCount = 1 << geoShardBits
	geoKeyBits    = 36
)

type geoShard struct {
	sparse sparseRef
	dense  bitmapRef
}

type indexView struct {
	version         uint16
	flags           uint32
	indexID         uint64
	generation      uint64
	sparseCellCount uint64
	denseCellCount  uint64
	shards          [geoShardCount]*geoShard
}

// Index is one fixed-level geographic candidate index. Readers load one
// immutable view and never inspect writer-side mutations.
type Index struct {
	view    atomic.Pointer[indexView]
	config  Config
	readers indexutil.ReaderTracker

	writerMu sync.Mutex
	pending  map[ImageID]Mutation
	closed   atomic.Bool
	stop     chan struct{}
	publish  chan struct{}
	workers  sync.WaitGroup

	persistence *persistentState
	mapping     []byte
}

// New constructs an index from canonical coordinates.
func New(spec Spec, config Config) (*Index, error) {
	config, err := config.withDefaults()
	if err != nil {
		return nil, err
	}
	cells := make(map[S2Key][]ImageID)
	seen := make(map[ImageID]struct{}, len(spec.Entries))
	for _, entry := range spec.Entries {
		if entry.ImageID == 0 {
			return nil, ErrInvalidImageID
		}
		if _, exists := seen[entry.ImageID]; exists {
			return nil, ErrDuplicateImageID
		}
		key, keyErr := KeyFromLatLng(entry.Latitude, entry.Longitude)
		if keyErr != nil {
			return nil, keyErr
		}
		seen[entry.ImageID] = struct{}{}
		cells[key] = append(cells[key], entry.ImageID)
	}
	return newIndex(spec.IndexID, spec.Flags, 1, cells, config), nil
}

func newIndex(indexID uint64, flags uint32, generation uint64, cells map[S2Key][]ImageID, config Config) *Index {
	index := &Index{
		config:  config,
		pending: make(map[ImageID]Mutation),
		stop:    make(chan struct{}),
		publish: make(chan struct{}, 1),
	}
	index.view.Store(buildView(indexID, flags, generation, cells))
	index.startPublisher()
	return index
}

func newIndexFromView(view *indexView, config Config, mapping []byte) (*Index, error) {
	if view == nil || view.version != geoFormatVersion {
		return nil, ErrCorruptStorage
	}
	index := &Index{
		config:  config,
		pending: make(map[ImageID]Mutation),
		stop:    make(chan struct{}),
		publish: make(chan struct{}, 1),
		mapping: mapping,
	}
	index.view.Store(view)
	index.startPublisher()
	return index, nil
}

func buildView(indexID uint64, flags uint32, generation uint64, cells map[S2Key][]ImageID) *indexView {
	view := &indexView{version: geoFormatVersion, flags: flags, indexID: indexID, generation: generation}
	grouped := make(map[int]map[S2Key][]ImageID)
	for key, ids := range cells {
		if len(ids) == 0 {
			continue
		}
		shardIndex := shardForKey(key)
		if grouped[shardIndex] == nil {
			grouped[shardIndex] = make(map[S2Key][]ImageID)
		}
		grouped[shardIndex][key] = ids
	}
	for shardIndex, shardCells := range grouped {
		shard := &geoShard{sparse: buildSparse(shardCells), dense: buildBitmap(shardCells)}
		view.shards[shardIndex] = shard
		view.sparseCellCount += uint64(shard.sparse.cellCount())
		view.denseCellCount += uint64(shard.dense.cellCount())
	}
	return view
}

func shardForKey(key S2Key) int {
	return int(uint64(key) >> (geoKeyBits - geoShardBits))
}

func (v *indexView) containsAt(key S2Key, id ImageID) bool {
	if v == nil || key == 0 {
		return false
	}
	shard := v.shards[shardForKey(key)]
	if shard == nil {
		return false
	}
	for _, value := range shard.sparse.lookup(key, nil) {
		if value == id {
			return true
		}
	}
	bitmap, ok := shard.dense.lookup(key)
	return ok && bitmap.Contains(id)
}

func (g *Index) startPublisher() {
	g.workers.Add(1)
	go func() {
		defer g.workers.Done()
		ticker := time.NewTicker(g.config.PublishInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				_ = g.Flush()
			case <-g.publish:
				_ = g.Flush()
			case <-g.stop:
				return
			}
		}
	}()
}

// Metadata returns a copied summary of the current generation.
func (g *Index) Metadata() Metadata {
	view := g.view.Load()
	return Metadata{
		IndexID: view.indexID, Flags: view.flags, Generation: view.generation,
		SparseCellCount: view.sparseCellCount, DenseCellCount: view.denseCellCount,
	}
}

// Pending returns the number of unpublished image mutations.
func (g *Index) Pending() int {
	g.writerMu.Lock()
	defer g.writerMu.Unlock()
	return len(g.pending)
}

func (g *Index) WaitIdle(ctx context.Context) error {
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

func (g *Index) Close() error {
	if g.persistence != nil {
		g.persistence.mu.Lock()
		defer g.persistence.mu.Unlock()
	}
	g.writerMu.Lock()
	if g.closed.Load() {
		g.writerMu.Unlock()
		return nil
	}
	_ = g.flushLocked()
	g.closed.Store(true)
	close(g.stop)
	var result error
	if g.persistence != nil && g.persistence.wal != nil {
		result = g.persistence.wal.close()
	}
	g.writerMu.Unlock()
	g.workers.Wait()
	g.readers.WaitForPriorReaders()
	if g.mapping != nil {
		if err := unmapFile(g.mapping); err != nil && result == nil {
			result = err
		}
		g.mapping = nil
	}
	return result
}
