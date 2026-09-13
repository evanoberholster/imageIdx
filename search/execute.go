package search

import (
	"context"
	"iter"
	"slices"

	"github.com/evanoberholster/imageIdx/bitmapidx"
)

// Search compiles clauses and immediately appends their sorted, unique matches
// to dst. Compile a Plan directly when executing a query more than once.
func Search(dst []ImageID, clauses ...Clause) ([]ImageID, error) {
	plan, err := Compile(clauses...)
	if err != nil {
		return dst, err
	}
	return plan.Search(dst, nil), nil
}

// Search appends the plan's sorted, unique intersection to dst. Estimates are
// ordering hints only; every execution reads current published index views.
func (plan Plan) Search(dst []ImageID, workspace *Workspace) []ImageID {
	result, _ := plan.SearchContext(context.Background(), dst, workspace)
	return result
}

// SearchContext is Search with cancellation checked between stages.
func (plan Plan) SearchContext(ctx context.Context, dst []ImageID, workspace *Workspace) ([]ImageID, error) {
	return plan.searchInto(ctx, dst, workspace, -1)
}

// SearchLimit appends at most limit sorted, unique matches to dst.
// A negative limit means no limit; zero returns dst unchanged.
func (plan Plan) SearchLimit(dst []ImageID, workspace *Workspace, limit int) []ImageID {
	if limit == 0 || plan.IsEmpty() {
		return dst
	}
	result, _ := plan.searchInto(context.Background(), dst, workspace, limit)
	return result
}

// Count returns the number of sorted, unique matches without retaining them
// in the caller slice. It reuses workspace scratch.
func (plan Plan) Count(workspace *Workspace) int {
	count, _ := plan.CountContext(context.Background(), workspace)
	return count
}

// CountContext is Count with cancellation checked between stages.
func (plan Plan) CountContext(ctx context.Context, workspace *Workspace) (int, error) {
	if plan.IsEmpty() {
		return 0, nil
	}
	// Bitmap-only plans count the intersection without materializing IDs.
	if len(plan.stages) == 1 && plan.stages[0].kind == StageBitmap {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		return int(bitmapidx.Count(plan.stages[0].bitmaps...)), nil
	}
	// Materialize into a private dst so it never aliases workspace scratch,
	// which searchInto uses for per-stage collects.
	out, err := plan.searchInto(ctx, nil, workspace, -1)
	if err != nil {
		return 0, err
	}
	return len(out), nil
}

// Iterate yields sorted, unique matches. The result is materialized once on
// first iteration (indexes do not provide ID-ordered cursors: timestamp scans
// in time order, geo scans in shard order), then yielded without further
// allocation.
func (plan Plan) Iterate(workspace *Workspace) iter.Seq[ImageID] {
	return func(yield func(ImageID) bool) {
		if plan.IsEmpty() {
			return
		}
		var scratch Workspace
		ws := workspace
		if ws == nil {
			ws = &scratch
		}
		// Materialize into a privately owned slice so workspace reuse by the
		// caller cannot corrupt iteration.
		ids := plan.Search(nil, ws)
		for _, id := range ids {
			if !yield(id) {
				return
			}
		}
	}
}

func (plan Plan) searchInto(ctx context.Context, dst []ImageID, workspace *Workspace, limit int) ([]ImageID, error) {
	if plan.IsEmpty() {
		return dst, nil
	}
	if limit == 0 {
		return dst, nil
	}
	var scratch Workspace
	ws := workspace
	if ws == nil {
		ws = &scratch
	}
	ws.scratch = ws.scratch[:0]
	// Pre-grow scratch from the plan estimate when the caller under-sized it.
	if estimate := plan.Estimate(); estimate > 0 && uint64(cap(ws.scratch)) < estimate {
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
	dst = collectStage(dst, ws, &plan.stages[0])
	if len(dst) == start {
		return truncateLimit(dst, start, limit), nil
	}

	// Filter-first: apply all in-place filters (bitmap, exact-timestamp)
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
		filtered := st.filter(candidates[:0], candidates, ws.scratch, ws)
		dst = dst[:start+len(filtered)]
		if len(dst) == start {
			return truncateLimit(dst, start, limit), nil
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
		ws.scratch = collectStage(ws.scratch[:0], ws, st)
		dst = intersectSortedSuffix(dst, start, ws.scratch)
		if len(dst) == start {
			return truncateLimit(dst, start, limit), nil
		}
	}
	return truncateLimit(dst, start, limit), nil
}

func truncateLimit(dst []ImageID, start int, limit int) []ImageID {
	if limit < 0 || len(dst)-start <= limit {
		return dst
	}
	return dst[:start+limit]
}

// collectStage appends one stage's sorted, unique candidates to dst. Geo
// stages accumulate covered cells into the workspace-owned MutableBitmap and
// materialize once, skipping the collect-then-sort round trip; other stages
// collect ID lists and sort them.
func collectStage(dst []ImageID, ws *Workspace, st *stage) []ImageID {
	if st.kind == StageGeo && ws != nil {
		m := ws.mutableBitmap()
		m.Reset()
		st.geoIndex.CollectGeoRanges(m, st.geoRanges)
		return m.AppendTo(dst)
	}
	start := len(dst)
	dst = st.collect(dst)
	values := sortUnique(dst[start:])
	return dst[:start+len(values)]
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
