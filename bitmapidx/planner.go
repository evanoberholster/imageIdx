package bitmapidx

import (
	"cmp"
	"slices"
)

// planIntersection returns a stable copy ordered by Base cardinality. Delta
// sizes are intentionally absent from the comparison.
func planIntersection(indexes []*Index) []*Index {
	return planIntersectionInto(nil, indexes)
}

func planIntersectionInto(dst, indexes []*Index) []*Index {
	planned := append(dst, indexes...)
	slices.SortStableFunc(planned, func(a, b *Index) int {
		switch {
		case a == nil && b == nil:
			return 0
		case a == nil:
			return 1
		case b == nil:
			return -1
		default:
			return cmp.Compare(a.view.Load().cardinality, b.view.Load().cardinality)
		}
	})
	return planned
}
