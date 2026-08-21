// Package geoindex implements a fixed-level S2 candidate index with immutable
// sparse and roaring bitmap partitions.
package geoindex

import (
	"errors"
	"time"

	"github.com/evanoberholster/imageIdx/bitmapidx"
)

type ImageID = bitmapidx.ImageID
type S2Key uint64

const (
	GeoS2Level                     = 16
	GeoSparseSlots                 = 4
	GeoFormatVersion        uint16 = 1
	DefaultPublishThreshold        = 256
	DefaultCoverCells              = 32
)

var (
	ErrClosed            = errors.New("geoindex: index is closed")
	ErrCorruptManifest   = errors.New("geoindex: corrupt manifest")
	ErrCorruptStorage    = errors.New("geoindex: corrupt storage generation")
	ErrCorruptWAL        = errors.New("geoindex: corrupt write-ahead log")
	ErrDuplicateImageID  = errors.New("geoindex: duplicate image ID")
	ErrImageNotFound     = errors.New("geoindex: image ID is not indexed")
	ErrInvalidConfig     = errors.New("geoindex: invalid configuration")
	ErrInvalidCoordinate = errors.New("geoindex: invalid geographic coordinate")
	ErrInvalidImageID    = errors.New("geoindex: image ID zero is reserved")
	ErrInvalidS2Cell     = errors.New("geoindex: invalid S2 cell")
	ErrInvalidS2Level    = errors.New("geoindex: S2 cell must be level 16")
	ErrNotPersistent     = errors.New("geoindex: index is not persistent")
)

type Config struct {
	PublishThreshold int
	PublishInterval  time.Duration
	MaxCoverCells    int
	Epoch            *bitmapidx.Epoch
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
	if c.Epoch == nil {
		c.Epoch = bitmapidx.NewEpoch()
	}
	return c, nil
}

type GeoEntry struct {
	ImageID   uint64
	Latitude  float64
	Longitude float64
}

type GeoUpdate = GeoEntry

type GeoCellEntry struct {
	Key     S2Key
	ImageID uint64
}

type GeoOp uint8

const (
	GeoAdd GeoOp = iota + 1
	GeoRemove
)

type GeoCellMutation struct {
	Key     S2Key
	ImageID uint64
	Op      GeoOp
}

type CoordinateLookup func(ImageID) (latitude, longitude float64, ok bool)

type Durability uint8

const (
	DurabilityOS Durability = iota
	DurabilitySync
)

type GeoIndexHeader struct {
	Version         uint16
	Flags           uint16
	IndexID         uint64
	S2Level         uint8
	_               [7]byte
	SparseCellCount uint64
	BitmapCellCount uint64
	Generation      uint64
}
