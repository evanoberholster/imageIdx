package timeidx

import (
	"cmp"
	"context"
	"errors"
	"os"
	"slices"
	"sync"
	"sync/atomic"

	common "github.com/evanoberholster/imageIdx/common"
	"github.com/evanoberholster/imageIdx/internal/storage"
)

type indexedRecord struct {
	marker    timemarker
	timestamp uint64
	imageID   ImageID
}

// Index is one compressed timestamp candidate index. Readers search immutable
// block views without locks; a single writer owns delta publication.
type Index struct {
	namespace  common.Namespace
	flags      uint32
	offset     uint64
	startTime  uint64
	endTime    uint64
	generation atomic.Uint64
	baseCount  atomic.Uint64
	config     Config
	directory  atomic.Pointer[blockDirectory]
	readers    storage.ReaderTracker

	writes        chan *Batch
	barriers      chan chan barrierResult
	rotate        chan rotateRequest
	compactions   chan compactionTask
	compactDone   chan compactionResult
	stop          chan struct{}
	closed        atomic.Bool
	pending       atomic.Int64
	closeOnce     sync.Once
	lifecycle     sync.RWMutex
	workers       sync.WaitGroup
	writerNeeds   []uint32
	writerTouched []int
	// writerMarks and writerBlocks cache per-write marker encodings and
	// block indexes from applyBatch validation, so the append pass
	// neither re-encodes timestamps nor re-searches the directory.
	// Writer-loop owned and reused across batches.
	writerMarks  []uint64
	writerBlocks []int
	// writeGroup and writeResults buffer one coalesced writer-loop group.
	// Writer-loop owned and reused across groups.
	writeGroup   []*Batch
	writeResults []error

	// failMu guards failed, the latched first WAL error. While set,
	// writes fail fast and maintenance refuses, until close and reopen.
	failMu sync.Mutex
	failed error

	// lock is the store directory lock, nil for in-memory indexes.
	lock *os.File

	persistence *persistentState
}

// fail latches the first WAL error wrapped with ErrRecoveryRequired and
// reports the latched error. Later failures keep the first one so the
// recovery story stays singular.
func (i *Index) fail(err error) error {
	i.failMu.Lock()
	defer i.failMu.Unlock()
	if i.failed == nil {
		i.failed = errors.Join(ErrRecoveryRequired, err)
	}
	return i.failed
}

// failedErr reports the latched WAL failure, if any.
func (i *Index) failedErr() error {
	i.failMu.Lock()
	defer i.failMu.Unlock()
	return i.failed
}

// New constructs an in-memory index from stable metadata and canonical
// timestamp entries.
func New(spec Spec, config Config) (*Index, error) {
	config, err := config.withDefaults()
	if err != nil {
		return nil, err
	}

	prepared := make([]indexedRecord, len(spec.Entries))
	seen := make(map[ImageID]struct{}, len(spec.Entries))
	for i, record := range spec.Entries {
		if _, duplicate := seen[record.ImageID]; duplicate {
			return nil, ErrDuplicateImageID
		}
		seen[record.ImageID] = struct{}{}
		marker, encodeErr := encodeTimemarkerChecked(record.Timestamp, spec.Offset)
		if encodeErr != nil {
			return nil, encodeErr
		}
		prepared[i] = indexedRecord{marker: marker, timestamp: record.Timestamp, imageID: record.ImageID}
	}
	// SortFunc avoids sort.Slice's reflective dispatch on the bulk-load
	// path; ordering matches newBase's validation exactly.
	slices.SortFunc(prepared, func(a, b indexedRecord) int {
		if result := cmp.Compare(a.marker, b.marker); result != 0 {
			return result
		}
		return cmp.Compare(a.imageID, b.imageID)
	})

	directory := buildDirectory(prepared, spec.Offset, config)
	index := &Index{
		namespace:     spec.Namespace,
		flags:         spec.Flags,
		offset:        spec.Offset,
		config:        config,
		writes:        make(chan *Batch, config.WriteQueueCapacity),
		barriers:      make(chan chan barrierResult),
		rotate:        make(chan rotateRequest),
		compactions:   make(chan compactionTask, max(config.CompactionQueue, len(directory.entries))),
		compactDone:   make(chan compactionResult, max(config.CompactionQueue, len(directory.entries))),
		stop:          make(chan struct{}),
		writerNeeds:   make([]uint32, len(directory.entries)),
		writerTouched: make([]int, 0, 256),
	}
	index.generation.Store(1)
	index.baseCount.Store(uint64(len(spec.Entries)))
	if len(prepared) != 0 {
		index.startTime = prepared[0].timestamp
		last := prepared[0].timestamp
		for i := 1; i < len(prepared); i++ {
			if prepared[i].timestamp < index.startTime {
				index.startTime = prepared[i].timestamp
			}
			if prepared[i].timestamp > last {
				last = prepared[i].timestamp
			}
		}
		if last != ^uint64(0) {
			index.endTime = last + 1
		} else {
			index.endTime = last
		}
	}
	index.directory.Store(directory)
	index.workers.Add(2)
	go index.writerLoop()
	go index.compactorLoop()
	return index, nil
}

func buildDirectory(records []indexedRecord, offset uint64, config Config) *blockDirectory {
	if len(records) == 0 {
		base, _ := newBase(nil, nil)
		return &blockDirectory{entries: []blockEntry{{
			startMarker: 0,
			endMarker:   markerSpaceEnd,
			meta: blockMetadata{
				StartTime:  offset,
				EndTime:    timestampForBoundary(markerSpaceEnd, offset),
				Generation: 1,
			},
			block: newBlock(base, config.DeltaCapacity),
		}}}
	}

	starts := make([]int, 0, (len(records)+config.BlockSize-1)/config.BlockSize)
	starts = append(starts, 0)
	for start := 0; start < len(records); {
		next := min(start+config.BlockSize, len(records))
		for next < len(records) && records[next].marker == records[next-1].marker {
			next++
		}
		if next < len(records) {
			starts = append(starts, next)
		}
		start = next
	}

	entries := make([]blockEntry, len(starts))
	var markerOffset uint64
	for i, start := range starts {
		end := len(records)
		if i+1 < len(starts) {
			end = starts[i+1]
		}
		startMarker := uint64(0)
		if i > 0 {
			startMarker = uint64(records[start].marker)
		}
		endMarker := markerSpaceEnd
		if i+1 < len(starts) {
			endMarker = uint64(records[end].marker)
		}
		markers := make([]timemarker, end-start)
		ids := make([]ImageID, end-start)
		for j := start; j < end; j++ {
			markers[j-start] = records[j].marker
			ids[j-start] = records[j].imageID
		}
		base, _ := newBase(markers, ids)
		entries[i] = blockEntry{
			startMarker: startMarker,
			endMarker:   endMarker,
			meta: blockMetadata{
				StartTime:    timestampForBoundary(startMarker, offset),
				EndTime:      timestampForBoundary(endMarker, offset),
				MarkerOffset: markerOffset * 4,
				IDOffset:     markerOffset * 8,
				Count:        uint32(end - start),
				Generation:   1,
			},
			block: newBlock(base, config.DeltaCapacity),
		}
		markerOffset += uint64(end - start)
	}
	return &blockDirectory{entries: entries}
}

// Metadata returns a copied summary of the current base generation.
func (i *Index) Metadata() Metadata {
	return Metadata{
		Namespace: i.namespace, Flags: i.flags, Generation: i.generation.Load(),
		Offset: i.offset, BaseCount: i.baseCount.Load(), BlockCount: uint32(len(i.directory.Load().entries)),
	}
}

// PendingCompactions reports in-flight block compactions.
func (i *Index) PendingCompactions() int64 { return i.pending.Load() }

// WaitIdle waits until background compactions drain or ctx is cancelled.
func (i *Index) WaitIdle(ctx context.Context) error {
	return storage.WaitForCondition(ctx, func() bool { return i.pending.Load() == 0 })
}

// Close stops background workers, closes the WAL, and releases mmap regions.
func (i *Index) Close() error {
	var closeErr error
	i.closeOnce.Do(func() {
		i.lifecycle.Lock()
		defer i.lifecycle.Unlock()
		i.closed.Store(true)
		close(i.stop)
		i.workers.Wait()
		i.readers.WaitForPriorReaders()
		if i.persistence != nil && i.persistence.wal != nil {
			// An already-closed handle reports os.ErrClosed, which is
			// tolerated so a broken WAL can never brick cleanup (and
			// with it, lock release); any other error propagates.
			if err := i.persistence.wal.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
				closeErr = err
			}
		}
		if i.persistence != nil {
			for _, mapping := range i.persistence.mappings {
				if err := storage.Unmap(mapping); err != nil && closeErr == nil {
					closeErr = err
				}
			}
		}
		// Last: releasing the directory lock admits the next opener.
		// A nil lock (in-memory index) is a no-op.
		if err := storage.ReleaseLock(i.lock); err != nil && closeErr == nil {
			closeErr = err
		}
	})
	return closeErr
}
