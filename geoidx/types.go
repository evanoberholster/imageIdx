// Package geoidx implements a fixed-level S2 candidate index with immutable
// sparse and roaring bitmap partitions.
package geoidx

import (
	"errors"
	"time"

	imageidx "github.com/evanoberholster/imageIdx"
)

type ImageID = imageidx.ImageID
type S2Key uint64

const (
	GeoS2Level                     = 16
	DefaultPublishThreshold        = 256
	DefaultCoverCells              = 32
	geoFormatVersion        uint16 = 1
	geoSparseSlots                 = 4
)

var (
	ErrClosed             = errors.New("geoidx: index is closed")
	ErrCorruptManifest    = errors.New("geoidx: corrupt manifest")
	ErrCorruptStorage     = errors.New("geoidx: corrupt storage generation")
	ErrCorruptWAL         = errors.New("geoidx: corrupt write-ahead log")
	ErrDuplicateImageID   = errors.New("geoidx: duplicate image ID")
	ErrInvalidConfig      = errors.New("geoidx: invalid configuration")
	ErrInvalidCoordinate  = errors.New("geoidx: invalid geographic coordinate")
	ErrInvalidImageID     = errors.New("geoidx: image ID zero is reserved")
	ErrInvalidMutation    = errors.New("geoidx: mutation does not match published or pending state")
	ErrInvalidS2Cell      = errors.New("geoidx: invalid S2 cell")
	ErrInvalidS2Level     = errors.New("geoidx: unsupported S2 cell level")
	ErrNotPersistent      = errors.New("geoidx: index is not persistent")
	ErrCoverWorkspaceFull = errors.New("geoidx: cover workspace capacity is insufficient")
	ErrCoverBufferFull    = errors.New("geoidx: cover destination capacity is insufficient")
	ErrUnsupportedEndian  = errors.New("geoidx: mmap format requires a little-endian host")
)

type Config struct {
	PublishThreshold int
	PublishInterval  time.Duration
	MaxCoverCells    int
}

func (c Config) withDefaults() (Config, error) {
	if c.PublishThreshold < 0 || c.PublishInterval < 0 || c.MaxCoverCells < 0 {
		return Config{}, ErrInvalidConfig
	}
	if c.PublishThreshold == 0 {
		c.PublishThreshold = DefaultPublishThreshold
	}
	if c.PublishInterval == 0 {
		c.PublishInterval = 25 * time.Millisecond
	}
	if c.MaxCoverCells == 0 {
		c.MaxCoverCells = DefaultCoverCells
	}
	if c.PublishThreshold < 1 || c.MaxCoverCells < 1 {
		return Config{}, ErrInvalidConfig
	}
	return c, nil
}

// Entry is one canonical coordinate to index.
type Entry struct {
	ImageID   ImageID
	Latitude  float64
	Longitude float64
}

// Spec describes an index's stable identity and initial coordinates.
type Spec struct {
	IndexID uint64
	Flags   uint32
	Entries []Entry
}

// Mutation moves an image between fixed-level S2 cells. S2Key zero means the
// image was absent (From) or is being removed (To). Supplying both cells makes
// coordinate moves explicit and avoids a database-sized reverse-location map.
type Mutation struct {
	ImageID ImageID
	From    S2Key
	To      S2Key
}

// Metadata is a copied summary of the current immutable generation.
type Metadata struct {
	IndexID         uint64
	Flags           uint32
	Generation      uint64
	SparseCellCount uint64
	DenseCellCount  uint64
}

type CoordinateLookup func(ImageID) (latitude, longitude float64, ok bool)

type Durability = imageidx.Durability

const (
	DurabilityOS   = imageidx.DurabilityOS
	DurabilitySync = imageidx.DurabilitySync
)
