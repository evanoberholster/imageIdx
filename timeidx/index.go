package timeidx

import (
	"context"
	"sort"
	"sync"
	"sync/atomic"

	"github.com/evanoberholster/imageIdx/internal/indexutil"
	"github.com/evanoberholster/imageIdx/internal/persist"
)

type indexedRecord struct {
	marker    timemarker
	timestamp uint64
	imageID   ImageID
}

// Index is one compressed timestamp candidate index. Readers search immutable
// block views without locks; a single writer owns delta publication.
type Index struct {
	indexID    uint64
	flags      uint32
	offset     uint64
	startTime  uint64
	endTime    uint64
	generation atomic.Uint64
	baseCount  atomic.Uint64
	config     Config
	directory  atomic.Pointer[blockDirectory]
	readers    indexutil.ReaderTracker

	writes        chan *Batch
	barriers      chan chan barrierResult
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

	persistence *persistentState
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
	sort.Slice(prepared, func(i, j int) bool {
		if prepared[i].marker != prepared[j].marker {
			return prepared[i].marker < prepared[j].marker
		}
		return prepared[i].imageID < prepared[j].imageID
	})

	directory := buildDirectory(prepared, spec.Offset, config)
	index := &Index{
		indexID:       spec.IndexID,
		flags:         spec.Flags,
		offset:        spec.Offset,
		config:        config,
		writes:        make(chan *Batch, config.WriteQueueCapacity),
		barriers:      make(chan chan barrierResult),
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
		IndexID: i.indexID, Flags: i.flags, Generation: i.generation.Load(),
		Offset: i.offset, BaseCount: i.baseCount.Load(), BlockCount: uint32(len(i.directory.Load().entries)),
	}
}

// PendingCompactions reports in-flight block compactions.
func (i *Index) PendingCompactions() int64 { return i.pending.Load() }

// WaitIdle waits until background compactions drain or ctx is cancelled.
func (i *Index) WaitIdle(ctx context.Context) error {
	return indexutil.WaitForCondition(ctx, func() bool { return i.pending.Load() == 0 })
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
			closeErr = i.persistence.wal.Close()
		}
		if i.persistence != nil {
			for _, mapping := range i.persistence.mappings {
				if err := persist.Unmap(mapping); err != nil && closeErr == nil {
					closeErr = err
				}
			}
		}
	})
	return closeErr
}
