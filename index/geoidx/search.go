package geoidx

import (
	"cmp"
	"math"
	"slices"
	"sort"

	common "github.com/evanoberholster/imageIdx/common"
	"github.com/golang/geo/s2"
)

// Result is one distance-aware geographic match. Distance is the approximate
// great-circle distance in meters from the query location to the matched cell's
// center, rounded to the nearest meter. Coordinates are not stored, so this is
// exact only at cell resolution (about 20 m). Distance is uint32, which holds the
// full 0..20,015 km range.
type Result struct {
	ImageID  common.ImageID
	Distance uint32
}

// scanRanges visits every live (key, imageID) whose key falls in ranges, in
// ascending key order with additions last. ranges must be normalized. fn
// returning false stops the scan.
func scanRanges(v *view, ranges []Range, fn func(S2Key, common.ImageID) bool) bool {
	if len(ranges) == 0 {
		return true
	}
	hasMutations := len(v.muts) != 0
	position := 0
	for _, r := range ranges {
		position += sort.Search(len(v.keys)-position, func(k int) bool {
			return v.keys[position+k] >= r.Start
		})
		for position < len(v.keys) && v.keys[position] <= r.End {
			id := v.ids[position]
			if id != 0 {
				if !hasMutations {
					if !fn(v.keys[position], id) {
						return false
					}
				} else if _, mutated := mutationIndex(v.muts, id); !mutated {
					if !fn(v.keys[position], id) {
						return false
					}
				}
			}
			position++
		}
	}
	if hasMutations {
		for n := range v.muts {
			m := &v.muts[n]
			if m.add && keyInRanges(ranges, m.key) {
				if !fn(m.key, m.id) {
					return false
				}
			}
		}
	}
	return true
}

// keyInRanges reports whether key falls inside normalized ranges.
func keyInRanges(ranges []Range, key S2Key) bool {
	index := sort.Search(len(ranges), func(i int) bool { return ranges[i].End >= key })
	return index < len(ranges) && ranges[index].Start <= key
}

// normalizeResult normalizes arbitrary caller ranges into a possibly
// stack-backed slice. The result aliases stack when it fits.
func normalizeResult(stack *[DefaultMaxCells]Range, ranges []Range) []Range {
	if len(ranges) <= len(stack) {
		return normalizeRanges(append(stack[:0], ranges...), 0)
	}
	return normalizeRanges(append([]Range(nil), ranges...), 0)
}

// appendBaseRanges appends the base ImageIDs covered by normalized ranges using
// a bulk copy per range. The base has no mutations, so no per-entry filtering is
// needed.
func appendBaseRanges(dst []common.ImageID, v *view, ranges []Range) []common.ImageID {
	for _, r := range ranges {
		low := sort.Search(len(v.keys), func(i int) bool { return v.keys[i] >= r.Start })
		high := sort.Search(len(v.keys), func(i int) bool { return v.keys[i] > r.End })
		if high > low {
			dst = append(dst, v.ids[low:high]...)
		}
	}
	return dst
}

// SearchRanges appends the ImageIDs whose key falls in ranges, in ascending
// ImageID order. ranges need not be sorted; they are normalized first. The
// result is a superset of the exact cells by construction (a cover may include
// boundary cells).
func (i *Index) SearchRanges(dst []common.ImageID, ranges []Range) []common.ImageID {
	i.withView(func(v *view) {
		var stack [DefaultMaxCells]Range
		normalized := normalizeResult(&stack, ranges)
		if len(normalized) == 0 {
			return
		}
		mark := len(dst)
		if len(v.muts) == 0 {
			// Fast path: no delta, so a range is a contiguous base run.
			dst = appendBaseRanges(dst, v, normalized)
		} else {
			scanRanges(v, normalized, func(_ S2Key, id common.ImageID) bool {
				dst = append(dst, id)
				return true
			})
		}
		tail := dst[mark:]
		// Repeated keys leave the IDs already ascending, so avoid re-sorting a
		// large ordered run. A base-only scan yields unique IDs, so dedup is
		// only needed when a mutation delta is present.
		if !slices.IsSorted(tail) {
			slices.Sort(tail)
		}
		if len(v.muts) != 0 {
			dst = compactSorted(dst, mark)
		}
	})
	return dst
}

// SearchCell appends the ImageIDs stored in cellID's cell.
func (i *Index) SearchCell(dst []common.ImageID, cellID uint64) ([]common.ImageID, error) {
	key, err := KeyFromCellID(cellID)
	if err != nil {
		return dst, err
	}
	return i.SearchRanges(dst, []Range{{Start: key, End: key}}), nil
}

// SearchCover appends the ImageIDs covered by region.
func (i *Index) SearchCover(dst []common.ImageID, region s2.Region, options ...CoverOptions) []common.ImageID {
	var stack [DefaultMaxCells]Range
	ranges := CoverRegion(stack[:0], region, options...)
	return i.SearchRanges(dst, ranges)
}

// SearchRect appends the ImageIDs covered by a latitude/longitude rectangle.
func (i *Index) SearchRect(dst []common.ImageID, rect s2.Rect, options ...CoverOptions) []common.ImageID {
	return i.SearchCover(dst, rect, options...)
}

// SearchPolygon appends the ImageIDs covered by polygon.
func (i *Index) SearchPolygon(dst []common.ImageID, polygon *s2.Polygon, options ...CoverOptions) []common.ImageID {
	return i.SearchCover(dst, polygon, options...)
}

// SearchRadius appends the ImageIDs covered by the cap of the given surface
// radius around a coordinate. The cover is a superset of the cap, so boundary
// cells may contribute images slightly beyond radiusMeters.
func (i *Index) SearchRadius(dst []common.ImageID, latitude, longitude, radiusMeters float64, options ...CoverOptions) ([]common.ImageID, error) {
	center, err := validLatLng(latitude, longitude)
	if err != nil {
		return dst, err
	}
	if radiusMeters < 0 {
		return dst, ErrInvalidCoordinate
	}
	var stack [DefaultMaxCells]Range
	ranges := CoverCap(stack[:0], CapFromMeters(center, radiusMeters), options...)
	return i.SearchRanges(dst, ranges), nil
}

// SearchRadiusResults appends distance-aware matches within radiusMeters of a
// coordinate. Results are in ascending ImageID order; Distance is measured from
// the query to each image's cell center.
func (i *Index) SearchRadiusResults(dst []Result, latitude, longitude, radiusMeters float64, options ...CoverOptions) ([]Result, error) {
	center, err := validLatLng(latitude, longitude)
	if err != nil {
		return dst, err
	}
	if radiusMeters < 0 {
		return dst, ErrInvalidCoordinate
	}
	i.withView(func(v *view) {
		var stack [DefaultMaxCells]Range
		ranges := normalizeResult(&stack, CoverCap(nil, CapFromMeters(center, radiusMeters), options...))
		mark := len(dst)
		scanRanges(v, ranges, func(key S2Key, id common.ImageID) bool {
			dst = append(dst, Result{ImageID: id, Distance: metersToUint32(keyDistanceMeters(key, center))})
			return true
		})
		slices.SortFunc(dst[mark:], compareResults)
	})
	return dst, nil
}

// Nearest appends the k nearest images to a coordinate. S2 cell/range expansion
// grows a cover until it holds at least k images; because the cover is a
// superset of the enclosing cap and every returned distance is within the cap
// radius, the top-k is exact at cell resolution. Ties break on ImageID.
func (i *Index) Nearest(dst []Result, latitude, longitude float64, k int) ([]Result, error) {
	if k <= 0 {
		return dst, nil
	}
	center, err := validLatLng(latitude, longitude)
	if err != nil {
		return dst, err
	}
	i.withView(func(v *view) {
		radius := nearestInitialMeters
		for round := 0; round < nearestMaxRounds; round++ {
			mark := len(dst)
			var stack [DefaultMaxCells]Range
			ranges := normalizeResult(&stack, CoverCap(nil, CapFromMeters(center, radius), CoverOptions{MaxCells: i.config.MaxCells}))
			collectDistances(v, ranges, center, &dst)
			if len(dst)-mark >= k {
				slices.SortFunc(dst[mark:], compareResults)
				dst = dst[:mark+k]
				return
			}
			dst = dst[:mark]
			if radius >= math.Pi*earthRadiusMeters {
				break
			}
			radius *= 4
		}
		mark := len(dst)
		full := []Range{{Start: 0, End: S2Key(^uint64(0))}}
		collectDistances(v, full, center, &dst)
		slices.SortFunc(dst[mark:], compareResults)
		if len(dst)-mark > k {
			dst = dst[:mark+k]
		}
	})
	return dst, nil
}

const (
	nearestInitialMeters = 64.0
	nearestMaxRounds     = 24
)

func collectDistances(v *view, ranges []Range, center s2.LatLng, dst *[]Result) {
	scanRanges(v, ranges, func(key S2Key, id common.ImageID) bool {
		*dst = append(*dst, Result{ImageID: id, Distance: metersToUint32(keyDistanceMeters(key, center))})
		return true
	})
}

func compareResults(a, b Result) int {
	if order := cmp.Compare(a.Distance, b.Distance); order != 0 {
		return order
	}
	return cmp.Compare(a.ImageID, b.ImageID)
}

func metersToUint32(meters float64) uint32 {
	if meters <= 0 {
		return 0
	}
	if meters >= math.MaxUint32 {
		return math.MaxUint32
	}
	return uint32(meters + 0.5)
}

// CountRanges returns the number of images whose key falls in ranges.
func (i *Index) CountRanges(ranges []Range) uint64 {
	count := uint64(0)
	i.withView(func(v *view) {
		var stack [DefaultMaxCells]Range
		normalized := normalizeResult(&stack, ranges)
		scanRanges(v, normalized, func(_ S2Key, _ common.ImageID) bool {
			count++
			return true
		})
	})
	return count
}

// EstimateRanges returns a cheap upper estimate of the images in ranges, used by
// the search planner to order stages. It counts base keys in range plus live
// additions without materializing ImageIDs.
func (i *Index) EstimateRanges(ranges []Range) uint64 {
	estimate := uint64(0)
	i.withView(func(v *view) {
		var stack [DefaultMaxCells]Range
		normalized := normalizeResult(&stack, ranges)
		for _, r := range normalized {
			low := sort.Search(len(v.keys), func(k int) bool { return v.keys[k] >= r.Start })
			high := sort.Search(len(v.keys), func(k int) bool { return v.keys[k] > r.End })
			if high > low {
				estimate += uint64(high - low)
			}
		}
		for n := range v.muts {
			if v.muts[n].add && keyInRanges(normalized, v.muts[n].key) {
				estimate++
			}
		}
	})
	return estimate
}

// Contains reports whether id is stored at key.
func (i *Index) Contains(key S2Key, id common.ImageID) bool {
	found := false
	i.withView(func(v *view) {
		if index, ok := mutationIndex(v.muts, id); ok {
			found = v.muts[index].add && v.muts[index].key == key
			return
		}
		low := sort.Search(len(v.keys), func(k int) bool { return v.keys[k] >= key })
		for low < len(v.keys) && v.keys[low] == key {
			if v.ids[low] == id {
				found = true
				return
			}
			low++
		}
	})
	return found
}

// compactSorted removes duplicate ImageIDs from dst[mark:], which must be
// sorted.
func compactSorted(dst []common.ImageID, mark int) []common.ImageID {
	if len(dst)-mark < 2 {
		return dst
	}
	out := mark + 1
	for n := mark + 1; n < len(dst); n++ {
		if dst[n] != dst[out-1] {
			dst[out] = dst[n]
			out++
		}
	}
	return dst[:out]
}

func validLatLng(latitude, longitude float64) (s2.LatLng, error) {
	if math.IsNaN(latitude) || math.IsNaN(longitude) ||
		math.IsInf(latitude, 0) || math.IsInf(longitude, 0) ||
		latitude < -90 || latitude > 90 || longitude < -180 || longitude > 180 {
		return s2.LatLng{}, ErrInvalidCoordinate
	}
	return s2.LatLngFromDegrees(latitude, longitude), nil
}
