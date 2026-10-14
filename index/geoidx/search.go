package geoidx

import (
	"cmp"
	schema "github.com/evanoberholster/imageIdx/schema"
	"github.com/evanoberholster/imageIdx/roar"
	"github.com/golang/geo/s2"
	"math"
	"math/bits"
	"slices"
	"sort"
	"sync"
)

// Result is one distance-aware geographic match. Distance is the great-circle
// distance in meters from the query location to the matched cell's center,
// rounded to the nearest meter. geoidx stores only S2 keys (no coordinates), so
// distance is exact at S2Level cell resolution (about 20 m). uint32 holds the
// full 0..20,015 km range.
type Result struct {
	ImageID  schema.ImageID
	Distance uint32
}

// viewTouched reports the effective state of id in the active, layered, then
// frozen mutation state (active is newest). touched is false when no layer
// names id.
func viewTouched(v *view, id schema.ImageID) (present, touched bool) {
	if index, ok := mutationIndex(v.active, id); ok {
		return v.active[index].add, true
	}
	for n := len(v.layers) - 1; n >= 0; n-- {
		if index, ok := mutationIndex(v.layers[n], id); ok {
			return v.layers[n][index].add, true
		}
	}
	if index, ok := mutationIndex(v.frozen, id); ok {
		return v.frozen[index].add, true
	}
	return false, false
}

// scanRanges is the fundamental geographic scan: it visits every live
// (S2Key, ImageID) whose key falls in ranges, in native ascending-key order,
// with pending additions last. It imposes no ImageID ordering. ranges must be
// normalized. fn returning false stops the scan.
func scanRanges(v *view, ranges []Range, fn func(S2Key, schema.ImageID) bool) bool {
	useDelta := viewDelta(v) != nil
	if !scanShards(v.shards, ranges, useDelta, v, fn) {
		return false
	}
	if useDelta {
		// Additions come from the cached key-sorted insert list, so only the
		// adds inside the ranges are visited.
		adds := v.deltaAdds
		for _, r := range ranges {
			low := sort.Search(len(adds), func(i int) bool { return adds[i].key >= r.Start })
			for low < len(adds) && adds[low].key <= r.End {
				if !fn(adds[low].key, adds[low].id) {
					return false
				}
				low++
			}
		}
	}
	return true
}

// scanShards walks the shard directory in global key order, visiting base
// entries inside ranges. When useDelta is set, base entries named by the delta
// are shadowed. The range cursor advances monotonically across shards, so a
// range that spans shards is continued rather than restarted. A repeated S2Key
// spanning shards is visited in every shard that holds part of its run.
func scanShards(shards []*shard, ranges []Range, useDelta bool, v *view, fn func(S2Key, schema.ImageID) bool) bool {
	if len(ranges) == 0 {
		return true
	}
	ri := 0
	for si := firstShardAtOrAfter(shards, ranges[0].Start); si < len(shards); si++ {
		s := shards[si]
		if len(s.keys) == 0 {
			continue
		}
		firstKey := s.keys[0]
		for ri < len(ranges) && ranges[ri].End < firstKey {
			ri++
		}
		if ri == len(ranges) {
			return true
		}
		lastKey := s.keys[len(s.keys)-1]
		for j := ri; j < len(ranges) && ranges[j].Start <= lastKey; j++ {
			r := ranges[j]
			low := sort.Search(len(s.keys), func(k int) bool { return s.keys[k] >= r.Start })
			for low < len(s.keys) && s.keys[low] <= r.End {
				id := s.ids[low]
				if id != 0 && (!useDelta || !deltaTouched(v, id)) {
					if !fn(s.keys[low], id) {
						return false
					}
				}
				low++
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
// a bulk copy per shard-range overlap. Callers use it only when no mutation
// layers exist.
func appendBaseRanges(dst []schema.ImageID, v *view, ranges []Range) []schema.ImageID {
	if len(ranges) == 0 {
		return dst
	}
	ri := 0
	for si := firstShardAtOrAfter(v.shards, ranges[0].Start); si < len(v.shards); si++ {
		s := v.shards[si]
		if len(s.keys) == 0 {
			continue
		}
		firstKey := s.keys[0]
		for ri < len(ranges) && ranges[ri].End < firstKey {
			ri++
		}
		if ri == len(ranges) {
			break
		}
		lastKey := s.keys[len(s.keys)-1]
		for j := ri; j < len(ranges) && ranges[j].Start <= lastKey; j++ {
			r := ranges[j]
			low := sort.Search(len(s.keys), func(k int) bool { return s.keys[k] >= r.Start })
			high := sort.Search(len(s.keys), func(k int) bool { return s.keys[k] > r.End })
			if high > low {
				dst = append(dst, s.ids[low:high]...)
			}
		}
	}
	return dst
}

// sortSuffix sorts dst[mark:] ascending by ImageID and removes duplicates,
// returning the (possibly shortened) dst. It skips the sort and any allocation
// when the native scan already produced ordered output.
func sortSuffix(dst []schema.ImageID, mark int, maxID schema.ImageID) []schema.ImageID {
	tail := dst[mark:]
	if len(tail) == 0 {
		return dst
	}
	if slices.IsSorted(tail) {
		tail = slices.Compact(tail)
	} else {
		tail = sortIDs(tail, maxID)
	}
	return dst[:mark+len(tail)]
}

// SearchRanges appends the ImageIDs whose key falls in ranges, in ascending
// ImageID order. ranges need not be sorted; they are normalized first. This is
// the initial-query path: ordering is required by the search merge/intersection
// paths, so a sort of the appended suffix is acceptable.
func (i *Index) SearchRanges(dst []schema.ImageID, ranges []Range) []schema.ImageID {
	i.withView(func(v *view) {
		var stack [DefaultMaxCells]Range
		normalized := normalizeResult(&stack, ranges)
		if len(normalized) == 0 {
			return
		}
		mark := len(dst)
		if viewDelta(v) == nil {
			dst = appendBaseRanges(dst, v, normalized)
		} else {
			scanRanges(v, normalized, func(_ S2Key, id schema.ImageID) bool {
				dst = append(dst, id)
				return true
			})
		}
		dst = sortSuffix(dst, mark, v.maxID)
	})
	return dst
}

// SearchCell appends the ImageIDs stored in cellID's cell, ascending.
func (i *Index) SearchCell(dst []schema.ImageID, cellID uint64) ([]schema.ImageID, error) {
	key, err := KeyFromCellID(cellID)
	if err != nil {
		return dst, err
	}
	return i.SearchRanges(dst, []Range{{Start: key, End: key}}), nil
}

// SearchCover appends the ImageIDs covered by region (a cell-superset: cover
// cells that merely intersect the region are included).
func (i *Index) SearchCover(dst []schema.ImageID, region s2.Region, options ...CoverOptions) []schema.ImageID {
	var stack [DefaultMaxCells]Range
	ranges := CoverRegion(stack[:0], region, options...)
	return i.SearchRanges(dst, ranges)
}

// SearchRect appends the ImageIDs covered by a latitude/longitude rectangle.
func (i *Index) SearchRect(dst []schema.ImageID, rect s2.Rect, options ...CoverOptions) []schema.ImageID {
	return i.SearchCover(dst, rect, options...)
}

// SearchPolygon appends the ImageIDs covered by polygon.
func (i *Index) SearchPolygon(dst []schema.ImageID, polygon *s2.Polygon, options ...CoverOptions) []schema.ImageID {
	return i.SearchCover(dst, polygon, options...)
}

// SearchRadius appends the ImageIDs whose cell center is within radiusMeters of
// a coordinate, in ascending ImageID order. Distance is measured from the query
// to the authoritative S2Level cell center, so the result is exact at cell
// resolution rather than a raw cover superset.
func (i *Index) SearchRadius(dst []schema.ImageID, latitude, longitude, radiusMeters float64, options ...CoverOptions) ([]schema.ImageID, error) {
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
		scanRanges(v, ranges, func(key S2Key, id schema.ImageID) bool {
			if keyDistanceMeters(key, center) <= radiusMeters {
				dst = append(dst, id)
			}
			return true
		})
		dst = sortSuffix(dst, mark, v.maxID)
	})
	return dst, nil
}

// SearchRadiusResults appends distance-aware matches whose cell center is within
// radiusMeters of a coordinate. Results are ordered by ascending distance then
// ImageID; the cover is only a candidate superset and every returned match
// passes the exact cell-center distance test.
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
		scanRanges(v, ranges, func(key S2Key, id schema.ImageID) bool {
			if d := keyDistanceMeters(key, center); d <= radiusMeters {
				dst = append(dst, Result{ImageID: id, Distance: metersToUint32(d)})
			}
			return true
		})
		slices.SortFunc(dst[mark:], compareResults)
	})
	return dst, nil
}

// nearestHit is one candidate in the bounded nearest-N heap.
type nearestHit struct {
	id     schema.ImageID
	meters float64
}

// nearestWorse reports whether a should be evicted before b (max-heap by
// distance, then ImageID).
func nearestWorse(a, b nearestHit) bool {
	if a.meters != b.meters {
		return a.meters > b.meters
	}
	return a.id > b.id
}

func nearestSiftUp(heap []nearestHit, index int) {
	for index > 0 {
		parent := (index - 1) / 2
		if !nearestWorse(heap[index], heap[parent]) {
			return
		}
		heap[index], heap[parent] = heap[parent], heap[index]
		index = parent
	}
}

func nearestSiftDown(heap []nearestHit, index int) {
	for {
		left, right := 2*index+1, 2*index+2
		worst := index
		if left < len(heap) && nearestWorse(heap[left], heap[worst]) {
			worst = left
		}
		if right < len(heap) && nearestWorse(heap[right], heap[worst]) {
			worst = right
		}
		if worst == index {
			return
		}
		heap[index], heap[worst] = heap[worst], heap[index]
		index = worst
	}
}

// nearestPush maintains a bounded max-heap of the best k hits.
func nearestPush(heap []nearestHit, k int, hit nearestHit) []nearestHit {
	if len(heap) < k {
		heap = append(heap, hit)
		nearestSiftUp(heap, len(heap)-1)
		return heap
	}
	if nearestWorse(heap[0], hit) {
		heap[0] = hit
		nearestSiftDown(heap, 0)
	}
	return heap
}

const (
	nearestInitialMeters = 64.0
	nearestMaxRounds     = 24
	nearestStackK        = 64
)

// Nearest appends the k nearest images to a coordinate, ascending by cell-center
// distance then ImageID. It keeps only a bounded top-k heap while growing an S2
// cap cover, and stops once the k-th cell-center distance is inside the covered
// radius, which proves no unvisited cell can hold a closer result.
func (i *Index) Nearest(dst []Result, latitude, longitude float64, k int) ([]Result, error) {
	if k <= 0 {
		return dst, nil
	}
	center, err := validLatLng(latitude, longitude)
	if err != nil {
		return dst, err
	}
	i.withView(func(v *view) {
		var stack [nearestStackK]nearestHit
		var heap []nearestHit
		if k <= len(stack) {
			heap = stack[:0]
		} else {
			heap = make([]nearestHit, 0, k)
		}
		proven := false
		radius := nearestInitialMeters
		for round := 0; round < nearestMaxRounds; round++ {
			heap = heap[:0]
			var cover [DefaultMaxCells]Range
			ranges := CoverCap(cover[:0], CapFromMeters(center, radius), CoverOptions{MaxCells: i.config.MaxCells})
			scanRanges(v, ranges, func(key S2Key, id schema.ImageID) bool {
				heap = nearestPush(heap, k, nearestHit{id: id, meters: keyDistanceMeters(key, center)})
				return true
			})
			if len(heap) >= k && heap[0].meters <= radius {
				proven = true
				break
			}
			if radius >= math.Pi*earthRadiusMeters {
				break
			}
			radius *= 4
		}
		if !proven {
			// The whole sphere proves exactness; a full scan settles the
			// remaining small-index and safety cases.
			heap = heap[:0]
			full := []Range{{Start: 0, End: S2Key(^uint64(0))}}
			scanRanges(v, full, func(key S2Key, id schema.ImageID) bool {
				heap = nearestPush(heap, k, nearestHit{id: id, meters: keyDistanceMeters(key, center)})
				return true
			})
		}
		slices.SortFunc(heap, func(a, b nearestHit) int {
			if a.meters != b.meters {
				return cmp.Compare(a.meters, b.meters)
			}
			return cmp.Compare(a.id, b.id)
		})
		for _, hit := range heap {
			dst = append(dst, Result{ImageID: hit.id, Distance: metersToUint32(hit.meters)})
		}
	})
	return dst, nil
}

const collectChunk = 256

// collector accumulates ImageIDs for a bitmap sink in bounded, stack-backed
// chunks. It flushes at the chunk capacity, so its own buffer never grows with
// the result cardinality and the sink folds the staged IDs.
type collector struct {
	dst   *roar.MutableBitmap
	buf   []schema.ImageID
	stack [collectChunk]schema.ImageID
}

func (c *collector) add(id schema.ImageID) {
	if len(c.buf) == cap(c.buf) {
		c.flush()
	}
	c.buf = append(c.buf, id)
}

func (c *collector) flush() {
	if len(c.buf) == 0 {
		return
	}
	c.dst.AddMany(c.buf)
	c.buf = c.buf[:0]
}

// collectorPool reuses collectors across queries. A fresh collector escapes to
// the heap (AddMany may retain the slice), so pooling keeps Filter/Collect
// queries allocation-free on the steady-state path while still bounding the
// staged buffer to a single chunk.
var collectorPool = sync.Pool{New: func() any { return new(collector) }}

// acquireCollector returns a pooled collector targeting dst.
func acquireCollector(dst *roar.MutableBitmap) *collector {
	c := collectorPool.Get().(*collector)
	c.dst = dst
	c.buf = c.stack[:0]
	return c
}

// releaseCollector returns c to the pool after the caller has flushed it.
func releaseCollector(c *collector) {
	c.dst = nil
	c.buf = c.stack[:0]
	collectorPool.Put(c)
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
		c := acquireCollector(dst)
		scanRanges(v, normalized, func(_ S2Key, id schema.ImageID) bool {
			c.add(id)
			return true
		})
		c.flush()
		releaseCollector(c)
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
		c := acquireCollector(dst)
		scanRanges(v, ranges, func(key S2Key, id schema.ImageID) bool {
			if keyDistanceMeters(key, center) <= radiusMeters {
				c.add(id)
			}
			return true
		})
		c.flush()
		releaseCollector(c)
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
		c := acquireCollector(dst)
		scanRanges(v, normalized, func(_ S2Key, id schema.ImageID) bool {
			if candidates.Contains(id) {
				c.add(id)
			}
			return true
		})
		c.flush()
		releaseCollector(c)
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
		c := acquireCollector(dst)
		scanRanges(v, ranges, func(key S2Key, id schema.ImageID) bool {
			if candidates.Contains(id) && keyDistanceMeters(key, center) <= radiusMeters {
				c.add(id)
			}
			return true
		})
		c.flush()
		releaseCollector(c)
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
		scanRanges(v, normalized, func(_ S2Key, _ schema.ImageID) bool {
			count++
			return true
		})
	})
	return count
}

// EstimateRanges returns a cheap estimate of the images in ranges, used by the
// search planner to order stages. It counts base keys in range plus pending
// additions without materializing ImageIDs.
func (i *Index) EstimateRanges(ranges []Range) uint64 {
	estimate := uint64(0)
	i.withView(func(v *view) {
		var stack [DefaultMaxCells]Range
		normalized := normalizeResult(&stack, ranges)
		for _, s := range v.shards {
			for _, r := range normalized {
				low := sort.Search(len(s.keys), func(k int) bool { return s.keys[k] >= r.Start })
				high := sort.Search(len(s.keys), func(k int) bool { return s.keys[k] > r.End })
				if high > low {
					estimate += uint64(high - low)
				}
			}
		}
		for n := range v.frozen {
			if v.frozen[n].add && keyInRanges(normalized, v.frozen[n].key) {
				estimate++
			}
		}
		for _, layer := range v.layers {
			for n := range layer {
				if layer[n].add && keyInRanges(normalized, layer[n].key) {
					estimate++
				}
			}
		}
		for n := range v.active {
			if v.active[n].add && keyInRanges(normalized, v.active[n].key) {
				estimate++
			}
		}
	})
	return estimate
}

// Contains reports whether id is stored at key in the effective view.
func (i *Index) Contains(key S2Key, id schema.ImageID) bool {
	found := false
	i.withView(func(v *view) {
		if present, touched := viewTouched(v, id); touched {
			found = present && layerKey(v, id) == key
			return
		}
		si := shardFor(v.shards, key, id)
		if si < 0 {
			return
		}
		s := v.shards[si]
		low := sort.Search(len(s.keys), func(k int) bool { return s.keys[k] >= key })
		for low < len(s.keys) && s.keys[low] == key {
			if s.ids[low] == id {
				found = true
				return
			}
			low++
		}
	})
	return found
}

// layerKey returns the key asserted by the newest mutation layer naming id.
func layerKey(v *view, id schema.ImageID) S2Key {
	if index, ok := mutationIndex(v.active, id); ok {
		return v.active[index].key
	}
	for n := len(v.layers) - 1; n >= 0; n-- {
		if index, ok := mutationIndex(v.layers[n], id); ok {
			return v.layers[n][index].key
		}
	}
	if index, ok := mutationIndex(v.frozen, id); ok {
		return v.frozen[index].key
	}
	return 0
}

func validLatLng(latitude, longitude float64) (s2.LatLng, error) {
	if math.IsNaN(latitude) || math.IsNaN(longitude) ||
		math.IsInf(latitude, 0) || math.IsInf(longitude, 0) ||
		latitude < -90 || latitude > 90 || longitude < -180 || longitude > 180 {
		return s2.LatLng{}, ErrInvalidCoordinate
	}
	return s2.LatLngFromDegrees(latitude, longitude), nil
}

// radixSortThreshold is the suffix length above which the specialized sorts
// replace the comparison sort used for small results.
const radixSortThreshold = 1 << 13

// bitsetMaxID caps the dense bitset accelerator (2^30 ids -> 128 MiB of bits).
const bitsetMaxID = 1 << 30

// sortIDs sorts ids ascending and removes duplicates, returning the shortened
// slice. Small suffixes use the comparison sort; broad results use a dense
// bitset when the ImageID domain is compact, otherwise an LSD radix sort. The
// sorted initial-query contract forces this work, so the goal is to keep it far
// below a general comparison sort. The input may alias the destination suffix.
func sortIDs(ids []schema.ImageID, maxID schema.ImageID) []schema.ImageID {
	switch len(ids) {
	case 0, 1:
		return ids
	}
	if len(ids) < radixSortThreshold {
		slices.Sort(ids)
		return slices.Compact(ids)
	}
	if out, ok := sortIDsBitset(ids, maxID); ok {
		return out
	}
	radixSortIDs(ids)
	return slices.Compact(ids)
}

// sortIDsBitset sorts and de-duplicates ids with a dense bitmap over [0, maxID].
// It returns the shortened slice and true when the bitset was worthwhile (small
// enough, non-sparse domain), otherwise false. The actual maximum of ids is used
// so a stale maxID hint can never overrun the bitset.
func sortIDsBitset(ids []schema.ImageID, maxID schema.ImageID) ([]schema.ImageID, bool) {
	if maxID == 0 {
		return ids, false
	}
	limit := maxID
	for _, id := range ids {
		if id > limit {
			limit = id
		}
	}
	if uint64(limit) > bitsetMaxID {
		return ids, false
	}
	// Reject sparse ID spaces where scanning the bitset would dominate. The
	// dense bitset costs limit/64 words plus len(ids) marks; it wins while that
	// stays within a small multiple of the result size.
	if uint64(limit) > 256*uint64(len(ids)) {
		return ids, false
	}
	words := int(uint64(limit)>>6) + 1
	set := acquireBitset(words)
	for _, id := range ids {
		set[id>>6] |= 1 << (id & 63)
	}
	out := 0
	for w, word := range set {
		for word != 0 {
			b := bits.TrailingZeros64(word)
			ids[out] = schema.ImageID(uint64(w)<<6 | uint64(b))
			out++
			word &= word - 1
		}
	}
	releaseBitset(set)
	return ids[:out], true
}

// bitsetScratchCap bounds the word count of a pooled bitset (256 KiB), so the
// pool cannot retain an unbounded buffer. Larger bitsets are allocated directly.
const bitsetScratchCap = 1 << 15

var bitsetPool = sync.Pool{New: func() any { return make([]uint64, 0, 1024) }}

// acquireBitset returns a zeroed bitset of words words, reusing a pooled buffer
// when possible so broad sorted queries do not allocate per call.
func acquireBitset(words int) []uint64 {
	if words > bitsetScratchCap {
		return make([]uint64, words)
	}
	set := bitsetPool.Get().([]uint64)
	if cap(set) < words {
		set = make([]uint64, words)
	}
	set = set[:words]
	clear(set)
	return set
}

// releaseBitset returns set to the pool for reuse.
func releaseBitset(set []uint64) {
	if cap(set) <= bitsetScratchCap {
		bitsetPool.Put(set[:0])
	}
}

// radixSortIDs sorts a uint64 slice with an 8-pass, base-256 LSD radix sort. One
// temp buffer the size of the result is allocated; eight passes guarantee the
// sorted data lands back in ids. Duplicates are preserved; the caller compacts.
func radixSortIDs(ids []schema.ImageID) {
	const shiftBits = 8
	const bucketCount = 1 << shiftBits
	const mask = bucketCount - 1
	buf := make([]schema.ImageID, len(ids))
	src, dst := ids, buf
	var counts [bucketCount]int
	for shift := uint(0); shift < 64; shift += shiftBits {
		for i := range counts {
			counts[i] = 0
		}
		for _, v := range src {
			counts[(v>>shift)&mask]++
		}
		sum := 0
		for i := range counts {
			c := counts[i]
			counts[i] = sum
			sum += c
		}
		for _, v := range src {
			k := (v >> shift) & mask
			dst[counts[k]] = v
			counts[k]++
		}
		src, dst = dst, src
	}
}
