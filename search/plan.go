package search

import (
	"sort"

	"github.com/evanoberholster/imageIdx/bitmapidx"
	"github.com/evanoberholster/imageIdx/geoindex"
	"github.com/evanoberholster/imageIdx/timestamp"
)

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

// Stage describes one compiled candidate stage.
type Stage struct {
	Kind     StageKind
	Estimate uint64
}

type executableStage interface {
	metadata() Stage
	collect([]ImageID) []ImageID
}

type filteringStage interface {
	executableStage
	// canFilter reports whether filter can verify an existing candidate set
	// without re-collecting the stage's own candidates.
	canFilter() bool
	filter(dst, candidates []ImageID) []ImageID
}

type bitmapStage struct {
	indexes  []*bitmapidx.Index
	estimate uint64
}

func (stage *bitmapStage) metadata() Stage {
	return Stage{Kind: StageBitmap, Estimate: stage.estimate}
}

func (stage *bitmapStage) collect(dst []ImageID) []ImageID {
	if len(stage.indexes) == 0 {
		return dst
	}
	smallest := stage.indexes[0]
	for _, index := range stage.indexes[1:] {
		if index.Metadata().BaseCardinality < smallest.Metadata().BaseCardinality {
			smallest = index
		}
	}
	start := len(dst)
	dst = smallest.AppendTo(dst)
	candidates := dst[start:]
	filtered := bitmapidx.FilterCandidates(candidates[:0], candidates, stage.indexes...)
	return dst[:start+len(filtered)]
}

func (stage *bitmapStage) canFilter() bool { return true }

func (stage *bitmapStage) filter(dst, candidates []ImageID) []ImageID {
	return bitmapidx.FilterCandidates(dst, candidates, stage.indexes...)
}

type timestampStage struct {
	index    *timestamp.Index
	start    uint64
	end      uint64
	exact    timestamp.ExactTimestampFunc
	estimate uint64
}

func (stage timestampStage) metadata() Stage {
	return Stage{Kind: StageTimestamp, Estimate: stage.estimate}
}

func (stage timestampStage) collect(dst []ImageID) []ImageID {
	if stage.exact != nil {
		return stage.index.SearchExact(dst, stage.start, stage.end, stage.exact)
	}
	return stage.index.Search(dst, stage.start, stage.end)
}

func (stage timestampStage) canFilter() bool { return stage.exact != nil }

func (stage timestampStage) filter(dst, candidates []ImageID) []ImageID {
	for _, id := range candidates {
		value, ok := stage.exact(id)
		if ok && value >= stage.start && value < stage.end {
			dst = append(dst, id)
		}
	}
	return dst
}

type geoStage struct {
	index    *geoindex.Index
	ranges   []geoindex.Range
	estimate uint64
}

func (stage geoStage) metadata() Stage {
	return Stage{Kind: StageGeo, Estimate: stage.estimate}
}

func (stage geoStage) collect(dst []ImageID) []ImageID {
	return stage.index.SearchRanges(dst, stage.ranges)
}

// Plan is a reusable, cardinality-ordered cross-index query.
type Plan struct {
	stages []executableStage
}

// Compile validates clauses, combines bitmap restrictions, and orders the
// resulting stages by their current cardinality estimates.
func Compile(clauses ...Clause) (Plan, error) {
	if len(clauses) == 0 {
		return Plan{}, ErrEmptyQuery
	}

	stages := make([]executableStage, 0, len(clauses))
	var bitmaps *bitmapStage
	for _, clause := range clauses {
		switch value := clause.(type) {
		case bitmapClause:
			if len(value.indexes) == 0 {
				return Plan{}, ErrInvalidClause
			}
			for _, index := range value.indexes {
				if index == nil {
					return Plan{}, ErrInvalidClause
				}
			}
			if bitmaps == nil {
				bitmaps = &bitmapStage{}
				stages = append(stages, bitmaps)
			}
			bitmaps.indexes = append(bitmaps.indexes, value.indexes...)
		case timestampClause:
			if value.index == nil {
				return Plan{}, ErrInvalidClause
			}
			stages = append(stages, timestampStage{
				index: value.index, start: value.start, end: value.end, exact: value.exact,
				estimate: value.index.EstimateRange(value.start, value.end),
			})
		case geoClause:
			if value.index == nil || len(value.ranges) == 0 {
				return Plan{}, ErrInvalidClause
			}
			stages = append(stages, geoStage{
				index: value.index, ranges: value.ranges,
				estimate: value.index.EstimateRanges(value.ranges),
			})
		default:
			return Plan{}, ErrInvalidClause
		}
	}

	if bitmaps != nil {
		bitmaps.estimate = ^uint64(0)
		for _, index := range bitmaps.indexes {
			if cardinality := index.Metadata().BaseCardinality; cardinality < bitmaps.estimate {
				bitmaps.estimate = cardinality
			}
		}
	}
	sort.SliceStable(stages, func(left, right int) bool {
		return stages[left].metadata().Estimate < stages[right].metadata().Estimate
	})
	return Plan{stages: stages}, nil
}

// Stages returns a copy of plan metadata in execution order.
func (plan Plan) Stages() []Stage {
	result := make([]Stage, len(plan.stages))
	for index, stage := range plan.stages {
		result[index] = stage.metadata()
	}
	return result
}
