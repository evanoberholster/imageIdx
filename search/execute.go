package search

import "slices"

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
	if len(plan.stages) == 0 {
		return dst
	}
	if workspace == nil {
		workspace = &Workspace{}
	}
	start := len(dst)
	dst = plan.stages[0].collect(dst)
	values := sortUnique(dst[start:])
	dst = dst[:start+len(values)]

	for stageIndex := 1; stageIndex < len(plan.stages) && len(dst) != start; stageIndex++ {
		stage := plan.stages[stageIndex]
		if filter, ok := stage.(filteringStage); ok && filter.canFilter() {
			candidates := dst[start:]
			filtered := filter.filter(candidates[:0], candidates)
			dst = dst[:start+len(filtered)]
			continue
		}

		workspace.scratch = workspace.scratch[:0]
		workspace.scratch = stage.collect(workspace.scratch)
		workspace.scratch = sortUnique(workspace.scratch)
		dst = intersectSortedSuffix(dst, start, workspace.scratch)
	}
	return dst
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
