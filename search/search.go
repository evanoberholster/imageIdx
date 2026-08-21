package search

import (
	"slices"

	"github.com/evanoberholster/imageIdx/bitmapidx"
)

// Search plans query with the default Scheduler and appends sorted, unique
// matches to dst.
func Search(dst []ImageID, query Query) ([]ImageID, error) {
	return Scheduler{}.Search(dst, query, nil)
}

// Search plans and executes query, optionally reusing workspace buffers.
func (scheduler Scheduler) Search(dst []ImageID, query Query, workspace *Workspace) ([]ImageID, error) {
	plan, err := scheduler.Plan(query)
	if err != nil {
		return dst, err
	}
	return plan.Execute(dst, workspace), nil
}

// Execute appends the plan's sorted, unique intersection to dst. Estimates are
// planning hints only; execution always queries the current published views.
func (plan Plan) Execute(dst []ImageID, workspace *Workspace) []ImageID {
	if len(plan.stages) == 0 {
		return dst
	}
	if workspace == nil {
		workspace = &Workspace{}
	}
	start := len(dst)
	dst = appendStage(dst, &plan.stages[0], workspace)
	values := sortUnique(dst[start:])
	dst = dst[:start+len(values)]

	for stageIndex := 1; stageIndex < len(plan.stages) && len(dst) != start; stageIndex++ {
		stage := &plan.stages[stageIndex]
		if stage.Kind == StageBitmap {
			candidates := dst[start:]
			filtered := bitmapidx.FilterInto(candidates[:0], candidates, stage.bitmaps...)
			dst = dst[:start+len(filtered)]
			continue
		}

		workspace.ids = workspace.ids[:0]
		workspace.ids = appendStage(workspace.ids, stage, workspace)
		workspace.ids = sortUnique(workspace.ids)
		dst = intersectSortedSuffix(dst, start, workspace.ids)
	}
	return dst
}

func appendStage(dst []ImageID, stage *plannedStage, workspace *Workspace) []ImageID {
	switch stage.Kind {
	case StageBitmap:
		return bitmapidx.IntersectInto(dst, stage.bitmaps...)
	case StageTimestamp:
		workspace.timestampIDs = workspace.timestampIDs[:0]
		if stage.timestamp.Exact == nil {
			workspace.timestampIDs = stage.timestamp.Index.Search(
				workspace.timestampIDs, stage.timestamp.Start, stage.timestamp.End,
			)
		} else {
			workspace.timestampIDs = stage.timestamp.Index.SearchExact(
				workspace.timestampIDs, stage.timestamp.Start, stage.timestamp.End, stage.timestamp.Exact,
			)
		}
		for _, id := range workspace.timestampIDs {
			dst = append(dst, ImageID(id))
		}
		return dst
	case StageGeo:
		return stage.geo.Index.SearchRanges(dst, stage.geo.Ranges)
	default:
		return dst
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
