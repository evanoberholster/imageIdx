package search

import (
	"errors"

	"github.com/evanoberholster/imageIdx/bitmapidx"

	"github.com/evanoberholster/imageIdx/timeidx"
)

var (
	ErrEmptyQuery    = errors.New("search: query has no clauses")
	ErrInvalidClause = errors.New("search: clause contains no usable index")
)

// Clause is one conjunctive search restriction. Clause implementations are
// created by this package so Compile can validate and optimize them together.
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

// FieldValues requires every supplied value to be present in the field.
func FieldValues(index *bitmapidx.FieldIndex, values ...bitmapidx.Value) Clause {
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
}

func (geoClause) searchClause() {}

// WithinCells restricts images to the union of the supplied S2 cells. The
// index must be a KindGeo field. Coarser cells are converted to level-16
// descendant ranges when the clause is created; finer cells snap to their
// level-16 ancestor. Invalid cell IDs join ErrInvalidClause with the
// underlying cause.
func WithinCells(index *bitmapidx.FieldIndex, cellIDs ...uint64) (Clause, error) {
	if index == nil || len(cellIDs) == 0 || index.Field().Kind != bitmapidx.KindGeo {
		return nil, ErrInvalidClause
	}
	ranges, err := bitmapidx.RangesFromCellIDs(make([]bitmapidx.GeoRange, 0, len(cellIDs)), cellIDs)
	if err != nil {
		return nil, errors.Join(ErrInvalidClause, err)
	}
	return geoClause{index: index, ranges: ranges}, nil
}
