// Package timeidx indexes exact unsigned nanoseconds since 1900-01-01 UTC.
// Each persistent namespace owns one COW mmap file. Published SoA shards are
// immutable; searches pin one coherent snapshot without taking writer locks.
package timeidx

import (
	"errors"
	"time"

	"github.com/evanoberholster/imageIdx/common"
	"github.com/evanoberholster/imageIdx/internal/storage"
)

type TimeKey uint64

// TimestampShift is retained for source compatibility only. The new index
// does not shift, quantize, or rebase timestamps.
const TimestampShift uint8 = 24
const DefaultBlockSize = 65_536
const DefaultDeltaCapacity = 2_048
const epochUnixSeconds int64 = -2_208_988_800

// EncodeTime converts a UTC instant without using UnixNano or time.Duration,
// and supports every instant in calendar years 1900 through 2100 inclusive.
// Local times without a timezone must be resolved by the caller.
func EncodeTime(t time.Time) (TimeKey, error) {
	u := t.UTC()
	if u.Year() < 1900 || u.Year() > 2100 {
		return 0, ErrInvalidRange
	}
	seconds := uint64(u.Unix() - epochUnixSeconds)
	return TimeKey(seconds*1_000_000_000 + uint64(u.Nanosecond())), nil
}

func DecodeTime(key TimeKey) (time.Time, error) {
	const end2100 uint64 = 6_342_969_600_000_000_000 // 2101-01-01, exclusive
	if uint64(key) >= end2100 {
		return time.Time{}, ErrInvalidRange
	}
	return time.Unix(epochUnixSeconds+int64(uint64(key)/1_000_000_000), int64(uint64(key)%1_000_000_000)).UTC(), nil
}

var (
	ErrBackpressure       = errors.New("timeidx: backpressure") // retained; synchronous COW does not return it
	ErrBatchInFlight      = errors.New("timeidx: write batch is already in flight")
	ErrBatchTooLarge      = errors.New("timeidx: write batch exceeds capacity")
	ErrClosed             = errors.New("timeidx: index is closed")
	ErrDuplicateImageID   = errors.New("timeidx: duplicate value ID")
	ErrInvalidBase        = errors.New("timeidx: invalid base arrays")
	ErrInvalidConfig      = errors.New("timeidx: invalid configuration")
	ErrInvalidMutation    = errors.New("timeidx: invalid mutation")
	ErrInvalidRange       = errors.New("timeidx: unsupported calendar time")
	ErrNotPersistent      = errors.New("timeidx: index is not persistent")
	ErrUninitializedBatch = errors.New("timeidx: write batch was not created with NewBatch")
	ErrUnsupportedEndian  = errors.New("timeidx: mmap requires little-endian host")
	ErrUnsupported        = errors.New("timeidx: unsupported format; rebuild from canonical timestamps")
	ErrCorrupt            = errors.New("timeidx: corrupt namespace file")
	ErrCorruptManifest    = ErrCorrupt                                 // legacy error spelling
	ErrCorruptWAL         = errors.New("timeidx: obsolete WAL format") // deprecated; no WAL in v4
	ErrNamespaceMismatch  = errors.New("timeidx: namespace identity mismatch")
	ErrInvalidNamespace   = errors.New("timeidx: invalid namespace")
	ErrStartupRequired    = errors.New("timeidx: namespace registry has not been published")
	ErrNamespaceFrozen    = errors.New("timeidx: namespace membership is immutable after startup")
	ErrRebuildRequired    = errors.New("timeidx: index rebuild required")
	ErrNamespaceNotFound  = errors.New("timeidx: namespace does not exist")
	ErrNamespaceExists    = storage.ErrExists
	ErrAmbiguousNamespace = errors.New("timeidx: specify a namespace when opening a multi-namespace directory")
	ErrStoreBusy          = storage.ErrBusy
	ErrStoreExists        = storage.ErrExists
	ErrRecoveryRequired   = errors.New("timeidx: uncertain commit; close and reopen")
)

type Entry struct {
	Timestamp uint64
	ImageID   common.ValueID
}

type Spec struct {
	Namespace common.Namespace
	Name      string
	Flags     uint32
	// Offset is retained as metadata for source compatibility, not used in encoding.
	Offset  uint64
	Entries []Entry
}

func (s Spec) NamespaceDef() common.NamespaceDef {
	return common.NamespaceDef{Namespace: s.Namespace, Name: s.Name, Kind: common.KindTimestamp}
}

type Metadata struct {
	Namespace  common.Namespace
	Flags      uint32
	Generation uint64
	Offset     uint64
	BaseCount  uint64
	BlockCount uint32 // number of size-bounded shards
}

type Config struct {
	// MaxShardBytes is the maximum SoA payload per shard (default 4 KiB,
	// selected by the clustered/historical shard sweeps; see BENCHMARKS.md).
	MaxShardBytes int64
	// MaxFileBytes is initial sparse arena capacity, NOT a growth limit.
	MaxFileBytes int64
	// BlockSize is a deprecated record-count alias for MaxShardBytes.
	BlockSize int
	// Deprecated queue/delta sizing fields are validated but no longer used.
	DeltaCapacity      int
	WriteQueueCapacity int
	CompactionQueue    int
}

func (c Config) withDefaults() (Config, error) {
	if c.MaxShardBytes < 0 || c.MaxFileBytes < 0 || c.BlockSize < 0 || c.BlockSize > 1<<24 ||
		c.DeltaCapacity < 0 || c.DeltaCapacity == 1 || c.DeltaCapacity > 1<<20 || c.WriteQueueCapacity < 0 || c.WriteQueueCapacity > 1<<16 ||
		c.CompactionQueue < 0 || c.CompactionQueue > 1<<16 {
		return Config{}, ErrInvalidConfig
	}
	if c.MaxShardBytes == 0 {
		c.MaxShardBytes = 4 << 10
		if c.BlockSize != 0 {
			c.MaxShardBytes = int64(c.BlockSize) * 16
		}
	}
	if c.MaxFileBytes == 0 {
		c.MaxFileBytes = 4 << 20
	}
	if c.MaxShardBytes < 16 || c.MaxShardBytes > 1<<30 || c.MaxFileBytes < 1024 || c.MaxFileBytes > 1<<41 {
		return Config{}, ErrInvalidConfig
	}
	if uint64(c.MaxFileBytes) > uint64(^uint(0)>>1)&^uint64(4095) {
		return Config{}, ErrInvalidConfig
	}
	return c, nil
}

type Durability = common.Durability

const (
	DurabilityOS   = common.DurabilityOS
	DurabilitySync = common.DurabilitySync
)
