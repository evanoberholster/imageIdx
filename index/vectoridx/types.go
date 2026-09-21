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
	"github.com/evanoberholster/imageIdx/internal/indexutil"
)

// NodeID is the identifier stored in HNSW edges. It is a compact uint32
// widened to the database-wide identifier type in Result values. What the ID
// refers to (image, face, or application-defined) comes from the index's
// IndexType.
type NodeID uint32

// ImageID is the database-wide identifier type shared by all index packages.
type ImageID = imageidx.ImageID

// IndexType identifies the entity domain an index addresses (images, faces,
// or an application-defined domain), mirroring bitmapidx.IndexType naming.
// It is the high byte of SpaceID. Zero is reserved and never valid; the set
// of constants is closed.
type IndexType uint8

const (
	IndexTypeImage  IndexType = iota + 1 // database-wide image identifier
	IndexTypeFace                        // face identifier
	IndexTypeCustom                      // application-defined identifier
)

// Valid reports whether t is a recognized index type.
func (t IndexType) Valid() bool { return t >= IndexTypeImage && t <= IndexTypeCustom }

// SpaceID is the single identity of one vector space: the high byte is the
// IndexType and the low byte is the space number within that type. NodeIDs
// are stored as compact uint32 values and widened to the database-wide
// identifier in Result values.
type SpaceID uint16

// NewSpaceID packs an index type and a within-type space number.
func NewSpaceID(indexType IndexType, space uint8) SpaceID {
	return SpaceID(uint16(indexType)<<8 | uint16(space))
}

// Type returns the index type encoded in s.
func (s SpaceID) Type() IndexType { return IndexType(uint8(s >> 8)) }

// Space returns the within-type space number encoded in s.
func (s SpaceID) Space() uint8 { return uint8(s) }

// Valid reports whether s carries a recognized index type.
func (s SpaceID) Valid() bool { return s.Type().Valid() }

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

// Dimensions is a supported fixed vector width. The set is closed: an index
// has exactly one for its lifetime, persisted in the header.
type Dimensions uint16

const (
	Dim512  Dimensions = 512
	Dim768  Dimensions = 768
	Dim1024 Dimensions = 1024
)

// Valid reports whether d is a supported fixed width.
func (d Dimensions) Valid() bool {
	switch d {
	case Dim512, Dim768, Dim1024:
		return true
	}
	return false
}

// Metric identifiers persisted in the superblock.
const (
	MetricL2     uint8 = 1
	MetricCosine uint8 = 2
	MetricDot    uint8 = 3
)

// HNSW baseline defaults. These are benchmark starting points, not format
// constants; every field is configurable through Config.
const (
	DefaultM              = 16
	DefaultM0             = 32
	DefaultEfConstruction = 128
	DefaultEfSearch       = 64

	// Dynamic-ef defaults mirror Weaviate's dynamicEf behaviour:
	// ef = clamp(k*DynamicEfFactor, DynamicEfMin, DynamicEfMax).
	DefaultDynamicEfFactor = 8
	DefaultDynamicEfMin    = 16
	DefaultDynamicEfMax    = 512

	// DefaultFlatSearchCutoff mirrors Weaviate's flatSearchCutoff: when a
	// filter's estimated match count is at or below the cutoff, an exact
	// scan beats ANN traversal. Negative disables the flat fallback.
	DefaultFlatSearchCutoff = 4000
	// DefaultFlatThreshold is the index size at or below which exact search
	// replaces HNSW entirely (Weaviate flat/dynamic index analogue).
	DefaultFlatThreshold = 2000
	// DefaultOversampleFactor scales ef for filtered queries.
	DefaultOversampleFactor = 8
)

// Config describes one vector space. The zero value is invalid: SpaceID and
// Dimensions must identify a supported space and ElementType must be set
// explicitly. M/M0/Ef fields default when zero. Metric defaults to L2 when
// zero. Dynamic-ef is disabled unless DynamicEfFactor != 0; FlatSearchCutoff
// and FlatThreshold default when zero, negative FlatSearchCutoff forces ANN.
type Config struct {
	SpaceID        SpaceID
	Dimensions     Dimensions
	ElementType    ElementType
	Metric         uint8
	M              uint16
	M0             uint16
	EfConstruction uint32
	EfSearch       uint32
	// Dynamic ef: ef = clamp(k*Factor, Min, Max). Zero Factor disables.
	DynamicEfFactor uint32
	DynamicEfMin    uint32
	DynamicEfMax    uint32
	// FlatSearchCutoff bounds the estimated filter matches that justify an
	// exact scan. Negative forces ANN. Zero takes the default.
	FlatSearchCutoff int
	// FlatThreshold uses exact search when live count <= threshold.
	// Negative disables. Zero takes the default.
	FlatThreshold int
	// OversampleFactor scales ef for filtered queries. Zero takes default.
	OversampleFactor uint32
	// UseHeuristic selects diversity-based neighbor selection (hnswlib
	// heuristic) instead of closest-only. Default true unless explicitly
	// disabled via DisableHeuristic.
	UseHeuristic     bool
	DisableHeuristic bool
	// FilterAware keeps filter-passing nodes in the result heap while still
	// traversing through all nodes (ACORN-lite). Default true.
	FilterAware        bool
	DisableFilterAware bool
	// AutoCompactRatio triggers async Compact when tombstones/live exceeds
	// the ratio (0 disables). CompactMinNodes gates small indexes.
	AutoCompactRatio float64
	CompactMinNodes  int
	// AutoVacuumDeltas triggers async Vacuum when the manifest holds at
	// least this many delta generations (0 disables).
	AutoVacuumDeltas int
	// AutoVacuumFragmentation triggers async Vacuum when the fraction of
	// tombstone records across deltas reaches this value (0 disables).
	AutoVacuumFragmentation float64
}

func (c Config) withDefaults() (Config, error) {
	if !c.SpaceID.Valid() || !c.Dimensions.Valid() {
		return Config{}, ErrInvalidConfig
	}
	switch c.ElementType {
	case ElementInt8, ElementUint8, ElementInt16, ElementUint16, ElementFloat32:
	default:
		return Config{}, ErrInvalidConfig
	}
	if c.Metric == 0 {
		c.Metric = MetricL2
	}
	switch c.Metric {
	case MetricL2, MetricCosine, MetricDot:
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
	if c.DynamicEfFactor > 0 {
		if c.DynamicEfMin == 0 {
			c.DynamicEfMin = DefaultDynamicEfMin
		}
		if c.DynamicEfMax == 0 {
			c.DynamicEfMax = DefaultDynamicEfMax
		}
		if c.DynamicEfMin < 1 || c.DynamicEfMax < c.DynamicEfMin || c.DynamicEfMax > 4096 {
			return Config{}, ErrInvalidConfig
		}
	}
	if c.FlatSearchCutoff == 0 {
		c.FlatSearchCutoff = DefaultFlatSearchCutoff
	}
	if c.FlatThreshold == 0 {
		c.FlatThreshold = DefaultFlatThreshold
	}
	if c.OversampleFactor == 0 {
		c.OversampleFactor = DefaultOversampleFactor
	}
	if c.OversampleFactor < 1 || c.OversampleFactor > 64 {
		return Config{}, ErrInvalidConfig
	}
	if !c.DisableHeuristic {
		c.UseHeuristic = true
	} else {
		c.UseHeuristic = false
	}
	if !c.DisableFilterAware {
		c.FilterAware = true
	} else {
		c.FilterAware = false
	}
	if c.AutoCompactRatio < 0 || c.AutoCompactRatio > 1 {
		return Config{}, ErrInvalidConfig
	}
	if c.CompactMinNodes < 0 {
		return Config{}, ErrInvalidConfig
	}
	if c.AutoVacuumDeltas < 0 {
		return Config{}, ErrInvalidConfig
	}
	if c.AutoVacuumFragmentation < 0 || c.AutoVacuumFragmentation > 1 {
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

// Result is one nearest-neighbor match. ImageID is the stored NodeID widened
// to the database-wide identifier type; its semantics come from
// Config.SpaceID.Type(). Score is monotonic with the index metric distance:
// a smaller distance always yields a smaller-or-equal Score. For L2 it tracks
// squared L2; for cosine it tracks (1-cos)*2^30; for dot it tracks
// querySelfDot-dot (zero for self). See EncodeScore for the saturation rule.
type Result struct {
	ImageID ImageID
	Score   uint32
}

// widenedID converts a stored NodeID to the database-wide identifier type.
func widenedID(id NodeID) ImageID { return ImageID(id) }

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

// Durability controls persistent WAL synchronization. DurabilityOS returns
// once the frame is visible to the OS; DurabilitySync fsyncs before
// returning. It is a runtime policy passed to Create/Open, not part of the
// persisted format.
type Durability = imageidx.Durability

const (
	DurabilityOS   = imageidx.DurabilityOS
	DurabilitySync = imageidx.DurabilitySync
)

var (
	ErrInvalidConfig   = errors.New("vectoridx: invalid configuration")
	ErrInvalidVector   = errors.New("vectoridx: vector dimension or type mismatch")
	ErrDuplicateNodeID = errors.New("vectoridx: duplicate live node ID")
	ErrNodeNotFound    = errors.New("vectoridx: node not found")
	ErrClosed          = errors.New("vectoridx: index is closed")
	ErrCorruptStorage  = errors.New("vectoridx: corrupt storage generation")
	ErrCorruptWAL      = errors.New("vectoridx: corrupt write-ahead log")
	ErrNotPersistent   = errors.New("vectoridx: index is not persistent")
	ErrInvalidQuery    = errors.New("vectoridx: invalid query")
	ErrUnsupportedHost = errors.New("vectoridx: storage format requires a little-endian host")
	// ErrStoreBusy reports another handle holding the index lock. It
	// aliases indexutil.ErrBusy; see internal/indexutil.
	ErrStoreBusy = indexutil.ErrBusy
	// ErrCompactionInProgress reports a second Compact while one is running.
	ErrCompactionInProgress = errors.New("vectoridx: compaction already in progress")
)
