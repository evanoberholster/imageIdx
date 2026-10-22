package timeidx

import (
	"context"
	"errors"
	"os"
	"sync"
	"sync/atomic"

	"github.com/evanoberholster/imageIdx/internal/pagealloc"
	"github.com/evanoberholster/imageIdx/internal/storage"
	schema "github.com/evanoberholster/imageIdx/schema"
)

type Index struct {
	namespace   schema.Namespace
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
	currentLive []storage.Extent // immutable extents of the published generation; writer-owned
	root        storage.Extent
	slot        int
	// Per-index seams are nil in production and never shared by namespaces.
	fault     func(string) error
	crash     func(string)
	lastStats CommitStats
	scratch   writeScratch // serialized writer-owned temporary routing storage

	// snapshotReaders is set on handles returned by Snapshot. It keeps the
	// snapshot's reader token on the live index instead of the cloned handle's
	// zero-value tracker.
	snapshotReaders *storage.ReaderTracker
	snapshotToken   uint32
	snapshotOnce    sync.Once
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

func (i *Index) readerTracker() *storage.ReaderTracker {
	if i.snapshotReaders != nil {
		return i.snapshotReaders
	}
	return &i.readers
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
	// Snapshot counters and the slice header live on the Go heap, not in mmap.
	v := i.view.Load()
	return Metadata{Namespace: i.namespace, Flags: i.flags, Offset: i.offset, Generation: v.generation, BaseCount: v.count, BlockCount: uint32(len(v.shards))}
}

// SourceGeneration is the contiguous database completeness watermark supplied
// by the application. Zero means no application checkpoint has been recorded.
func (i *Index) SourceGeneration() uint64 { return i.view.Load().sourceGeneration }

// CheckSourceGeneration detects logical staleness, not structural corruption.
func (i *Index) CheckSourceGeneration(required uint64) error {
	if i.isClosed() {
		return ErrClosed
	}
	if i.SourceGeneration() < required {
		return ErrIndexStale
	}
	return nil
}
func (i *Index) Namespace() schema.Namespace        { return i.namespace }
func (i *Index) Generation() uint64                 { return i.view.Load().generation }
func (i *Index) Len() uint64                        { return i.view.Load().count }
func (i *Index) PendingCompactions() int64          { return 0 }
func (i *Index) WaitIdle(ctx context.Context) error { return ctx.Err() }

// barrier reports whether a compatibility call may proceed. Writes are already
// durable when Apply returns, so it never advances the generation or rewrites
// data; it only surfaces closed, latched-failure and (when required) in-memory
// state. requirePersistent additionally rejects indexes with no backing file.
func (i *Index) barrier(requirePersistent bool) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.isClosed() {
		return ErrClosed
	}
	if i.failed != nil {
		return i.failed
	}
	if requirePersistent && i.file == nil {
		return ErrNotPersistent
	}
	return nil
}

// Checkpoint is a compatibility barrier requiring a persistent index.
// All accepted mutations are already compacted.
func (i *Index) Checkpoint() error { return i.barrier(true) }

// Sync is a compatibility barrier: successful changed Apply calls already sync.
func (i *Index) Sync() error { return i.Checkpoint() }

// Vacuum is a compatibility barrier; it does not compact or shrink the file.
func (i *Index) Vacuum() error { return i.Checkpoint() }

// Compact is a compatibility barrier; accepted mutations are already compacted.
func (i *Index) Compact() error { return i.barrier(false) }
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
	i.readerTracker().WaitForPriorReaders()
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
