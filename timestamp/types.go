// Package timestamp implements a block-local timestamp index optimized for
// immutable, structure-of-arrays base data and small append-only deltas.
package timestamp

import (
	"errors"
	"math"
)

type ImageID uint64

type Timemarker uint32

const TimestampShift uint8 = 24

const (
	TimestampIndexMagic   uint32 = 0x54534958 // TSIX
	TimestampIndexVersion uint16 = 1
	DefaultBlockSize             = 65_536
	DefaultDeltaCapacity         = 2_048
)

var (
	ErrBackpressure       = errors.New("timestamp: block delta is awaiting compaction")
	ErrBatchInFlight      = errors.New("timestamp: write batch is already in flight")
	ErrBatchTooLarge      = errors.New("timestamp: write batch exceeds delta capacity")
	ErrClosed             = errors.New("timestamp: index is closed")
	ErrDuplicateImageID   = errors.New("timestamp: duplicate image ID")
	ErrInvalidBase        = errors.New("timestamp: invalid base arrays")
	ErrInvalidConfig      = errors.New("timestamp: invalid configuration")
	ErrInvalidRange       = errors.New("timestamp: timestamp cannot be represented by this index")
	ErrInvalidWrite       = errors.New("timestamp: invalid write operation")
	ErrUninitializedBatch = errors.New("timestamp: write batch was not created with NewWriteBatch")
)

func EncodeTimemarker(timestamp, offset uint64) Timemarker {
	return Timemarker((timestamp - offset) >> TimestampShift)
}

func EncodeTimemarkerChecked(timestamp, offset uint64) (Timemarker, error) {
	if timestamp < offset || ((timestamp-offset)>>TimestampShift) > math.MaxUint32 {
		return 0, ErrInvalidRange
	}
	return EncodeTimemarker(timestamp, offset), nil
}

func DecodeTimemarker(marker Timemarker, offset uint64) uint64 {
	return offset + (uint64(marker) << TimestampShift)
}

type TimestampIndexHeader struct {
	Magic      uint32
	Version    uint16
	Flags      uint16
	Count      uint64
	StartTime  uint64
	EndTime    uint64
	OffsetTime uint64
	Shift      uint8
	_          [7]byte
	Generation uint64
}

func (h TimestampIndexHeader) Validate() error {
	if h.Magic != TimestampIndexMagic || h.Version != TimestampIndexVersion || h.Shift != TimestampShift {
		return ErrInvalidBase
	}
	if h.Count != 0 && h.StartTime >= h.EndTime {
		return ErrInvalidBase
	}
	return nil
}

type TimestampBlockMeta struct {
	StartTime    uint64
	EndTime      uint64
	MarkerOffset uint64
	IDOffset     uint64
	Count        uint32
	Flags        uint32
	Generation   uint64
}

type Record struct {
	Timestamp uint64
	ImageID   ImageID
}

type WriteOp uint8

const (
	WriteAdd WriteOp = iota + 1
	WriteDelete
)

type TimestampWrite struct {
	Timestamp uint64
	ImageID   ImageID
	Op        WriteOp
}

type Config struct {
	BlockSize          int
	DeltaCapacity      int
	WriteQueueCapacity int
	CompactionQueue    int
	Durability         Durability
}

type Durability uint8

const (
	DurabilityOS Durability = iota
	DurabilitySync
)

func (c Config) withDefaults() (Config, error) {
	if c.BlockSize < 0 || c.DeltaCapacity < 0 || c.WriteQueueCapacity < 0 || c.CompactionQueue < 0 {
		return Config{}, ErrInvalidConfig
	}
	if c.Durability > DurabilitySync {
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
