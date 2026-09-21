// Package vectoridx implements a persistent HNSW approximate-nearest-neighbor
// vector index with direct uint32 NodeIDs, fixed dimensions, and scalar L2
// distance kernels.
//
// This file defines the core domain types. See doc.go for the package
// overview, format notes, and concurrency model.
package vectoridx

import (
	"errors"

	imageidx "github.com/evanoberholster/imageIdx"
)

// NodeID is the identifier stored in HNSW edges and returned by search.
// Its semantic meaning comes from the index TypeID (ImageID, FaceID, or a
// future application identifier). VectorIdx never translates it.
type NodeID uint32

// TypeID identifies the vector space and NodeID semantics. It does not imply
// dimensions or element representation; those persist explicitly in the header.
type TypeID uint16

// ElementType selects the fixed per-index vector representation.
type ElementType uint8

const (
	ElementInvalid ElementType = iota
	ElementInt8
	ElementUint8
	ElementInt16
	ElementUint16
	ElementFloat32
)

// Supported fixed dimensions. An index has exactly one for its lifetime.
const (
	Dim512  = 512
	Dim768  = 768
	Dim1024 = 1024
)

// Metric identifiers persisted in the superblock. V1 supports L2 only.
const (
	MetricL2 uint8 = 1
)

// HNSW baseline defaults. These are benchmark starting points, not format
// constants; every field is configurable through Config.
const (
	DefaultM              = 16
	DefaultM0             = 32
	DefaultEfConstruction = 128
	DefaultEfSearch       = 64
)

// Config describes one vector space. The zero value is invalid: Dimensions
// and ElementType must be set explicitly. M/M0/Ef fields default when zero.
type Config struct {
	TypeID         TypeID
	Dimensions     uint16
	ElementType    ElementType
	M              uint16
	M0             uint16
	EfConstruction uint32
	EfSearch       uint32
}

func (c Config) withDefaults() (Config, error) {
	if c.Dimensions != Dim512 && c.Dimensions != Dim768 && c.Dimensions != Dim1024 {
		return Config{}, ErrInvalidConfig
	}
	switch c.ElementType {
	case ElementInt8, ElementUint8, ElementInt16, ElementUint16, ElementFloat32:
	default:
		return Config{}, ErrInvalidConfig
	}
	if c.M == 0 {
		c.M = DefaultM
	}
	if c.M0 == 0 {
		c.M0 = DefaultM0
	}
	if c.EfConstruction == 0 {
		c.EfConstruction = DefaultEfConstruction
	}
	if c.EfSearch == 0 {
		c.EfSearch = DefaultEfSearch
	}
	if c.M < 2 || c.M0 < 2 || c.M > 128 || c.M0 > 128 {
		return Config{}, ErrInvalidConfig
	}
	if c.EfConstruction < 1 || c.EfConstruction > 4096 {
		return Config{}, ErrInvalidConfig
	}
	if c.EfSearch < 1 || c.EfSearch > 4096 {
		return Config{}, ErrInvalidConfig
	}
	return c, nil
}

// ElementSize returns the byte width of one vector element.
func (c Config) ElementSize() int {
	switch c.ElementType {
	case ElementInt8, ElementUint8:
		return 1
	case ElementInt16, ElementUint16:
		return 2
	case ElementFloat32:
		return 4
	default:
		return 0
	}
}

// VectorBytes returns Dimensions*ElementSize for a validated Config.
func (c Config) VectorBytes() int { return int(c.Dimensions) * c.ElementSize() }

// Result is one nearest-neighbor match. Score is monotonic with squared L2
// distance: a smaller distance always yields a smaller-or-equal Score.
// See EncodeScore for the saturation rule at the uint32 boundary.
type Result struct {
	NodeID NodeID
	Score  uint32
}

// CandidateFilter restricts which live nodes may be returned. A nil filter
// accepts every live, non-tombstoned node. Filtering is applied after ANN
// discovery and never constrains graph traversal (VectorIdx.MD section 20).
//
// Adapt a bitmap index with a closure:
//
//	filter := vectoridx.CandidateFilter(func(id vectoridx.NodeID) bool {
//	    return bitmap.Contains(imageidx.ImageID(id))
//	})
type CandidateFilter func(NodeID) bool

// Durability controls when a mutation is acknowledged relative to WAL sync.
type Durability = imageidx.Durability

const (
	DurabilityOS   = imageidx.DurabilityOS
	DurabilitySync = imageidx.DurabilitySync
)

var (
	ErrInvalidConfig   = errors.New("vectoridx: invalid configuration")
	ErrInvalidNodeID   = errors.New("vectoridx: invalid node ID")
	ErrInvalidVector   = errors.New("vectoridx: vector dimension or type mismatch")
	ErrDuplicateNodeID = errors.New("vectoridx: duplicate live node ID")
	ErrNodeNotFound    = errors.New("vectoridx: node not found")
	ErrClosed          = errors.New("vectoridx: index is closed")
	ErrCorruptStorage  = errors.New("vectoridx: corrupt storage generation")
	ErrCorruptWAL      = errors.New("vectoridx: corrupt write-ahead log")
	ErrNotPersistent   = errors.New("vectoridx: index is not persistent")
	ErrInvalidQuery    = errors.New("vectoridx: invalid query")
	ErrUnsupportedHost = errors.New("vectoridx: storage format requires a little-endian host")
)
