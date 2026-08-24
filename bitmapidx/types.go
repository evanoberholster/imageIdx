// Package bitmapidx implements immutable, copy-on-write bitmap indexes over
// database-wide image identifiers (imageidx.ImageID).
//
// Each Index represents one logical predicate (for example a person, album,
// tag, or location value) as a set of ImageIDs. Readers observe one immutable
// snapshot loaded atomically; writers build replacement snapshots and publish
// them with a single atomic store, so readers never observe a partially
// applied batch and never acquire a lock.
//
// A snapshot is bounded to three layers: an immutable Base plus at most one
// Frozen and one Active delta of recent additions/removals. Membership is
// evaluated with Active taking precedence over Frozen over Base. Deltas are
// periodically folded into a replacement Base in the background.
//
// Reader lifetime tracking is internal. Standalone indexes own their tracker;
// persistent stores share one tracker across their child indexes.
package bitmapidx

import (
	"errors"
	"time"

	imageidx "github.com/evanoberholster/imageIdx"
)

// ImageID is the database-wide identifier type shared by all index packages.
type ImageID = imageidx.ImageID

// IndexType identifies a bitmap predicate category (location, person, album,
// and so on). The concrete registry is database-defined.
type IndexType uint16

const (
	IndexTypeLocation IndexType = iota + 1
	IndexTypePerson
	IndexTypeAlbum
	IndexTypeTag
	IndexTypeObject
	IndexTypeEvent
)

// Key identifies one logical bitmap predicate: an IndexType plus a value
// within that category (for example Location + ParisID).
type Key struct {
	Type    IndexType
	ValueID uint64
}

const (
	bitmapFormatVersion uint16 = 1

	// DefaultActiveTarget is the default Active-to-Frozen rotation threshold.
	DefaultActiveTarget = 2_048
	// DefaultHardDeltaLimit is the maximum default size of a visible delta.
	DefaultHardDeltaLimit = 4_095
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

// Config controls delta rotation and compaction. The zero value is valid;
// defaults are filled in by New.
type Config struct {
	// ActiveTarget is the Active delta size at which a write rotates the delta
	// to Frozen and schedules background compaction. It must stay below
	// HardDeltaLimit.
	ActiveTarget int
	// HardDeltaLimit bounds each reader-visible delta. A write that would
	// exceed it instead folds the deltas into a replacement Base, so a view
	// never grows past Base + Frozen + Active.
	HardDeltaLimit int
	// CompactDelay is primarily useful to tests and workload tuning. Zero
	// starts background compaction immediately.
	CompactDelay time.Duration
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
	return c, nil
}

// Mutation is one atomically published change. Remove wins when an ID appears
// in both slices, which makes retry/cancellation behavior deterministic.
type Mutation struct {
	Add    []ImageID
	Remove []ImageID
}

// Durability controls persistent WAL synchronization.
type Durability = imageidx.Durability

const (
	DurabilityOS   = imageidx.DurabilityOS
	DurabilitySync = imageidx.DurabilitySync
)

// Spec describes an index's stable identity and initial contents.
type Spec struct {
	Key     Key
	IndexID uint64
	Flags   uint32
	IDs     []ImageID
}

// Metadata is the immutable public portion of the current index snapshot. It
// is cheap to obtain because it reads only the atomically published view.
type Metadata struct {
	Key             Key
	IndexID         uint64
	Flags           uint32
	BaseCardinality uint64
	Generation      uint64
}
