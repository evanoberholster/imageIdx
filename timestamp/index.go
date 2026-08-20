package timestamp

import (
	"context"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

type indexedRecord struct {
	marker    Timemarker
	timestamp uint64
	imageID   ImageID
}

type Index struct {
	Header TimestampIndexHeader

	offset    uint64
	config    Config
	directory atomic.Pointer[blockDirectory]
	epoch     readerEpoch

	writes        chan *TimestampWriteBatch
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

	persistenceMu sync.Mutex
	storeDir      string
	wal           *timestampWAL
	mappings      [][]byte
}

func New(records []Record, offset uint64, config Config) (*Index, error) {
	config, err := config.withDefaults()
	if err != nil {
		return nil, err
	}

	prepared := make([]indexedRecord, len(records))
	seen := make(map[ImageID]struct{}, len(records))
	for i, record := range records {
		if _, duplicate := seen[record.ImageID]; duplicate {
			return nil, ErrDuplicateImageID
		}
		seen[record.ImageID] = struct{}{}
		marker, encodeErr := EncodeTimemarkerChecked(record.Timestamp, offset)
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

	directory := buildDirectory(prepared, offset, config)
	index := &Index{
		Header: TimestampIndexHeader{
			Magic:      TimestampIndexMagic,
			Version:    TimestampIndexVersion,
			Count:      uint64(len(records)),
			OffsetTime: offset,
			Shift:      TimestampShift,
			Generation: 1,
		},
		offset:        offset,
		config:        config,
		writes:        make(chan *TimestampWriteBatch, config.WriteQueueCapacity),
		barriers:      make(chan chan barrierResult),
		compactions:   make(chan compactionTask, max(config.CompactionQueue, len(directory.entries))),
		compactDone:   make(chan compactionResult, max(config.CompactionQueue, len(directory.entries))),
		stop:          make(chan struct{}),
		writerNeeds:   make([]uint32, len(directory.entries)),
		writerTouched: make([]int, 0, 256),
	}
	if len(prepared) != 0 {
		index.Header.StartTime = prepared[0].timestamp
		last := prepared[0].timestamp
		for i := 1; i < len(prepared); i++ {
			if prepared[i].timestamp < index.Header.StartTime {
				index.Header.StartTime = prepared[i].timestamp
			}
			if prepared[i].timestamp > last {
				last = prepared[i].timestamp
			}
		}
		if last != ^uint64(0) {
			index.Header.EndTime = last + 1
		} else {
			index.Header.EndTime = last
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
		base, _ := NewTimestampBase(nil, nil)
		return &blockDirectory{entries: []blockEntry{{
			startMarker: 0,
			endMarker:   markerSpaceEnd,
			meta: TimestampBlockMeta{
				StartTime:  offset,
				EndTime:    timestampForBoundary(markerSpaceEnd, offset),
				Generation: 1,
			},
			block: newTimestampBlock(base, config.DeltaCapacity),
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
		markers := make([]Timemarker, end-start)
		ids := make([]ImageID, end-start)
		for j := start; j < end; j++ {
			markers[j-start] = records[j].marker
			ids[j-start] = records[j].imageID
		}
		base, _ := NewTimestampBase(markers, ids)
		entries[i] = blockEntry{
			startMarker: startMarker,
			endMarker:   endMarker,
			meta: TimestampBlockMeta{
				StartTime:    timestampForBoundary(startMarker, offset),
				EndTime:      timestampForBoundary(endMarker, offset),
				MarkerOffset: markerOffset * 4,
				IDOffset:     markerOffset * 8,
				Count:        uint32(end - start),
				Generation:   1,
			},
			block: newTimestampBlock(base, config.DeltaCapacity),
		}
		markerOffset += uint64(end - start)
	}
	return &blockDirectory{entries: entries}
}

func (i *Index) OffsetTime() uint64 { return i.offset }

func (i *Index) BlockCount() int { return len(i.directory.Load().entries) }

func (i *Index) PendingCompactions() int64 { return i.pending.Load() }

func (i *Index) WaitIdle(ctx context.Context) error {
	ticker := time.NewTicker(100 * time.Microsecond)
	defer ticker.Stop()
	for {
		if i.pending.Load() == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (i *Index) Close() error {
	var closeErr error
	i.closeOnce.Do(func() {
		i.lifecycle.Lock()
		defer i.lifecycle.Unlock()
		i.closed.Store(true)
		close(i.stop)
		i.workers.Wait()
		i.epoch.synchronize()
		if i.wal != nil {
			closeErr = i.wal.close()
		}
		for _, mapping := range i.mappings {
			if err := unmapFile(mapping); err != nil && closeErr == nil {
				closeErr = err
			}
		}
	})
	return closeErr
}
