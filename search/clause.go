package search

import (
	"errors"

	"github.com/evanoberholster/imageIdx/index/bitmapidx"
	"github.com/evanoberholster/imageIdx/index/geoidx"

	"github.com/evanoberholster/imageIdx/index/timeidx"
)

var (
	ErrEmptyQuery    = errors.New("search: query has no clauses")
	ErrInvalidClause = errors.New("search: invalid clause")
)

// Clause is one conjunctive search restriction. Clause implementations are
// created by this package so Compile can validate and optimize them together.
//
// Constructors capture their arguments; Compile validates nil indexes, empty
// value lists, kind mismatches and cells. Empty ranges are
// valid and match nothing; empty value lists are rejected.
type Clause interface {
	searchClause()
}

type namespaceClauseKind uint8

const (
	namespaceValues namespaceClauseKind = iota + 1
	namespaceText
	namespaceRange
)

type namespaceClause struct {
	index  *bitmapidx.Index
	kind   namespaceClauseKind
	values []bitmapidx.Value
	text   string
	bounds bitmapidx.NumericRange
}

func (namespaceClause) searchClause() {}

// AllValues requires every supplied value to be present in the namespace.
// Compile rejects an empty value list. Canonical bytes are copied at Compile;
// callers must not mutate Value.Bytes until compilation completes.
func AllValues(index *bitmapidx.Index, values ...bitmapidx.Value) Clause {
	return namespaceClause{index: index, kind: namespaceValues, values: append([]bitmapidx.Value(nil), values...)}
}

// NamespaceText requires every analyzed token in text to be present.
func NamespaceText(index *bitmapidx.Index, text string) Clause {
	return namespaceClause{index: index, kind: namespaceText, text: text}
}

// NamespaceRange restricts a continuous namespace to bounds.
func NamespaceRange(index *bitmapidx.Index, bounds bitmapidx.NumericRange) Clause {
	return namespaceClause{index: index, kind: namespaceRange, bounds: bounds}
}

type aclClause struct {
	userID   uint64
	groupIDs []uint64
}

func (aclClause) searchClause() {}

// WithACL restricts results to images granted to userID or any supplied
// group ID. Group membership must be authenticated by the caller. Compile
// this clause with CompileWithStore so it can resolve the dedicated ACL namespaces.
func WithACL(userID uint64, groupIDs ...uint64) Clause {
	return aclClause{userID: userID, groupIDs: append([]uint64(nil), groupIDs...)}
}

type timestampClause struct {
	index *timeidx.Index
	start uint64
	end   uint64
	exact timeidx.ExactTimestampFunc
}

func (timestampClause) searchClause() {}

// TimestampOption configures a timestamp clause.
type TimestampOption interface {
	applyTimestamp(*timestampClause)
}

type timestampOptionFunc func(*timestampClause)

func (option timestampOptionFunc) applyTimestamp(clause *timestampClause) {
	option(clause)
}

// ExactTimestamp verifies lossy timestamp candidates against canonical data.
func ExactTimestamp(lookup timeidx.ExactTimestampFunc) TimestampOption {
	return timestampOptionFunc(func(clause *timestampClause) {
		clause.exact = lookup
	})
}

// During restricts images to the half-open timestamp range [start, end).
// Bounds use timeidx's exact unsigned nanoseconds since 1900-01-01 UTC,
// the same encoding as the selected index and any ExactTimestamp lookup.
// An empty range (start >= end) is valid and compiles to an empty plan;
// a nil index is rejected at Compile time with ErrInvalidClause.
func During(index *timeidx.Index, start, end uint64, options ...TimestampOption) Clause {
	clause := timestampClause{index: index, start: start, end: end}
	for _, option := range options {
		if option != nil {
			option.applyTimestamp(&clause)
		}
	}
	return clause
}

type geoClause struct {
	index  *geoidx.Index
	ranges []geoidx.Range
	cells  []uint64
}

func (geoClause) searchClause() {}

// InCells restricts to the union of S2 cells against a geoidx index. Like other
// constructors, it defers validation/normalization to Compile and copies the
// input slice. Cell IDs may be at S2Level or coarser; coarser cells expand to
// their descendants.
//
// TODO(geoidx): Add city/area queries once a place-to-S2-region representation
// is defined. geoidx itself should remain independent of geocoding and
// place-name databases.
func InCells(index *geoidx.Index, cellIDs ...uint64) Clause {
	return geoClause{index: index, cells: append([]uint64(nil), cellIDs...)}
}
