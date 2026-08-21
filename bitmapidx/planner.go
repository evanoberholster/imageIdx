package bitmapidx

import "sort"

// PlanAND returns a stable copy ordered by Base cardinality. Delta sizes are
// intentionally absent from the comparison.
func PlanAND(indexes []*BitmapIndex) []*BitmapIndex {
	planned := append([]*BitmapIndex(nil), indexes...)
	sort.SliceStable(planned, func(a, b int) bool {
		if planned[a] == nil {
			return false
		}
		if planned[b] == nil {
			return true
		}
		return planned[a].view.Load().Cardinality < planned[b].view.Load().Cardinality
	})
	return planned
}
