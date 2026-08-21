// Package bitmapidx implements immutable, copy-on-write bitmap indexes for
// database-wide image identifiers.
package bitmapidx

import (
	"errors"
	"time"
)

type ImageID uint64

type IndexType uint16

const (
	IndexTypeLocation IndexType = iota + 1
	IndexTypePerson
	IndexTypeAlbum
	IndexTypeTag
	IndexTypeObject
	IndexTypeEvent
)

type BitmapKey struct {
	Type    IndexType
	ValueID uint64
}

type Flags = uint32

const (
	BitmapFormatMagic     uint32 = 0x424d4958 // BMIX
	BitmapFormatVersion   uint16 = 1
	DefaultActiveTarget          = 2_048
	DefaultHardDeltaLimit        = 4_095
)

var (
	ErrClosed          = errors.New("bitmapidx: index is closed")
	ErrCorruptSegment  = errors.New("bitmapidx: corrupt segment")
	ErrCorruptManifest = errors.New("bitmapidx: corrupt manifest")
	ErrCorruptWAL      = errors.New("bitmapidx: corrupt write-ahead log")
	ErrDuplicateKey    = errors.New("bitmapidx: duplicate bitmap key")
	ErrDuplicateID     = errors.New("bitmapidx: duplicate index ID")
	ErrInvalidConfig   = errors.New("bitmapidx: invalid configuration")
	ErrInvalidIndex    = errors.New("bitmapidx: invalid bitmap index metadata")
	ErrNotPersistent   = errors.New("bitmapidx: index collection is not persistent")
)

type Config struct {
	// ActiveTarget controls rotation from Active to Frozen. It must remain
	// lower than HardDeltaLimit.
	ActiveTarget int
	// HardDeltaLimit bounds each reader-visible delta. Writers compact or
	// locally apply backpressure rather than publish another layer.
	HardDeltaLimit int
	// CompactDelay is primarily useful to tests and workload tuning. Zero
	// starts background compaction immediately.
	CompactDelay time.Duration
	// Epoch can be shared by a collection so a multi-index query enters once.
	Epoch *Epoch
}

func (c Config) withDefaults() (Config, error) {
	if c.ActiveTarget < 0 || c.HardDeltaLimit < 0 || c.CompactDelay < 0 {
		return Config{}, ErrInvalidConfig
	}
	if c.ActiveTarget == 0 {
		c.ActiveTarget = DefaultActiveTarget
	}
	if c.HardDeltaLimit == 0 {
		c.HardDeltaLimit = DefaultHardDeltaLimit
	}
	if c.ActiveTarget < 1 || c.HardDeltaLimit < 2 || c.ActiveTarget >= c.HardDeltaLimit {
		return Config{}, ErrInvalidConfig
	}
	if c.Epoch == nil {
		c.Epoch = NewEpoch()
	}
	return c, nil
}

type BitmapMutation struct {
	Additions []uint64
	Removals  []uint64
}

type Durability uint8

const (
	DurabilityOS Durability = iota
	DurabilitySync
)

type IndexSpec struct {
	Key      BitmapKey
	IndexID  uint64
	Flags    Flags
	ImageIDs []uint64
}
