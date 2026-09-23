package search

import (
	"cmp"
	"context"
	"errors"
	"math"
	"slices"
	"time"

	imageidx "github.com/evanoberholster/imageIdx"
	"github.com/evanoberholster/imageIdx/index/bitmapidx"
	"github.com/evanoberholster/imageIdx/index/timeidx"
)

// RankedResult preserves the ranker's native score precision. Lower scores
// sort first; equal scores break ties by ascending ImageID.
type RankedResult[S uint64 | float64] struct {
	ImageID imageidx.ImageID
	Score   S
}

// Result carries exact timestamp gaps or the integer score of a ranked source.
type Result = RankedResult[uint64]

// GeoResult carries haversine distance in metres, including fractional metres.
type GeoResult = RankedResult[float64]

// RankTime appends the plan's matches ordered by ascending
// |timestamp - target|, keeping the best k. A negative k ranks every match; a
// zero k returns dst unchanged. lookup supplies canonical timestamps; images
// without one are skipped. Workspace reuse bounds candidate scratch growth;
// index materialization may still allocate.
func (plan Plan) RankTime(ctx context.Context, dst []Result, workspace *Workspace, lookup timeidx.ExactTimestampFunc, target uint64, k int) ([]Result, error) {
	return plan.rankTime(ctx, dst, workspace, lookup, target, k, nil)
}

// RankTimeWithMetrics records filter stages plus scoring, heap and sort time.
func (plan Plan) RankTimeWithMetrics(ctx context.Context, dst []Result, workspace *Workspace, lookup timeidx.ExactTimestampFunc, target uint64, k int, metrics *SearchMetrics) ([]Result, error) {
	if metrics == nil {
		return dst, ErrInvalidOptions
	}
	return plan.rankTime(ctx, dst, workspace, lookup, target, k, metrics)
}

func (plan Plan) rankTime(ctx context.Context, dst []Result, workspace *Workspace, lookup timeidx.ExactTimestampFunc, target uint64, k int, metrics *SearchMetrics) ([]Result, error) {
	if metrics != nil {
		started := time.Now()
		metrics.reset(plan, OperationRankTime)
		defer func() { metrics.TotalDuration = time.Since(started) }()
	}
	if lookup == nil {
		return dst, ErrInvalidOptions
	}
	return rankByScore(ctx, dst, plan, workspace, k, metrics, func(id ImageID) (uint64, bool) {
		stamp, ok := lookup(id)
		if !ok {
			return 0, false
		}
		if stamp >= target {
			return stamp - target, true
		}
		return target - stamp, true
	})
}

// RankGeo appends the plan's matches ordered by ascending haversine distance
// in metres from the origin, keeping the best k. A negative k ranks every
// match; a zero k returns dst unchanged. Invalid origins and nil lookups
// return errors; images without valid coordinates are skipped.
func (plan Plan) RankGeo(ctx context.Context, dst []GeoResult, workspace *Workspace, latitude, longitude float64, lookup bitmapidx.GeoCoordinateLookup, k int) ([]GeoResult, error) {
	return plan.rankGeo(ctx, dst, workspace, latitude, longitude, lookup, k, nil)
}

// RankGeoWithMetrics records filter stages plus scoring, heap and sort time.
func (plan Plan) RankGeoWithMetrics(ctx context.Context, dst []GeoResult, workspace *Workspace, latitude, longitude float64, lookup bitmapidx.GeoCoordinateLookup, k int, metrics *SearchMetrics) ([]GeoResult, error) {
	if metrics == nil {
		return dst, ErrInvalidOptions
	}
	return plan.rankGeo(ctx, dst, workspace, latitude, longitude, lookup, k, metrics)
}

func (plan Plan) rankGeo(ctx context.Context, dst []GeoResult, workspace *Workspace, latitude, longitude float64, lookup bitmapidx.GeoCoordinateLookup, k int, metrics *SearchMetrics) ([]GeoResult, error) {
	if metrics != nil {
		started := time.Now()
		metrics.reset(plan, OperationRankGeo)
		defer func() { metrics.TotalDuration = time.Since(started) }()
	}
	if lookup == nil {
		return dst, ErrInvalidOptions
	}
	if _, err := bitmapidx.KeyFromLatLng(latitude, longitude); err != nil {
		return dst, errors.Join(ErrInvalidOptions, err)
	}
	return rankByScore(ctx, dst, plan, workspace, k, metrics, func(id ImageID) (float64, bool) {
		lat, lng, ok := lookup(id)
		if !ok {
			return 0, false
		}
		if _, err := bitmapidx.KeyFromLatLng(lat, lng); err != nil {
			return 0, false
		}
		if lat == latitude && lng == longitude {
			return 0, true
		}
		meters := bitmapidx.DistanceMeters(latitude, longitude, lat, lng)
		if math.IsNaN(meters) || math.IsInf(meters, 0) || meters < 0 {
			// Invalid distances cannot participate in an ordered result.
			return 0, false
		}
		return meters, true
	})
}

// rankByScore keeps the best k scored candidates in a bounded max-heap built
// directly on dst, then sorts them ascending by (Score, ImageID). It returns
// dst with the ranked prefix appended. A negative k ranks every candidate; a
// zero k returns dst unchanged. On cancellation no partial ranked suffix is
// returned. Only the top-k heap and bounded candidate batches are retained.
func rankByScore[S uint64 | float64](ctx context.Context, dst []RankedResult[S], plan Plan, ws *Workspace, k int, metrics *SearchMetrics, score func(ImageID) (S, bool)) ([]RankedResult[S], error) {
	if metrics != nil {
		metrics.Rank.K = k
	}
	if k == 0 {
		if metrics != nil && ctx.Err() == nil {
			metrics.Complete = true
		}
		return dst, ctx.Err()
	}
	base := len(dst)
	visit := func(ids []ImageID) bool {
		var started time.Time
		if metrics != nil {
			started = time.Now()
		}
		for _, id := range ids {
			if err := ctx.Err(); err != nil {
				if metrics != nil {
					metrics.Rank.ScoreDuration += time.Since(started)
				}
				return false
			}
			if metrics != nil {
				metrics.Rank.Candidates++
			}
			value, ok := score(id)
			if !ok {
				if metrics != nil {
					metrics.Rank.Skipped++
				}
				continue
			}
			if metrics != nil {
				metrics.Rank.Scored++
			}
			result := RankedResult[S]{ImageID: id, Score: value}
			size := len(dst) - base
			if k < 0 || size < k {
				dst = append(dst, result)
				siftWorstUp(dst[base:], size)
				continue
			}
			heap := dst[base:]
			if worseResult(heap[0], result) {
				heap[0] = result
				siftWorstDown(heap, 0)
			}
		}
		if metrics != nil {
			metrics.Rank.ScoreDuration += time.Since(started)
		}
		return true
	}
	var err error
	if metrics != nil {
		err = plan.walkWithMetrics(ctx, ws, 0, executionBatch, visit, metrics)
		metrics.Matched = metrics.Rank.Candidates
	} else {
		err = plan.walk(ctx, ws, 0, executionBatch, visit)
	}
	if err != nil {
		return dst[:base], err
	}
	var sortStart time.Time
	if metrics != nil {
		sortStart = time.Now()
	}
	slices.SortFunc(dst[base:], func(left, right RankedResult[S]) int {
		if left.Score != right.Score {
			return cmp.Compare(left.Score, right.Score)
		}
		return cmp.Compare(left.ImageID, right.ImageID)
	})
	if metrics != nil {
		metrics.Rank.SortDuration = time.Since(sortStart)
		metrics.Returned = uint64(len(dst) - base)
		metrics.Complete = true
	}
	return dst, nil
}

// worseResult reports whether a should be evicted before b: higher score
// first, then larger ImageID.
func worseResult[S uint64 | float64](a, b RankedResult[S]) bool {
	if a.Score != b.Score {
		return a.Score > b.Score
	}
	return a.ImageID > b.ImageID
}

func siftWorstUp[S uint64 | float64](heap []RankedResult[S], index int) {
	for index > 0 {
		parent := (index - 1) / 2
		if !worseResult(heap[index], heap[parent]) {
			return
		}
		heap[index], heap[parent] = heap[parent], heap[index]
		index = parent
	}
}

func siftWorstDown[S uint64 | float64](heap []RankedResult[S], index int) {
	for {
		left, right := 2*index+1, 2*index+2
		worst := index
		if left < len(heap) && worseResult(heap[left], heap[worst]) {
			worst = left
		}
		if right < len(heap) && worseResult(heap[right], heap[worst]) {
			worst = right
		}
		if worst == index {
			return
		}
		heap[index], heap[worst] = heap[worst], heap[index]
		index = worst
	}
}

// FilterTime appends the candidates whose exact timestamp is in [start, end).
// It is the time analogue of a range filter for ranked candidate lists and
// allocates nothing beyond dst growth. A nil lookup or empty range appends
// nothing. There is deliberately no Context form: it touches no index,
// only the caller slice, so its cost is bounded by len(candidates) and a
// caller needing cancellation can check around it.
func FilterTime(dst, candidates []ImageID, lookup timeidx.ExactTimestampFunc, start, end uint64) []ImageID {
	if lookup == nil || start >= end {
		return dst
	}
	for _, id := range candidates {
		if stamp, ok := lookup(id); ok && stamp >= start && stamp < end {
			dst = append(dst, id)
		}
	}
	return dst
}

// FilterGeo appends the candidates inside the union of the supplied S2 cells,
// using the same cell expansion as InCells. It is the geo analogue of
// FilterTime for ranked candidate lists. A nil index or empty cell list
// appends nothing; invalid cells join ErrInvalidClause. Like FilterTime it
// has no Context form: cell expansion is the only index-independent work
// and the probe itself is bounded by the candidate list.
func FilterGeo(dst, candidates []ImageID, index *bitmapidx.FieldIndex, cellIDs ...uint64) ([]ImageID, error) {
	if index == nil || len(cellIDs) == 0 {
		return dst, nil
	}
	if index.Field().Kind != bitmapidx.KindGeo {
		return dst, ErrInvalidClause
	}
	ranges, err := bitmapidx.RangesFromCellIDs(nil, cellIDs)
	if err != nil {
		return dst, errors.Join(ErrInvalidClause, err)
	}
	return index.FilterGeoRanges(dst, candidates, ranges)
}

// FilterRanked appends the ranked entries that satisfy the plan to dst as
// Result values, preserving the source's order and score. It is the bridge for
// ANN results: pass vectoridx.Result entries with their ImageID and integer
// score. Entries whose imageID reports ok=false are skipped.
//
// Only supplied candidates are probed (lossy timestamps may buffer a range).
// Because the plan is applied
// after ranking, a selective plan can under-fill; over-fetch from the ranked
// source, or filter during ANN traversal with a CandidateFilter instead.
func (plan Plan) FilterRanked[T any](ctx context.Context, dst []Result, workspace *Workspace, ranked []T, imageID func(T) (imageidx.ImageID, bool), score func(T) uint64) ([]Result, error) {
	if len(ranked) == 0 || plan.IsEmpty() {
		return dst, ctx.Err()
	}
	if imageID == nil || score == nil {
		return dst, ErrInvalidClause
	}
	var scratch Workspace
	ws := workspace
	if ws == nil {
		ws = &scratch
	}
	start := len(dst)
	if err := plan.prepareProbes(ctx, ws, 0, nil); err != nil {
		return dst, err
	}
	// Keep the original entry positions: duplicate IDs can carry different scores.
	var positions [executionBatch]int
	var ids [executionBatch]ImageID
	for offset := 0; offset < len(ranked); offset += executionBatch {
		count := 0
		for n := offset; n < min(offset+executionBatch, len(ranked)); n++ {
			id, ok := imageID(ranked[n])
			if !ok {
				continue
			}
			positions[count], ids[count] = n, id
			count++
		}
		ws.batch = append(ws.batch[:0], ids[:count]...)
		matches, err := plan.probe(ctx, ws.batch, ws, 0, nil)
		if err != nil {
			return dst[:start], err
		}
		read := 0
		for _, id := range matches {
			for ids[read] != id {
				read++
			}
			dst = append(dst, Result{ImageID: id, Score: score(ranked[positions[read]])})
			read++
		}
	}
	if err := ctx.Err(); err != nil {
		return dst[:start], err
	}
	return dst, nil
}
