package search

import (
	"errors"

	"github.com/evanoberholster/imageIdx/bitmapidx"
	"github.com/evanoberholster/imageIdx/geoidx"
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

type bitmapClause struct {
	indexes []*bitmapidx.Index
}

func (bitmapClause) searchClause() {}

// AllBitmaps requires every supplied bitmap to contain an image.
func AllBitmaps(indexes ...*bitmapidx.Index) Clause {
	return bitmapClause{indexes: append([]*bitmapidx.Index(nil), indexes...)}
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
	index  *geoidx.Index
	ranges []geoidx.Range
}

func (geoClause) searchClause() {}

// WithinCells restricts images to the union of the supplied S2 cells. Cells
// may be at any valid level up to geoidx.GeoS2Level; coarser cells are
// converted to level-16 descendant ranges when the clause is created.
// Invalid cell IDs join ErrInvalidClause with the underlying cause.
func WithinCells(index *geoidx.Index, cellIDs ...uint64) (Clause, error) {
	if index == nil || len(cellIDs) == 0 {
		return nil, ErrInvalidClause
	}
	ranges, err := geoidx.RangesFromCellIDs(make([]geoidx.Range, 0, len(cellIDs)), cellIDs)
	if err != nil {
		return nil, errors.Join(ErrInvalidClause, err)
	}
	return geoClause{index: index, ranges: ranges}, nil
}
