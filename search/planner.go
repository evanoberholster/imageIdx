package search

import (
	"sort"

	"github.com/evanoberholster/imageIdx/bitmapidx"
	"github.com/evanoberholster/imageIdx/geoindex"
)

// Scheduler builds stable, cardinality-ordered plans. It is stateless and safe
// for concurrent use.
type Scheduler struct{}

type plannedStage struct {
	Stage
	bitmaps   []*bitmapidx.BitmapIndex
	timestamp TimestampRange
	geo       GeoRange
}

type Plan struct {
	stages []plannedStage
}

func (Scheduler) Plan(query Query) (Plan, error) {
	stageCount := 0
	if len(query.Bitmaps) != 0 {
		stageCount++
	}
	stageCount += len(query.TimestampRanges) + len(query.GeoRanges)
	if stageCount == 0 {
		return Plan{}, ErrEmptyQuery
	}

	stages := make([]plannedStage, 0, stageCount)
	if len(query.Bitmaps) != 0 {
		bitmaps := append([]*bitmapidx.BitmapIndex(nil), query.Bitmaps...)
		estimate := ^uint64(0)
		for _, index := range bitmaps {
			if index == nil {
				return Plan{}, ErrInvalidQuery
			}
			if cardinality := index.BaseCardinality(); cardinality < estimate {
				estimate = cardinality
			}
		}
		stages = append(stages, plannedStage{
			Stage:   Stage{Kind: StageBitmap, Estimate: estimate},
			bitmaps: bitmaps,
		})
	}

	for ordinal, predicate := range query.TimestampRanges {
		if predicate.Index == nil {
			return Plan{}, ErrInvalidQuery
		}
		stages = append(stages, plannedStage{
			Stage:     Stage{Kind: StageTimestamp, Ordinal: ordinal, Estimate: predicate.Index.EstimateRange(predicate.Start, predicate.End)},
			timestamp: predicate,
		})
	}

	for ordinal, predicate := range query.GeoRanges {
		if predicate.Index == nil {
			return Plan{}, ErrInvalidQuery
		}
		predicate.Ranges = geoindex.NormalizeRanges(append([]geoindex.S2KeyRange(nil), predicate.Ranges...))
		stages = append(stages, plannedStage{
			Stage: Stage{Kind: StageGeo, Ordinal: ordinal, Estimate: predicate.Index.EstimateRanges(predicate.Ranges)},
			geo:   predicate,
		})
	}

	sort.SliceStable(stages, func(left, right int) bool {
		return stages[left].Estimate < stages[right].Estimate
	})
	return Plan{stages: stages}, nil
}

// Stages returns a copy of the public plan metadata in execution order.
func (plan Plan) Stages() []Stage {
	result := make([]Stage, len(plan.stages))
	for index := range plan.stages {
		result[index] = plan.stages[index].Stage
	}
	return result
}
