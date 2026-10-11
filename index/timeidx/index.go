package timeidx

import (
	"context"
	"errors"
	"os"
	"sync"
	"sync/atomic"

	"github.com/evanoberholster/imageIdx/common"
	"github.com/evanoberholster/imageIdx/internal/pagealloc"
	"github.com/evanoberholster/imageIdx/internal/storage"
)

type Index struct {
	namespace   common.Namespace
	flags       uint32
	offset      uint64 // compatibility metadata only
	config      Config
	mu          sync.Mutex
	view        atomic.Pointer[snapshot]
	closed      atomic.Bool
	ownerClosed *atomic.Bool
	readers     storage.ReaderTracker
	failed      error
	file        *storage.MappedFile
	retired     []*storage.MappedFile
	path        string
	lock        *os.File
	alloc       *pagealloc.Allocator
	live        []storage.Extent
	root        storage.Extent
	slot        int
	// Per-index seams are nil in production and never shared by namespaces.
	fault     func(string) error
	crash     func(string)
	lastStats CommitStats
	scratch   writeScratch // serialized writer-owned temporary routing storage
}

type CommitStats struct {
	LogicalBytesWritten uint64
	AllocatedBytes      uint64
	RewrittenShards     int
	FileBytes           uint64
}

func (i *Index) isClosed() bool {
	return i.closed.Load() || (i.ownerClosed != nil && i.ownerClosed.Load())
}

func (i *Index) CommitStats() CommitStats { i.mu.Lock(); defer i.mu.Unlock(); return i.lastStats }

func NewMemory(spec Spec, config Config) (*Index, error) {
	cfg, err := config.withDefaults()
	if err != nil {
		return nil, err
	}
	shards, err := initialShards(spec.Entries, int(cfg.MaxShardBytes/recordBytes))
	if err != nil {
		return nil, err
	}
	i := &Index{namespace: spec.Namespace, flags: spec.Flags, offset: spec.Offset, config: cfg}
	i.view.Store(&snapshot{shards: shards, generation: 1, count: uint64(len(spec.Entries))})
	return i, nil
}

func (i *Index) Metadata() Metadata {
	token := i.readers.Acquire()
	defer i.readers.Release(token)
	v := i.view.Load()
	return Metadata{Namespace: i.namespace, Flags: i.flags, Offset: i.offset, Generation: v.generation, BaseCount: v.count, BlockCount: uint32(len(v.shards))}
}
func (i *Index) Namespace() common.Namespace        { return i.namespace }
func (i *Index) Generation() uint64                 { return i.view.Load().generation }
func (i *Index) Len() uint64                        { return i.view.Load().count }
func (i *Index) PendingCompactions() int64          { return 0 }
func (i *Index) WaitIdle(ctx context.Context) error { return ctx.Err() }

// All accepted mutations are already compacted. These compatibility barriers
// do not advance the generation or rewrite data.
func (i *Index) Checkpoint() error {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.isClosed() {
		return ErrClosed
	}
	if i.failed != nil {
		return i.failed
	}
	if i.file == nil {
		return ErrNotPersistent
	}
	return nil
}
func (i *Index) Sync() error   { return i.Checkpoint() }
func (i *Index) Vacuum() error { return i.Checkpoint() }
func (i *Index) Compact() error {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.isClosed() {
		return ErrClosed
	}
	return i.failed
}
func (i *Index) OnDiskBytes() uint64 {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.file == nil {
		return 0
	}
	return uint64(len(i.file.Bytes()))
}

func (i *Index) Close() error {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.closed.Swap(true) {
		return nil
	}
	i.readers.WaitForPriorReaders()
	i.scratch = writeScratch{}
	var err error
	if i.file != nil {
		err = i.file.Close()
		i.file = nil
	}
	for _, m := range i.retired {
		err = errors.Join(err, m.Close())
	}
	i.retired = nil
	return errors.Join(err, storage.ReleaseLock(i.lock))
}

func (i *Index) inject(stage string) error {
	if i.crash != nil {
		i.crash(stage)
	}
	if i.fault != nil {
		return i.fault(stage)
	}
	return nil
}
