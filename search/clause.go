package search

import (
	"errors"

	"github.com/evanoberholster/imageIdx/index/bitmapidx"

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

type fieldClauseKind uint8

const (
	fieldValues fieldClauseKind = iota + 1
	fieldText
	fieldRange
)

type fieldClause struct {
	index  *bitmapidx.FieldIndex
	kind   fieldClauseKind
	values []bitmapidx.Value
	text   string
	bounds bitmapidx.NumericRange
}

func (fieldClause) searchClause() {}

// AllValues requires every supplied value to be present in the field.
// Compile rejects an empty value list. Canonical bytes are copied at Compile;
// callers must not mutate Value.Bytes until compilation completes.
func AllValues(index *bitmapidx.FieldIndex, values ...bitmapidx.Value) Clause {
	return fieldClause{index: index, kind: fieldValues, values: append([]bitmapidx.Value(nil), values...)}
}

// FieldText requires every analyzed token in text to be present.
func FieldText(index *bitmapidx.FieldIndex, text string) Clause {
	return fieldClause{index: index, kind: fieldText, text: text}
}

// FieldRange restricts a continuous field to bounds.
func FieldRange(index *bitmapidx.FieldIndex, bounds bitmapidx.NumericRange) Clause {
	return fieldClause{index: index, kind: fieldRange, bounds: bounds}
}

type aclClause struct {
	userID   uint64
	groupIDs []uint64
}

func (aclClause) searchClause() {}

// WithACL restricts results to images granted to userID or any supplied
// group ID. Group membership must be authenticated by the caller. Compile
// this clause with CompileWithStore so it can resolve the dedicated ACL fields.
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
	index  *bitmapidx.FieldIndex
	ranges []bitmapidx.GeoRange
	cells  []uint64
}

func (geoClause) searchClause() {}

// InCells restricts to the union of S2 cells. Like other constructors, it
// defers validation/normalization to Compile and copies the input slice.
func InCells(index *bitmapidx.FieldIndex, cellIDs ...uint64) Clause {
	return geoClause{index: index, cells: append([]uint64(nil), cellIDs...)}
}
