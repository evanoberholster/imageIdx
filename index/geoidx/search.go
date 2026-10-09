package geoidx

import (
	"cmp"
	"math"
	"slices"
	"sort"

	common "github.com/evanoberholster/imageIdx/common"
	"github.com/evanoberholster/imageIdx/roar"
	"github.com/golang/geo/s2"
)

// Result is one distance-aware geographic match. Distance is the great-circle
// distance in meters from the query location to the matched cell's center,
// rounded to the nearest meter. geoidx stores only S2 keys (no coordinates), so
// distance is exact at S2Level cell resolution (about 20 m). uint32 holds the
// full 0..20,015 km range.
type Result struct {
	ImageID  common.ImageID
	Distance uint32
}

// scanRanges is the fundamental geographic scan: it visits every live
// (S2Key, ImageID) whose key falls in ranges, in native ascending-key order,
// with pending additions last. It imposes no ImageID ordering. ranges must be
// normalized. fn returning false stops the scan.
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

// sortSuffix ascending-sorts dst[mark:] by ImageID, skipping the sort when the
// native key order already left it ordered.
func sortSuffix(dst []common.ImageID, mark int, maxID common.ImageID, dedup bool) []common.ImageID {
	tail := dst[mark:]
	if !slices.IsSorted(tail) {
		sortIDs(tail, maxID)
	}
	if dedup {
		dst = compactSorted(dst, mark)
	}
	return dst
}

// SearchRanges appends the ImageIDs whose key falls in ranges, in ascending
// ImageID order. ranges need not be sorted; they are normalized first. This is
// the initial-query path: ordering is required by the search merge/intersection
// paths, so a sort of the appended suffix is acceptable.
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
		dst = sortSuffix(dst, mark, v.maxID, len(v.muts) != 0)
	})
	return dst
}

// SearchCell appends the ImageIDs stored in cellID's cell, ascending.
func (i *Index) SearchCell(dst []common.ImageID, cellID uint64) ([]common.ImageID, error) {
	key, err := KeyFromCellID(cellID)
	if err != nil {
		return dst, err
	}
	return i.SearchRanges(dst, []Range{{Start: key, End: key}}), nil
}

// SearchCover appends the ImageIDs covered by region (a cell-superset: cover
// cells that merely intersect the region are included).
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

// SearchRadius appends the ImageIDs whose cell center is within radiusMeters of
// a coordinate, in ascending ImageID order. Distance is measured from the query
// to the authoritative S2Level cell center, so the result is exact at cell
// resolution rather than a raw cover superset.
func (i *Index) SearchRadius(dst []common.ImageID, latitude, longitude, radiusMeters float64, options ...CoverOptions) ([]common.ImageID, error) {
	center, err := validLatLng(latitude, longitude)
	if err != nil {
		return dst, err
	}
	if radiusMeters < 0 {
		return dst, ErrInvalidCoordinate
	}
	i.withView(func(v *view) {
		var stack [DefaultMaxCells]Range
		ranges := CoverCap(stack[:0], CapFromMeters(center, radiusMeters), options...)
		mark := len(dst)
		scanRanges(v, ranges, func(key S2Key, id common.ImageID) bool {
			if keyDistanceMeters(key, center) <= radiusMeters {
				dst = append(dst, id)
			}
			return true
		})
		dst = sortSuffix(dst, mark, v.maxID, len(v.muts) != 0)
	})
	return dst, nil
}

// SearchRadiusResults appends distance-aware matches whose cell center is within
// radiusMeters of a coordinate. Results are in ascending ImageID order; the
// cover is only a candidate superset and every returned match passes the exact
// cell-center distance test.
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
		ranges := CoverCap(stack[:0], CapFromMeters(center, radiusMeters), options...)
		mark := len(dst)
		scanRanges(v, ranges, func(key S2Key, id common.ImageID) bool {
			if d := keyDistanceMeters(key, center); d <= radiusMeters {
				dst = append(dst, Result{ImageID: id, Distance: metersToUint32(d)})
			}
			return true
		})
		slices.SortFunc(dst[mark:], compareResults)
	})
	return dst, nil
}

// Nearest appends the k nearest images to a coordinate, ascending by cell-center
// distance then ImageID. It grows an S2 cap cover until the k-th distance is
// within the covered radius, which proves no unvisited cell can hold a closer
// result (the cover contains every cell whose center is inside the cap).
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
			ranges := CoverCap(stack[:0], CapFromMeters(center, radius), CoverOptions{MaxCells: i.config.MaxCells})
			scanRanges(v, ranges, func(key S2Key, id common.ImageID) bool {
				dst = append(dst, Result{ImageID: id, Distance: metersToUint32(keyDistanceMeters(key, center))})
				return true
			})
			if len(dst)-mark >= k {
				slices.SortFunc(dst[mark:], compareResults)
				// Rounded k-th distance overestimates the exact k-th by at most
				// 0.5 m; if it is within the covered radius the top-k is exact.
				if float64(dst[mark+k-1].Distance)+0.5 <= radius {
					dst = dst[:mark+k]
					return
				}
			}
			dst = dst[:mark]
			if radius >= math.Pi*earthRadiusMeters {
				break
			}
			radius *= 4
		}
		// Fallback: cover the whole sphere and take the top-k.
		mark := len(dst)
		full := []Range{{Start: 0, End: S2Key(^uint64(0))}}
		scanRanges(v, full, func(key S2Key, id common.ImageID) bool {
			dst = append(dst, Result{ImageID: id, Distance: metersToUint32(keyDistanceMeters(key, center))})
			return true
		})
		slices.SortFunc(dst[mark:], compareResults)
		if len(dst)-mark > k {
			dst = dst[:k+mark]
		}
	})
	return dst, nil
}

const (
	nearestInitialMeters = 64.0
	nearestMaxRounds     = 24
	collectChunk         = 256
)

// collector accumulates ImageIDs for a bitmap sink, sorting the batch with the
// radix path before a single AddMany so the roaring build sees sorted input.
type collector struct {
	dst   *roar.MutableBitmap
	maxID common.ImageID
	buf   []common.ImageID
	stack [collectChunk]common.ImageID
}

func newCollector(dst *roar.MutableBitmap, maxID common.ImageID) *collector {
	c := &collector{dst: dst, maxID: maxID}
	c.buf = c.stack[:0]
	return c
}

func (c *collector) add(id common.ImageID) { c.buf = append(c.buf, id) }

func (c *collector) flush() {
	if len(c.buf) == 0 {
		return
	}
	if !slices.IsSorted(c.buf) {
		sortIDs(c.buf, c.maxID)
	}
	c.dst.AddMany(c.buf)
	c.buf = c.stack[:0]
}

// CollectRanges adds every ImageID whose key falls in ranges directly to dst in
// native S2 order. No sorted slice is built and no global sort runs.
func (i *Index) CollectRanges(dst *roar.MutableBitmap, ranges []Range) {
	if dst == nil {
		return
	}
	i.withView(func(v *view) {
		var stack [DefaultMaxCells]Range
		normalized := normalizeResult(&stack, ranges)
		c := newCollector(dst, v.maxID)
		scanRanges(v, normalized, func(_ S2Key, id common.ImageID) bool {
			c.add(id)
			return true
		})
		c.flush()
	})
}

// CollectCover adds every ImageID covered by region to dst in native S2 order.
func (i *Index) CollectCover(dst *roar.MutableBitmap, region s2.Region, options ...CoverOptions) {
	var stack [DefaultMaxCells]Range
	ranges := CoverRegion(stack[:0], region, options...)
	i.CollectRanges(dst, ranges)
}

// CollectRect adds every ImageID covered by a rectangle to dst.
func (i *Index) CollectRect(dst *roar.MutableBitmap, rect s2.Rect, options ...CoverOptions) {
	i.CollectCover(dst, rect, options...)
}

// CollectPolygon adds every ImageID covered by polygon to dst.
func (i *Index) CollectPolygon(dst *roar.MutableBitmap, polygon *s2.Polygon, options ...CoverOptions) {
	i.CollectCover(dst, polygon, options...)
}

// CollectRadius adds every ImageID whose cell center is within radiusMeters to
// dst in native S2 order.
func (i *Index) CollectRadius(dst *roar.MutableBitmap, latitude, longitude, radiusMeters float64, options ...CoverOptions) error {
	center, err := validLatLng(latitude, longitude)
	if err != nil {
		return err
	}
	if radiusMeters < 0 {
		return ErrInvalidCoordinate
	}
	if dst == nil {
		return nil
	}
	i.withView(func(v *view) {
		var stack [DefaultMaxCells]Range
		ranges := CoverCap(stack[:0], CapFromMeters(center, radiusMeters), options...)
		c := newCollector(dst, v.maxID)
		scanRanges(v, ranges, func(key S2Key, id common.ImageID) bool {
			if keyDistanceMeters(key, center) <= radiusMeters {
				c.add(id)
			}
			return true
		})
		c.flush()
	})
	return nil
}

// CollectCell adds every ImageID stored in cellID's cell to dst.
func (i *Index) CollectCell(dst *roar.MutableBitmap, cellID uint64) error {
	key, err := KeyFromCellID(cellID)
	if err != nil {
		return err
	}
	i.CollectRanges(dst, []Range{{Start: key, End: key}})
	return nil
}

// FilterRanges adds the ImageIDs that are both covered by ranges and present in
// candidates directly to dst, in native S2 order. It never builds a sorted
// []ImageID and never globally sorts: geography filters an existing bitmap.
func (i *Index) FilterRanges(dst *roar.MutableBitmap, candidates roar.Bitmap, ranges []Range) {
	if dst == nil || candidates.IsEmpty() {
		return
	}
	i.withView(func(v *view) {
		var stack [DefaultMaxCells]Range
		normalized := normalizeResult(&stack, ranges)
		c := newCollector(dst, v.maxID)
		scanRanges(v, normalized, func(_ S2Key, id common.ImageID) bool {
			if candidates.Contains(id) {
				c.add(id)
			}
			return true
		})
		c.flush()
	})
}

// FilterCover adds the candidates covered by region to dst in native S2 order.
func (i *Index) FilterCover(dst *roar.MutableBitmap, candidates roar.Bitmap, region s2.Region, options ...CoverOptions) {
	var stack [DefaultMaxCells]Range
	ranges := CoverRegion(stack[:0], region, options...)
	i.FilterRanges(dst, candidates, ranges)
}

// FilterRect adds the candidates covered by a rectangle to dst.
func (i *Index) FilterRect(dst *roar.MutableBitmap, candidates roar.Bitmap, rect s2.Rect, options ...CoverOptions) {
	i.FilterCover(dst, candidates, rect, options...)
}

// FilterPolygon adds the candidates covered by polygon to dst.
func (i *Index) FilterPolygon(dst *roar.MutableBitmap, candidates roar.Bitmap, polygon *s2.Polygon, options ...CoverOptions) {
	i.FilterCover(dst, candidates, polygon, options...)
}

// FilterRadius adds the candidates whose cell center is within radiusMeters to
// dst in native S2 order.
func (i *Index) FilterRadius(dst *roar.MutableBitmap, candidates roar.Bitmap, latitude, longitude, radiusMeters float64, options ...CoverOptions) error {
	center, err := validLatLng(latitude, longitude)
	if err != nil {
		return err
	}
	if radiusMeters < 0 {
		return ErrInvalidCoordinate
	}
	if dst == nil || candidates.IsEmpty() {
		return nil
	}
	i.withView(func(v *view) {
		var stack [DefaultMaxCells]Range
		ranges := CoverCap(stack[:0], CapFromMeters(center, radiusMeters), options...)
		c := newCollector(dst, v.maxID)
		scanRanges(v, ranges, func(key S2Key, id common.ImageID) bool {
			if candidates.Contains(id) && keyDistanceMeters(key, center) <= radiusMeters {
				c.add(id)
			}
			return true
		})
		c.flush()
	})
	return nil
}

// FilterCell adds the candidates stored in cellID's cell to dst.
func (i *Index) FilterCell(dst *roar.MutableBitmap, candidates roar.Bitmap, cellID uint64) error {
	key, err := KeyFromCellID(cellID)
	if err != nil {
		return err
	}
	i.FilterRanges(dst, candidates, []Range{{Start: key, End: key}})
	return nil
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
