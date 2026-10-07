package search

import (
	"context"
	"slices"
	"time"
)

// SearchOnce compiles clauses and immediately appends their sorted,
// unique matches to dst. It is named for its single-use cost model:
// compiling per execution repeats canonicalization, hashing, and
// cardinality estimation, so Compile a Plan directly when executing a
// query more than once.
func SearchOnce(ctx context.Context, dst []ImageID, clauses ...Clause) ([]ImageID, error) {
	plan, err := Compile(clauses...)
	if err != nil {
		return dst, err
	}
	return plan.AppendIDs(ctx, dst, nil)
}

// Count returns the number of sorted, unique matches without retaining them.
func (plan Plan) Count(ctx context.Context, workspace *Workspace) (uint64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if plan.IsEmpty() {
		return 0, nil
	}
	// Single-namespace plans can count directly without materializing IDs.
	if len(plan.stages) == 1 && plan.stages[0].kind == StageNamespace {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		stage := &plan.stages[0]
		if stage.namespaceKind == namespaceRange {
			return stage.namespaceIndex.CountRange(stage.namespaceRange)
		}
		return stage.namespaceQuery.Count()
	}
	bitmapOnly := true
	for _, st := range plan.stages {
		if st.kind == StageTimestamp {
			bitmapOnly = false
			break
		}
	}
	if bitmapOnly {
		return plan.countBitmaps(ctx, workspace)
	}
	var count uint64
	err := plan.walk(ctx, workspace, 0, executionBatch, func(ids []ImageID) bool { count += uint64(len(ids)); return true })
	if err != nil {
		return 0, err
	}
	return count, nil
}

// searchInto keeps the full-result fast path: materialize the smallest source
// once, then filter its sorted IDs in place. walk uses bounded batches for
// pages, visitors and ranking; both paths retain the same stage semantics.
func (plan Plan) searchInto(ctx context.Context, dst []ImageID, workspace *Workspace, metrics *SearchMetrics) ([]ImageID, error) {
	if plan.IsEmpty() {
		return dst, nil
	}
	var scratch Workspace
	ws := workspace
	if ws == nil {
		ws = &scratch
	}
	ws.scratch = ws.scratch[:0]
	needsScratch := false
	for _, st := range plan.stages[1:] {
		if st.kind == StageGeo || !st.canFilter() {
			needsScratch = true
			break
		}
	}
	// Pre-grow scratch from the plan estimate when the caller under-sized it.
	if estimate := plan.Estimate(); needsScratch && estimate > 0 && uint64(cap(ws.scratch)) < estimate {
		need := estimate
		if need > 1<<20 {
			need = 1 << 20
		}
		if uint64(cap(ws.scratch)) < need {
			grown := make([]ImageID, 0, need)
			ws.scratch = grown
		}
	}

	if err := ctx.Err(); err != nil {
		return dst, err
	}
	start := len(dst)
	var err error
	var stageStart time.Time
	if metrics != nil {
		metrics.reach(0, StageModeSource)
		stageStart = time.Now()
	}
	dst, err = collectStage(dst, ws, &plan.stages[0])
	if metrics != nil {
		stage := &metrics.Stages[0]
		stage.Duration += time.Since(stageStart)
		stage.Collected = uint64(len(dst) - start)
		stage.Passed = stage.Collected
	}
	if err != nil {
		return dst, err
	}
	if len(dst) == start {
		return dst, nil
	}

	// Filter-first: apply all in-place filters (namespace, exact-timestamp)
	// before any collect+intersect stage (lossy timestamp, geo).
	for index := 1; index < len(plan.stages); index++ {
		if err := ctx.Err(); err != nil {
			return dst, err
		}
		st := &plan.stages[index]
		if !st.canFilter() {
			continue
		}
		candidates := dst[start:]
		if metrics != nil {
			stage := metrics.reach(index, StageModeFilter)
			stage.Input = uint64(len(candidates))
			stageStart = time.Now()
		}
		filtered, err := st.filter(candidates[:0], candidates, ws.scratch, ws)
		if metrics != nil {
			stage := &metrics.Stages[index]
			stage.Duration += time.Since(stageStart)
			stage.Passed = uint64(len(filtered))
		}
		if err != nil {
			return dst, err
		}
		dst = dst[:start+len(filtered)]
		if len(dst) == start {
			return dst, nil
		}
	}
	for index := 1; index < len(plan.stages); index++ {
		if err := ctx.Err(); err != nil {
			return dst, err
		}
		st := &plan.stages[index]
		if st.canFilter() {
			continue
		}
		if metrics != nil {
			stage := metrics.reach(index, StageModeCollectIntersect)
			stage.Input = uint64(len(dst) - start)
			stageStart = time.Now()
		}
		ws.scratch, err = collectStage(ws.scratch[:0], ws, st)
		if metrics != nil {
			stage := &metrics.Stages[index]
			stage.Collected = uint64(len(ws.scratch))
		}
		if err != nil {
			if metrics != nil {
				metrics.Stages[index].Duration += time.Since(stageStart)
			}
			return dst, err
		}
		dst = intersectSortedSuffix(dst, start, ws.scratch)
		if metrics != nil {
			stage := &metrics.Stages[index]
			stage.Duration += time.Since(stageStart)
			stage.Passed = uint64(len(dst) - start)
		}
		if len(dst) == start {
			return dst, nil
		}
	}
	return dst, nil
}

// collectStage appends one stage's candidates to dst. Bitmap-backed namespace
// and geo collects are already ascending and unique, so only timestamp scans
// need sorting.
func collectStage(dst []ImageID, ws *Workspace, st *stage) ([]ImageID, error) {
	switch st.kind {
	case StageGeo:
		return st.geoIndex.SearchRanges(dst, st.geoRanges), nil
	case StageTimestamp:
		start := len(dst)
		var err error
		dst, err = st.collect(dst)
		if err != nil {
			return dst, err
		}
		return dst[:start+len(sortUnique(dst[start:]))], nil
	default:
		return st.collect(dst)
	}
}

func sortUnique(values []ImageID) []ImageID {
	slices.Sort(values)
	return slices.Compact(values)
}

func intersectSortedSuffix(dst []ImageID, start int, right []ImageID) []ImageID {
	left := dst[start:]
	leftIndex, rightIndex, output := 0, 0, start
	for leftIndex < len(left) && rightIndex < len(right) {
		switch {
		case left[leftIndex] < right[rightIndex]:
			leftIndex++
		case left[leftIndex] > right[rightIndex]:
			rightIndex++
		default:
			dst[output] = left[leftIndex]
			output++
			leftIndex++
			rightIndex++
		}
	}
	return dst[:output]
}
