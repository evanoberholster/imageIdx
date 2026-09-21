// Package timeidx implements a block-local timestamp index optimized for
// immutable, structure-of-arrays base data and small append-only deltas.
package timeidx

import (
	"errors"
	"math"

	imageidx "github.com/evanoberholster/imageIdx"
	"github.com/evanoberholster/imageIdx/internal/indexutil"
	"github.com/evanoberholster/imageIdx/internal/persist"
)

type ImageID = imageidx.ImageID

type timemarker uint32

const TimestampShift uint8 = 24

const (
	DefaultBlockSize     = 65_536
	DefaultDeltaCapacity = 2_048

	// maxBlockSize caps one base block so per-block slices stay well
	// below allocation-panic territory and uint32 count fields never
	// truncate.
	maxBlockSize = 1 << 24
	// maxDeltaCapacity caps one delta so its planes plus hash table
	// stay bounded (~200 bytes per entry worst case across the
	// active/frozen/spare trio).
	maxDeltaCapacity = 1 << 20
	// maxQueueCapacity caps writer channels, which hold pointers and
	// batch shells only.
	maxQueueCapacity = 1 << 16
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
	ErrUnsupported        = errors.New("timeidx: unsupported format")
	// ErrStoreBusy reports another process holding the store lock. It
	// aliases indexutil.ErrBusy; see internal/indexutil.
	ErrStoreBusy = indexutil.ErrBusy
	// ErrStoreExists reports creating a store in a non-empty directory.
	// It aliases indexutil.ErrExists; see internal/indexutil.
	ErrStoreExists = indexutil.ErrExists
	// ErrRecoveryRequired reports a WAL failure that latched the index:
	// every later write and maintenance call fails with it until the
	// index is closed and reopened. It aliases persist.ErrWALFailed.
	// Recovery is close plus reopen: replay restores the last durable
	// prefix and reports a torn tail as a clean stop or ErrCorruptWAL.
	ErrRecoveryRequired = persist.ErrWALFailed
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
	// Upper bounds keep worst-case allocations (delta planes and hash
	// tables, channel buffers, WAL scratch) far from overflow and panic
	// on every platform, including 32-bit int arithmetic.
	if c.BlockSize > maxBlockSize || c.DeltaCapacity > maxDeltaCapacity ||
		c.WriteQueueCapacity > maxQueueCapacity || c.CompactionQueue > maxQueueCapacity {
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
