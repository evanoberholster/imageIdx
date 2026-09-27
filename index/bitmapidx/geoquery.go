package bitmapidx

import (
	"cmp"
	"math"
	"slices"
	"sort"

	common "github.com/evanoberholster/imageIdx/common"
	"github.com/evanoberholster/imageIdx/roar"
)

// geoKeys returns the occupied keys inside ranges from base plus deltas,
// sorted and deduplicated with one allocation. Prefer forEachGeoKey when
// callers can tolerate duplicate keys and want no allocation.
func (v *namespaceView) geoKeys(ranges []GeoRange) []NamespaceKey {
	var keys []NamespaceKey
	collect := func(entries []NamespaceKey) {
		for _, keyRange := range ranges {
			start, end := geoBaseRangeIn(entries, keyRange)
			keys = append(keys, entries[start:end]...)
		}
	}
	collect(v.base.keys)
	for _, delta := range []*namespaceDelta{v.frozen, v.active} {
		if delta == nil {
			continue
		}
		collect(delta.keys)
	}
	if len(keys) < 2 {
		return keys
	}
	slices.SortFunc(keys, compareNamespaceKey)
	return slices.Compact(keys)
}

// allKeys returns one copy of every occupied key in the namespace, sorted
// and deduplicated. Used by full scans such as AppendTo with deltas.
func (v *namespaceView) allKeys() []NamespaceKey {
	var keys []NamespaceKey
	keys = append(keys, v.base.keys...)
	for _, delta := range []*namespaceDelta{v.frozen, v.active} {
		if delta == nil {
			continue
		}
		keys = append(keys, delta.keys...)
	}
	if len(keys) < 2 {
		return keys
	}
	slices.SortFunc(keys, compareNamespaceKey)
	return slices.Compact(keys)
}

// geoBaseRangeIn returns the index span of entries sorted by Hash inside
// an inclusive key range, via two binary searches. Inverted ranges select
// nothing.
func geoBaseRangeIn(entries []NamespaceKey, keyRange GeoRange) (int, int) {
	if keyRange.End < keyRange.Start {
		return 0, 0
	}
	start := sort.Search(len(entries), func(i int) bool { return entries[i].Hash >= keyRange.Start })
	end := sort.Search(len(entries), func(i int) bool { return entries[i].Hash > keyRange.End })
	if start > end {
		return end, end
	}
	return start, end
}

// GeoRanges returns the images inside any inclusive key range, sorted
// and unique. Ranges may be unnormalized and overlapping. The no-delta
// path references base postings directly; the delta path unions effective
// postings for occupied keys.
func (i *Index) GeoRanges(ranges []GeoRange) (result roar.Bitmap, resultErr error) {
	if i.spec().Kind != common.KindBitmapGeo {
		return roar.Bitmap{}, ErrNamespaceMismatch
	}
	if len(ranges) == 0 {
		return roar.Bitmap{}, nil
	}
	token := i.readers.Acquire()
	defer i.readers.Release(token)
	view := i.view.Load()
	if i.closed.Load() {
		return roar.Bitmap{}, nil
	}
	if !view.hasDeltas() {
		postings := acquirePostings(view.countGeoKeys(ranges))
		for _, keyRange := range ranges {
			start, end := geoBaseRangeIn(view.base.keys, keyRange)
			for index := start; index < end; index++ {
				*postings = append(*postings, &view.base.postings[index])
			}
		}
		out, owned := unionPostings(*postings)
		releasePostings(postings)
		i.detachOwned(&out, owned)
		return out, nil
	}
	keys := view.geoKeys(ranges)
	if len(keys) == 0 {
		return roar.Bitmap{}, nil
	}
	postings := acquirePostings(len(keys))
	defer releasePostings(postings)
	for _, key := range keys {
		if p, ok := view.effective(key); ok {
			*postings = append(*postings, p)
		}
	}
	out, owned := unionPostings(*postings)
	i.detachOwned(&out, owned)
	return out, nil
}

// FilterGeoRanges appends the candidates inside any key range to dst. Covered
// base spans and in-range delta keys are resolved once per call, so each
// candidate costs a posting probe instead of a binary search. It preserves the
// candidates' order and allocates nothing beyond dst growth for typical range
// counts. Prefer CollectGeoRanges plus bitmap probes when buckets are many or
// the match will be reused; see CountGeoCells for the planning probe.
func (i *Index) FilterGeoRanges(dst, candidates []common.ImageID, ranges []GeoRange) ([]common.ImageID, error) {
	if i.spec().Kind != common.KindBitmapGeo {
		return dst, ErrNamespaceMismatch
	}
	token := i.readers.Acquire()
	view := i.view.Load()
	if !i.closed.Load() && len(ranges) != 0 {
		var spanBuf [8]geoBaseSpan
		var keyBuf [16]NamespaceKey
		probe := view.prepareGeoProbe(ranges, spanBuf[:0], keyBuf[:0])
		for _, id := range candidates {
			if view.geoProbeContains(&probe, id) {
				dst = append(dst, id)
			}
		}
	}
	i.readers.Release(token)
	return dst, nil
}

// geoBaseSpan is one precomputed base key span for a geo range.
type geoBaseSpan struct {
	start int
	end   int
}

// geoProbe is a prepared geo filter: base spans and in-range delta keys are
// resolved once so candidate probes avoid per-candidate binary searches.
type geoProbe struct {
	spans     []geoBaseSpan
	deltaKeys []NamespaceKey
}

// prepareGeoProbe resolves ranges against the view once. spanBuf and keyBuf are
// caller-owned scratch; unusually large range sets fall back to allocation.
func (v *namespaceView) prepareGeoProbe(ranges []GeoRange, spanBuf []geoBaseSpan, keyBuf []NamespaceKey) geoProbe {
	spans := spanBuf
	for _, keyRange := range ranges {
		if keyRange.End < keyRange.Start {
			continue
		}
		start, end := geoBaseRangeIn(v.base.keys, keyRange)
		spans = append(spans, geoBaseSpan{start: start, end: end})
	}
	if !v.hasDeltas() {
		return geoProbe{spans: spans}
	}
	keys := keyBuf
	for _, delta := range []*namespaceDelta{v.frozen, v.active} {
		if delta == nil {
			continue
		}
		for _, key := range delta.keys {
			if key.Slot != 0 {
				continue
			}
			for _, keyRange := range ranges {
				if key.Hash >= keyRange.Start && key.Hash <= keyRange.End {
					keys = append(keys, key)
					break
				}
			}
		}
	}
	return geoProbe{spans: spans, deltaKeys: keys}
}

// geoProbeContains reports whether id holds any bucket inside the prepared
// spans, walking base postings directly when no deltas are visible and the
// full active/frozen/base precedence otherwise.
func (v *namespaceView) geoProbeContains(p *geoProbe, id common.ImageID) bool {
	if p.deltaKeys == nil {
		for _, span := range p.spans {
			postings := v.base.postings[span.start:span.end]
			for index := range postings {
				if postings[index].contains(id) {
					return true
				}
			}
		}
		return false
	}
	for _, span := range p.spans {
		for _, key := range v.base.keys[span.start:span.end] {
			if v.contains(key, id) {
				return true
			}
		}
	}
	for _, key := range p.deltaKeys {
		if v.contains(key, id) {
			return true
		}
	}
	return false
}

// EstimateGeoRanges returns the effective candidate count for occupied
// buckets inside ranges without materializing. Multi-valued overlap
// makes it an upper bound; it is a planning hint, not an exact count.
func (i *Index) EstimateGeoRanges(ranges []GeoRange) (uint64, error) {
	if i.spec().Kind != common.KindBitmapGeo {
		return 0, ErrNamespaceMismatch
	}
	token := i.readers.Acquire()
	defer i.readers.Release(token)
	view := i.view.Load()
	if i.closed.Load() {
		return 0, nil
	}
	if view.hasDeltas() {
		return view.estimateKeys(view.geoKeys(ranges)), nil
	}
	var estimate uint64
	for _, keyRange := range ranges {
		start, end := geoBaseRangeIn(view.base.keys, keyRange)
		for _, p := range view.base.postings[start:end] {
			estimate += p.cardinality()
		}
	}
	return estimate, nil
}

// CountGeoRanges returns the number of images inside any key range by
// unioning bitmaps and counting, without appending IDs. The no-delta
// path pre-sizes scratch once from the occupied bucket count.
func (i *Index) CountGeoRanges(ranges []GeoRange) (uint64, error) {
	if i.spec().Kind != common.KindBitmapGeo {
		return 0, ErrNamespaceMismatch
	}
	token := i.readers.Acquire()
	defer i.readers.Release(token)
	view := i.view.Load()
	if i.closed.Load() {
		return 0, nil
	}
	if !view.hasDeltas() {
		bitmaps := acquireBitmaps(view.countGeoKeys(ranges))
		defer releaseBitmaps(bitmaps)
		for _, keyRange := range ranges {
			start, end := geoBaseRangeIn(view.base.keys, keyRange)
			for _, p := range view.base.postings[start:end] {
				*bitmaps = append(*bitmaps, p.bitmap())
			}
		}
		return roar.OrMany(*bitmaps...).Cardinality(), nil
	}
	keys := view.geoKeys(ranges)
	bitmaps := acquireBitmaps(len(keys))
	defer releaseBitmaps(bitmaps)
	for _, key := range keys {
		if p, ok := view.effective(key); ok {
			*bitmaps = append(*bitmaps, p.bitmap())
		}
	}
	return roar.OrMany(*bitmaps...).Cardinality(), nil
}

// CollectGeoRanges folds every effective posting inside ranges into m.
// The caller resets m first and materializes once with AppendTo. The
// delta path uses deduplicated keys so each posting folds once.
func (i *Index) CollectGeoRanges(m *roar.MutableBitmap, ranges []GeoRange) {
	if m == nil || len(ranges) == 0 || i.spec().Kind != common.KindBitmapGeo {
		return
	}
	token := i.readers.Acquire()
	view := i.view.Load()
	if !i.closed.Load() {
		if !view.hasDeltas() {
			for _, keyRange := range ranges {
				start, end := geoBaseRangeIn(view.base.keys, keyRange)
				for index := start; index < end; index++ {
					view.base.postings[index].addTo(m)
				}
			}
		} else {
			view.forEachGeoKey(ranges, func(key NamespaceKey) {
				if p, ok := view.effective(key); ok {
					p.addTo(m)
				}
			})
		}
	}
	i.readers.Release(token)
}

// CountGeoCells returns the number of occupied level-16 buckets inside
// any key range without allocating. It is the verify-versus-collect
// planning probe: small counts favor FilterGeoRanges probes, large counts
// favor CollectGeoRanges materialization. Delta keys present in multiple
// layers count twice, so it is an upper bound.
func (i *Index) CountGeoCells(ranges []GeoRange) int {
	if len(ranges) == 0 || i.spec().Kind != common.KindBitmapGeo {
		return 0
	}
	token := i.readers.Acquire()
	view := i.view.Load()
	count := 0
	if !i.closed.Load() {
		count = view.countGeoKeys(ranges)
	}
	i.readers.Release(token)
	return count
}

// forEachGeoKey calls fn for every occupied key inside ranges without
// allocating. Keys present in multiple layers repeat; callers must be
// idempotent (re-Oring into a mutable bitmap, counting at most once per
// layer is caller's choice). Prefer geoKeys when duplicates cost more
// than one allocation.
func (v *namespaceView) forEachGeoKey(ranges []GeoRange, fn func(NamespaceKey)) {
	for _, keyRange := range ranges {
		if keyRange.End < keyRange.Start {
			continue
		}
		start, end := geoBaseRangeIn(v.base.keys, keyRange)
		for _, key := range v.base.keys[start:end] {
			fn(key)
		}
	}
	for _, delta := range []*namespaceDelta{v.frozen, v.active} {
		if delta == nil {
			continue
		}
		for _, key := range delta.keys {
			if key.Slot != 0 {
				continue
			}
			for _, keyRange := range ranges {
				if key.Hash >= keyRange.Start && key.Hash <= keyRange.End {
					fn(key)
					break
				}
			}
		}
	}
}

// countGeoKeys counts occupied keys inside ranges without allocating.
// Delta keys present in multiple layers count twice; see CountGeoCells.
func (v *namespaceView) countGeoKeys(ranges []GeoRange) int {
	count := 0
	for _, keyRange := range ranges {
		if keyRange.End < keyRange.Start {
			continue
		}
		start, end := geoBaseRangeIn(v.base.keys, keyRange)
		count += end - start
	}
	for _, delta := range []*namespaceDelta{v.frozen, v.active} {
		if delta == nil {
			continue
		}
		for _, key := range delta.keys {
			if key.Slot != 0 {
				continue
			}
			for _, keyRange := range ranges {
				if key.Hash >= keyRange.Start && key.Hash <= keyRange.End {
					count++
					break
				}
			}
		}
	}
	return count
}

// AppendTo appends every indexed image ID in ascending order, preserving
// dst's prefix. Capacity is reserved once from the cardinality estimate
// so per-key appends do not regrow. Postings append per key, then the
// tail sorts and compacts once; disjoint single-valued namespaces could merge
// without a global sort, but one sort keeps the path simple and uniform.
func (i *Index) AppendTo(dst []common.ImageID) []common.ImageID {
	token := i.readers.Acquire()
	defer i.readers.Release(token)
	view := i.view.Load()
	if i.closed.Load() {
		return dst
	}
	if !view.hasDeltas() {
		var estimate uint64
		for index := range view.base.postings {
			estimate += view.base.postings[index].cardinality()
		}
		if estimate > 0 {
			dst = slices.Grow(dst, int(estimate))
		}
		start := len(dst)
		for index := range view.base.postings {
			dst = view.base.postings[index].appendTo(dst)
		}
		values := dst[start:]
		slices.Sort(values)
		values = slices.Compact(values)
		return dst[:start+len(values)]
	}
	keys := view.allKeys()
	if estimate := view.estimateKeys(keys); estimate > 0 {
		dst = slices.Grow(dst, int(estimate))
	}
	start := len(dst)
	for _, key := range keys {
		if p, ok := view.effective(key); ok {
			dst = p.appendTo(dst)
		}
	}
	values := dst[start:]
	slices.Sort(values)
	values = slices.Compact(values)
	return dst[:start+len(values)]
}

// validateGeoChanges enforces one cell per image within the batch before
// publication. Removals validate first so a stale From reports
// ErrInvalidMutation rather than tripping the target-exists check; then
// each added image must be new to its target key and appear only once in
// the batch. Cross-batch single-cell discipline has no reverse index:
// successive Adds of different cells for one image are caller error (use
// Move); only same-key duplicates and in-batch double-adds are rejected
// here.
func validateGeoChanges(view *namespaceView, changes []stagedChange) error {
	if view.namespace.Kind != common.KindBitmapGeo {
		return nil
	}
	for _, change := range changes {
		for _, id := range change.remove {
			if !view.contains(change.key, id) {
				return ErrInvalidMutation
			}
		}
	}
	added := make(map[common.ImageID]struct{}, len(changes))
	for _, change := range changes {
		for _, id := range change.add {
			if _, ok := added[id]; ok {
				return ErrDuplicateImageID
			}
			added[id] = struct{}{}
			if view.contains(change.key, id) {
				return ErrDuplicateImageID
			}
		}
	}
	return nil
}

// GeoCoordinateLookup supplies canonical coordinates for nearest search.
// Images without coordinates report ok=false and are skipped.
type GeoCoordinateLookup func(common.ImageID) (latitude, longitude float64, ok bool)

// NearestResult is one nearest match with its haversine distance.
type NearestResult struct {
	ImageID        common.ImageID
	DistanceMeters float64
}

// Nearest returns the exact top K among indexed images with coordinates.
// It scans the whole effective set and sorts every candidate: O(N log N)
// in namespace size. It suits modest namespaces, not full-catalog nearest search.
// Invalid origins, non-positive k, nil lookups, and wrong kinds append
// nothing.
func (i *Index) Nearest(dst []NearestResult, latitude, longitude float64, k int, lookup GeoCoordinateLookup) []NearestResult {
	if i.spec().Kind != common.KindBitmapGeo || k <= 0 || lookup == nil {
		return dst
	}
	if _, err := KeyFromLatLng(latitude, longitude); err != nil {
		return dst
	}
	start := len(dst)
	for _, id := range i.AppendTo(nil) {
		lat, lng, ok := lookup(id)
		if !ok {
			continue
		}
		dst = append(dst, NearestResult{ImageID: id, DistanceMeters: haversineMeters(latitude, longitude, lat, lng)})
	}
	slices.SortFunc(dst[start:], func(a, b NearestResult) int {
		return cmp.Compare(a.DistanceMeters, b.DistanceMeters)
	})
	if len(dst)-start > k {
		dst = dst[:start+k]
	}
	return dst
}

// DistanceMeters returns the haversine distance in metres between two
// coordinates. It is the metric Nearest ranks by, exposed for callers that
// rank their own candidate sets.
func DistanceMeters(lat1, lng1, lat2, lng2 float64) float64 {
	return haversineMeters(lat1, lng1, lat2, lng2)
}

func haversineMeters(lat1, lng1, lat2, lng2 float64) float64 {
	const radians = math.Pi / 180
	lat1 *= radians
	lat2 *= radians
	dLat := lat2 - lat1
	dLng := (lng2 - lng1) * radians
	a := math.Sin(dLat/2)*math.Sin(dLat/2) + math.Cos(lat1)*math.Cos(lat2)*math.Sin(dLng/2)*math.Sin(dLng/2)
	return earthRadiusMeters * 2 * math.Atan2(math.Sqrt(a), math.Sqrt(1-a))
}
