package search

import (
	"errors"

	"github.com/evanoberholster/imageIdx/bitmapidx"
	"github.com/evanoberholster/imageIdx/geoindex"
	"github.com/evanoberholster/imageIdx/timestamp"
)

type ImageID = bitmapidx.ImageID

var (
	ErrEmptyQuery   = errors.New("search: query has no predicates")
	ErrInvalidQuery = errors.New("search: query contains a nil index")
)

// TimestampRange selects compressed timestamp candidates in [Start, End).
// Exact optionally verifies lossy boundary candidates against canonical data.
type TimestampRange struct {
	Index *timestamp.Index
	Start uint64
	End   uint64
	Exact timestamp.ExactTimestampFunc
}

// GeoRange selects candidates in normalized, inclusive S2 key ranges.
type GeoRange struct {
	Index  *geoindex.GeoIndex
	Ranges []geoindex.S2KeyRange
}

// Query ANDs every supplied predicate. Bitmaps are treated as one conjunctive
// group so bitmapidx can retain its cardinality planner and roaring fast path.
type Query struct {
	Bitmaps         []*bitmapidx.BitmapIndex
	TimestampRanges []TimestampRange
	GeoRanges       []GeoRange
}

type StageKind uint8

const (
	StageBitmap StageKind = iota + 1
	StageTimestamp
	StageGeo
)

func (kind StageKind) String() string {
	switch kind {
	case StageBitmap:
		return "bitmap"
	case StageTimestamp:
		return "timestamp"
	case StageGeo:
		return "geo"
	default:
		return "unknown"
	}
}

// Stage describes one scheduled candidate stage. Ordinal identifies the
// predicate in its Query slice; the combined bitmap group always uses zero.
type Stage struct {
	Kind     StageKind
	Ordinal  int
	Estimate uint64
}

// Workspace retains temporary candidate buffers between searches. A
// Workspace is reusable but must not be used by concurrent searches.
type Workspace struct {
	ids          []ImageID
	timestampIDs []timestamp.ImageID
}

func NewWorkspace(capacity int) *Workspace {
	if capacity < 0 {
		capacity = 0
	}
	return &Workspace{
		ids:          make([]ImageID, 0, capacity),
		timestampIDs: make([]timestamp.ImageID, 0, capacity),
	}
}

// Reset releases references while retaining allocated buffer capacity.
func (workspace *Workspace) Reset() {
	if workspace == nil {
		return
	}
	workspace.ids = workspace.ids[:0]
	workspace.timestampIDs = workspace.timestampIDs[:0]
}
