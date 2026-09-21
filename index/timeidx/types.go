// Package timeidx implements a block-local timestamp index optimized for
// immutable, structure-of-arrays base data and small append-only deltas.
package timeidx

import (
	"errors"
	"math"

	imageidx "github.com/evanoberholster/imageIdx"
)

type ImageID = imageidx.ImageID

type timemarker uint32

const TimestampShift uint8 = 24

const (
	DefaultBlockSize     = 65_536
	DefaultDeltaCapacity = 2_048
)

var (
	ErrBackpressure       = errors.New("timeidx: block delta is awaiting compaction")
	ErrBatchInFlight      = errors.New("timeidx: write batch is already in flight")
	ErrBatchTooLarge      = errors.New("timeidx: write batch exceeds delta capacity")
	ErrClosed             = errors.New("timeidx: index is closed")
	ErrDuplicateImageID   = errors.New("timeidx: duplicate image ID")
	ErrInvalidBase        = errors.New("timeidx: invalid base arrays")
	ErrInvalidConfig      = errors.New("timeidx: invalid configuration")
	ErrInvalidMutation    = errors.New("timeidx: invalid mutation")
	ErrInvalidRange       = errors.New("timeidx: timestamp cannot be represented by this index")
	ErrNotPersistent      = errors.New("timeidx: index is not persistent")
	ErrUninitializedBatch = errors.New("timeidx: write batch was not created with NewBatch")
	ErrUnsupportedEndian  = errors.New("timeidx: mmap format requires a little-endian host")
)

func encodeTimemarker(timestamp, offset uint64) timemarker {
	return timemarker((timestamp - offset) >> TimestampShift)
}

func encodeTimemarkerChecked(timestamp, offset uint64) (timemarker, error) {
	if timestamp < offset || ((timestamp-offset)>>TimestampShift) > math.MaxUint32 {
		return 0, ErrInvalidRange
	}
	return encodeTimemarker(timestamp, offset), nil
}

func decodeTimemarker(marker timemarker, offset uint64) uint64 {
	return offset + (uint64(marker) << TimestampShift)
}

type blockMetadata struct {
	StartTime    uint64
	EndTime      uint64
	MarkerOffset uint64
	IDOffset     uint64
	Count        uint32
	Flags        uint32
	Generation   uint64
}

// Entry is one canonical timestamp to index.
type Entry struct {
	Timestamp uint64
	ImageID   ImageID
}

// Spec describes an index's stable identity, compression offset, and initial
// entries.
type Spec struct {
	IndexID uint64
	Flags   uint32
	Offset  uint64
	Entries []Entry
}

// Metadata is a copied summary of the current persisted/base generation.
// BaseCount deliberately excludes pending Active and Frozen corrections.
type Metadata struct {
	IndexID    uint64
	Flags      uint32
	Generation uint64
	Offset     uint64
	BaseCount  uint64
	BlockCount uint32
}

// Config controls base block sizing, fixed delta capacity, and writer queues.
// The zero value selects the benchmarked defaults.
type Config struct {
	BlockSize          int
	DeltaCapacity      int
	WriteQueueCapacity int
	CompactionQueue    int
}

type Durability = imageidx.Durability

const (
	DurabilityOS   = imageidx.DurabilityOS
	DurabilitySync = imageidx.DurabilitySync
)

func (c Config) withDefaults() (Config, error) {
	if c.BlockSize < 0 || c.DeltaCapacity < 0 || c.WriteQueueCapacity < 0 || c.CompactionQueue < 0 {
		return Config{}, ErrInvalidConfig
	}
	if c.BlockSize == 0 {
		c.BlockSize = DefaultBlockSize
	}
	if c.DeltaCapacity == 0 {
		c.DeltaCapacity = DefaultDeltaCapacity
	}
	if c.WriteQueueCapacity == 0 {
		c.WriteQueueCapacity = 64
	}
	if c.CompactionQueue == 0 {
		c.CompactionQueue = 16
	}
	if c.BlockSize < 1 || c.DeltaCapacity < 2 {
		return Config{}, ErrInvalidConfig
	}
	return c, nil
}

func markerRange(start, end, offset uint64) (lo, hi uint64, ok bool) {
	if start >= end || end <= offset {
		return 0, 0, false
	}
	if start < offset {
		start = offset
	}
	startDelta := start - offset
	lo = startDelta >> TimestampShift
	if lo > math.MaxUint32 {
		return 0, 0, false
	}

	last := end - 1
	if last < offset {
		return 0, 0, false
	}
	lastMarker := (last - offset) >> TimestampShift
	if lastMarker > math.MaxUint32 {
		lastMarker = math.MaxUint32
	}
	hi = lastMarker + 1
	return lo, hi, lo < hi
}
