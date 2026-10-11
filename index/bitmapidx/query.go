package bitmapidx

import (
	"bytes"
	"slices"
	"sort"
	"sync"

	common "github.com/evanoberholster/imageIdx/common"
	"github.com/evanoberholster/imageIdx/roar"
)

// rangeEffectiveTmpPool recycles the accumulator used to fold delta-corrected
// keys during range queries, so the allocation count does not grow with the
// number of corrected keys.
var rangeEffectiveTmpPool = sync.Pool{New: func() any { return roar.NewMutableBitmap() }}

// maxPooledRangeEffectiveBytes bounds a pooled range accumulator.
const maxPooledRangeEffectiveBytes = 1 << 20

// rangeUnion returns the union of the effective postings for keys, which must
// be the sorted, unique in-range key set, and whether the result is heap-owned.
// Base-only keys union through the posting kernel; keys touched by a delta are
// folded into a reusable accumulator and enumerated once, so the allocation
// count stays independent of the number of corrected keys.
func (v *namespaceView) rangeUnion(keys []NamespaceKey) (roar.Bitmap, bool) {
	postings := acquirePostings(len(keys))
	ids := acquireIDs(0)
	tmp := rangeEffectiveTmpPool.Get().(*roar.MutableBitmap)
	for _, key := range keys {
		if v.deltaTouches(key) {
			if p, ok := v.effectiveInto(key, tmp); ok {
				*ids = p.AppendTo(*ids)
			}
			continue
		}
		if p, ok := v.base.get(key); ok {
			*postings = append(*postings, p)
		}
	}
	base, owned := unionPostings(*postings)
	releasePostings(postings)
	if len(*ids) != 0 {
		corr := roar.FromUnsorted(*ids)
		base = roar.Or(base, corr)
		owned = true
	}
	releaseIDs(ids)
	if tmp.RetainedBytes() <= maxPooledRangeEffectiveBytes {
		rangeEffectiveTmpPool.Put(tmp)
	}
	return base, owned
}

// NumericRange is an explicit integer range over a continuous namespace's
// stored grid domain. Open ends and inclusivity are explicit booleans, so
// no bound arithmetic can overflow and empty ranges are representable.
type NumericRange struct {
	Lo          int64
	Hi          int64
	HasLo       bool
	HasHi       bool
	LoInclusive bool
	HiInclusive bool
}

// Between returns the half-open range [lo, hi).
func Between(lo, hi int64) NumericRange {
	return NumericRange{Lo: lo, Hi: hi, HasLo: true, HasHi: true, LoInclusive: true}
}

// AtLeast returns [lo, +inf).
func AtLeast(lo int64) NumericRange {
	return NumericRange{Lo: lo, HasLo: true, LoInclusive: true}
}

// AtMost returns (-inf, hi].
func AtMost(hi int64) NumericRange {
	return NumericRange{Hi: hi, HasHi: true, HiInclusive: true}
}

// empty reports whether the range cannot match any integer: inverted
// bounds, or equal bounds without full inclusivity.
func (r NumericRange) empty() bool {
	if r.HasLo && r.HasHi {
		if r.Lo > r.Hi {
			return true
		}
		if r.Lo == r.Hi && !(r.LoInclusive && r.HiInclusive) {
			return true
		}
	}
	return false
}

func (r NumericRange) contains(grid int64) bool {
	if r.HasLo {
		if r.LoInclusive {
			if grid < r.Lo {
				return false
			}
		} else if grid <= r.Lo {
			return false
		}
	}
	if r.HasHi {
		if r.HiInclusive {
			if grid > r.Hi {
				return false
			}
		} else if grid >= r.Hi {
			return false
		}
	}
	return true
}

// queryKey deduplicates query values by hash-level key and canonical
// bytes. The string conversion copies once per value on the map path;
// tiny queries use a linear scan with no allocation instead.
type queryKey struct {
	base      NamespaceKey
	canonical string
}

// MatchValues returns the images holding every supplied value (logical
// AND), sorted and unique. An empty value list matches nothing.
// Store-owned results are detached from mmap state.
func (i *Index) MatchValues(values ...Value) (result roar.Bitmap, resultErr error) {
	if len(values) == 0 {
		return roar.Bitmap{}, nil
	}
	token := i.readers.Acquire()
	defer i.readers.Release(token)
	view := i.view.Load()
	if i.closed.Load() {
		return roar.Bitmap{}, nil
	}
	keys, ok, err := i.resolveQueryKeys(view, values)
	if err != nil {
		return roar.Bitmap{}, err
	}
	if !ok {
		return roar.Bitmap{}, nil
	}
	result, owned := view.matchResolved(keys)
	i.detachOwned(&result, owned)
	return result, nil
}

// MatchText analyzes query text and returns the images holding every
// resulting token (logical AND). A query producing no tokens matches
// nothing.
func (i *Index) MatchText(query string) (roar.Bitmap, error) {
	namespace := i.spec()
	if namespace.Kind != common.KindBitmapString {
		return roar.Bitmap{}, ErrNamespaceMismatch
	}
	tokens, err := i.analyzer.TokenSet(query, nil)
	if err != nil {
		return roar.Bitmap{}, err
	}
	if len(tokens) == 0 {
		return roar.Bitmap{}, nil
	}
	values := make([]Value, 0, len(tokens))
	for _, text := range tokens {
		values = append(values, Value{Kind: common.KindBitmapString, Bytes: []byte(text)})
	}
	return i.MatchValues(values...)
}

// canonicalizedValue pairs one value's hash-level base key with its
// canonical bytes. canonical is nil for continuous and geo values, which
// have no dictionary entry.
type canonicalizedValue struct {
	base      NamespaceKey
	canonical []byte
}

// canonicalizeValues deduplicates values, canonicalizes them for namespace, and
// returns each distinct value's base key and canonical bytes in first-seen
// order. When retain is set the canonical bytes are copied so callers can
// re-resolve later (compiled queries); otherwise they may alias caller input
// and are valid only for the call. Deduplication is a linear scan for tiny
// inputs and a map for larger ones.
func (i *Index) canonicalizeValues(namespace NamespaceSpec, values []Value, retain bool) ([]canonicalizedValue, error) {
	out := make([]canonicalizedValue, 0, len(values))
	const linearDedupBound = 8
	useLinear := len(values) <= linearDedupBound
	var seen map[queryKey]struct{}
	if !useLinear {
		seen = make(map[queryKey]struct{}, len(values))
	}
	for _, value := range values {
		base, canonical, err := i.keyForValue(namespace, value)
		if err != nil {
			return nil, err
		}
		if useLinear {
			duplicate := false
			for _, prev := range out {
				if prev.base == base && bytes.Equal(prev.canonical, canonical) {
					duplicate = true
					break
				}
			}
			if duplicate {
				continue
			}
		} else {
			signature := queryKey{base: base, canonical: string(canonical)}
			if _, ok := seen[signature]; ok {
				continue
			}
			seen[signature] = struct{}{}
		}
		if retain && canonical != nil {
			canonical = append([]byte{}, canonical...)
		}
		out = append(out, canonicalizedValue{base: base, canonical: canonical})
	}
	return out, nil
}

// resolveKeysPartial resolves each canonicalized value to its dictionary
// key, including any collision slot, and reports complete=false when a
// hashed value is absent. Continuous and geo values pass through with their
// base key.
func resolveKeysPartial(view *namespaceView, items []canonicalizedValue) (keys []NamespaceKey, complete bool) {
	keys = make([]NamespaceKey, 0, len(items))
	complete = true
	for _, item := range items {
		if item.canonical == nil {
			keys = append(keys, item.base)
			continue
		}
		key, ok := view.dict.resolve(item.base, item.canonical)
		if !ok {
			complete = false
			continue
		}
		keys = append(keys, key)
	}
	return keys, complete
}

// resolveQueryKeys maps query values to dictionary keys with slots. ok is
// false when a hashed value is absent, which empties the conjunction.
func (i *Index) resolveQueryKeys(view *namespaceView, values []Value) ([]NamespaceKey, bool, error) {
	items, err := i.canonicalizeValues(i.spec(), values, false)
	if err != nil {
		return nil, false, err
	}
	keys, complete := resolveKeysPartial(view, items)
	if !complete {
		return nil, false, nil
	}
	return keys, true, nil
}

// matchResolved intersects the effective posting of every resolved key and
// reports whether the result is heap-owned. Missing keys empty the
// conjunction. Uses pooled posting scratch. A single operand is passed
// through with its own ownership; multi-way intersections are freshly built.
func (v *namespaceView) matchResolved(keys []NamespaceKey) (result roar.Bitmap, owned bool) {
	postings := acquirePostings(len(keys))
	defer releasePostings(postings)
	for _, key := range keys {
		p, ok := v.effective(key)
		if !ok {
			return roar.Bitmap{}, true
		}
		*postings = append(*postings, p)
	}
	switch len(*postings) {
	case 0:
		return roar.Bitmap{}, true
	case 1:
		p := (*postings)[0]
		return p.bitmap(), p.owned
	default:
		return intersectPostings(*postings), true
	}
}

// ValueIDs appends every image holding value to dst in ascending order.
// Unknown values append nothing. It materializes only the one posting.
func (i *Index) ValueIDs(dst []common.ImageID, value Value) ([]common.ImageID, error) {
	namespace := i.spec()
	base, canonical, err := i.keyForValue(namespace, value)
	if err != nil {
		return dst, err
	}
	token := i.readers.Acquire()
	view := i.view.Load()
	if i.closed.Load() {
		i.readers.Release(token)
		return dst, nil
	}
	key := base
	if canonical != nil {
		resolved, ok := view.dict.resolve(base, canonical)
		if !ok {
			i.readers.Release(token)
			return dst, nil
		}
		key = resolved
	}
	if p, ok := view.effective(key); ok {
		dst = p.appendTo(dst)
	}
	i.readers.Release(token)
	return dst, nil
}

// Range returns the images whose continuous value falls inside bounds,
// sorted and unique. The no-delta path references base postings directly;
// the delta path collects occupied keys across all layers, then unions
// their effective postings.
func (i *Index) Range(bounds NumericRange) (result roar.Bitmap, resultErr error) {
	namespace := i.spec()
	if namespace.Kind != common.KindBitmapContinuous {
		return roar.Bitmap{}, ErrInvalidRange
	}
	if bounds.empty() {
		return roar.Bitmap{}, nil
	}
	token := i.readers.Acquire()
	defer i.readers.Release(token)
	view := i.view.Load()
	if i.closed.Load() {
		return roar.Bitmap{}, nil
	}
	if !view.hasDeltas() {
		start, end := view.baseRangeIndexes(bounds)
		postings := acquirePostings(end - start)
		for index := start; index < end; index++ {
			*postings = append(*postings, &view.base.postings[index])
		}
		out, owned := unionPostings(*postings)
		releasePostings(postings)
		i.detachOwned(&out, owned)
		return out, nil
	}
	keys := view.continuousKeys(bounds)
	if len(keys) == 0 {
		return roar.Bitmap{}, nil
	}
	out, owned := view.rangeUnion(keys)
	i.detachOwned(&out, owned)
	return out, nil
}

// EstimateRange returns a planning hint for the occupied grid keys
// inside bounds without materializing: base cardinality plus delta
// additions minus removals. Multi-valued overlap makes it an upper bound.
func (i *Index) EstimateRange(bounds NumericRange) (uint64, error) {
	namespace := i.spec()
	if namespace.Kind != common.KindBitmapContinuous {
		return 0, ErrInvalidRange
	}
	if bounds.empty() {
		return 0, nil
	}
	token := i.readers.Acquire()
	defer i.readers.Release(token)
	view := i.view.Load()
	if i.closed.Load() {
		return 0, nil
	}
	if !view.hasDeltas() {
		start, end := view.baseRangeIndexes(bounds)
		var estimate uint64
		for index := start; index < end; index++ {
			estimate += view.base.postings[index].cardinality()
		}
		return estimate, nil
	}
	return view.estimateKeys(view.continuousKeys(bounds)), nil
}

// estimateKeys returns the effective cardinality of keys without
// materializing: base cardinality plus delta additions minus removals. A
// single key is exact because composeNamespaceDelta admits an addition only when
// the ID is absent from base+frozen and a removal only when it is present,
// so CountValues uses this for one key; multiple keys can overlap, making the
// sum an upper bound. The running total never underflows: a removal larger
// than the current total clamps to zero, which can only make the multi-key
// planning hint less exact, never break a query.
func (v *namespaceView) estimateKeys(keys []NamespaceKey) uint64 {
	var estimate uint64
	for _, key := range keys {
		if p, ok := v.base.get(key); ok {
			estimate += p.cardinality()
		}
		for _, delta := range []*namespaceDelta{v.frozen, v.active} {
			bd, ok := delta.delta(key)
			if !ok {
				continue
			}
			estimate += bd.additions.Cardinality()
			if removals := bd.removals.Cardinality(); removals > estimate {
				estimate = 0
			} else {
				estimate -= removals
			}
		}
	}
	return estimate
}

// CountValues returns the number of images holding every supplied value
// without materializing the intersection when possible. No values count
// zero.
func (i *Index) CountValues(values ...Value) (uint64, error) {
	if len(values) == 0 {
		return 0, nil
	}
	token := i.readers.Acquire()
	defer i.readers.Release(token)
	view := i.view.Load()
	if i.closed.Load() {
		return 0, nil
	}
	keys, ok, err := i.resolveQueryKeys(view, values)
	if err != nil {
		return 0, err
	}
	if !ok {
		return 0, nil
	}
	if len(keys) == 1 {
		// estimateKeys is exact for a single key: composeNamespaceDelta admits an
		// addition only when the ID is absent from base+frozen and a removal
		// only when it is present, so base + additions - removals is the true
		// corrected cardinality. This avoids materializing the posting just to
		// count it.
		return view.estimateKeys(keys), nil
	}
	postings := acquirePostings(len(keys))
	defer releasePostings(postings)
	for _, key := range keys {
		p, ok := view.effective(key)
		if !ok {
			return 0, nil
		}
		*postings = append(*postings, p)
	}
	return intersectPostingsCardinality(*postings), nil
}

// CountRange returns the number of images inside bounds by unioning
// bitmaps and counting, without appending IDs. Scratch is pre-sized once
// per call.
func (i *Index) CountRange(bounds NumericRange) (uint64, error) {
	namespace := i.spec()
	if namespace.Kind != common.KindBitmapContinuous {
		return 0, ErrInvalidRange
	}
	if bounds.empty() {
		return 0, nil
	}
	token := i.readers.Acquire()
	defer i.readers.Release(token)
	view := i.view.Load()
	if i.closed.Load() {
		return 0, nil
	}
	if !view.hasDeltas() {
		start, end := view.baseRangeIndexes(bounds)
		bitmaps := acquireBitmaps(end - start)
		defer releaseBitmaps(bitmaps)
		for index := start; index < end; index++ {
			*bitmaps = append(*bitmaps, view.base.postings[index].bitmap())
		}
		return roar.OrMany(*bitmaps...).Cardinality(), nil
	}
	keys := view.continuousKeys(bounds)
	if len(keys) == 0 {
		return 0, nil
	}
	out, _ := view.rangeUnion(keys)
	return out.Cardinality(), nil
}

// FilterValues appends the candidates holding every supplied value to
// dst. No values append nothing: an empty conjunction matches nothing.
func (i *Index) FilterValues(dst, candidates []common.ImageID, values ...Value) ([]common.ImageID, error) {
	if len(values) == 0 {
		return dst, nil
	}
	token := i.readers.Acquire()
	defer i.readers.Release(token)
	view := i.view.Load()
	if i.closed.Load() {
		return dst, nil
	}
	keys, ok, err := i.resolveQueryKeys(view, values)
	if err != nil {
		return dst, err
	}
	if !ok || len(keys) == 0 || len(candidates) == 0 {
		return dst, nil
	}
	return view.filterResolved(dst, candidates, keys), nil
}

// FilterRange preserves order and duplicates; dst=candidates[:0] is safe.
// It probes candidates or unions postings, according to the estimated work.
func (i *Index) FilterRange(dst, candidates []common.ImageID, bounds NumericRange) ([]common.ImageID, error) {
	if i.spec().Kind != common.KindBitmapContinuous {
		return dst, ErrInvalidRange
	}
	if bounds.empty() || len(candidates) == 0 {
		return dst, nil
	}
	token := i.readers.Acquire()
	defer i.readers.Release(token)
	view := i.view.Load()
	if i.closed.Load() {
		return dst, nil
	}
	start, end := view.baseRangeIndexes(bounds)
	keys := view.base.keys[start:end]
	if view.hasDeltas() {
		keys = view.continuousKeys(bounds)
	}
	if len(keys) > 1 && uint64(len(keys))*uint64(len(candidates)) > 4*view.estimateKeys(keys) {
		bit, _ := view.rangeUnion(keys)
		return filterBitmap(dst, candidates, bit), nil
	}
	for _, id := range candidates {
		for _, key := range keys {
			if view.contains(key, id) {
				dst = append(dst, id)
				break
			}
		}
	}
	return dst, nil
}

// filterBitmap appends the candidates present in bit, tolerating unsorted
// input. It probes with a cursor and allocates nothing beyond dst growth.
func filterBitmap(dst, candidates []common.ImageID, bit roar.Bitmap) []common.ImageID {
	if bit.IsEmpty() {
		return dst
	}
	cursor := bit.NewCursor()
	for _, id := range candidates {
		if cursor.Contains(id) {
			dst = append(dst, id)
		}
	}
	return dst
}

// continuousKeys merges the sorted key spans from base, frozen, and active.
// It allocates once and emits each occupied grid key at most once.
func (v *namespaceView) continuousKeys(bounds NumericRange) []NamespaceKey {
	start, end := v.baseRangeIndexes(bounds)
	base := v.base.keys[start:end]
	var frozen, active []NamespaceKey
	if v.frozen != nil {
		start, end = rangeKeyIndexes(v.frozen.keys, bounds)
		frozen = v.frozen.keys[start:end]
	}
	if v.active != nil {
		start, end = rangeKeyIndexes(v.active.keys, bounds)
		active = v.active.keys[start:end]
	}
	var keys []NamespaceKey
	if total := len(base) + len(frozen) + len(active); total != 0 {
		keys = make([]NamespaceKey, 0, total)
	}
	for len(base)+len(frozen)+len(active) != 0 {
		var next NamespaceKey
		found := false
		for _, run := range [3][]NamespaceKey{base, frozen, active} {
			if len(run) != 0 && (!found || compareNamespaceKey(run[0], next) < 0) {
				next, found = run[0], true
			}
		}
		keys = append(keys, next)
		if len(base) != 0 && base[0] == next {
			base = base[1:]
		}
		if len(frozen) != 0 && frozen[0] == next {
			frozen = frozen[1:]
		}
		if len(active) != 0 && active[0] == next {
			active = active[1:]
		}
	}
	return keys
}

// baseRangeIndexes returns the contiguous base key span inside bounds.
// One namespace owns one Type and continuous keys always use Slot zero, so
// Hash order alone selects the span via two binary searches.
func (v *namespaceView) baseRangeIndexes(bounds NumericRange) (int, int) {
	return rangeKeyIndexes(v.base.keys, bounds)
}

func rangeKeyIndexes(keys []NamespaceKey, bounds NumericRange) (int, int) {
	start := 0
	if bounds.HasLo {
		start = sort.Search(len(keys), func(index int) bool {
			grid := decodeOrderKey(keys[index].Hash)
			if bounds.LoInclusive {
				return grid >= bounds.Lo
			}
			return grid > bounds.Lo
		})
	}
	end := len(keys)
	if bounds.HasHi {
		end = sort.Search(len(keys), func(index int) bool {
			grid := decodeOrderKey(keys[index].Hash)
			if bounds.HiInclusive {
				return grid > bounds.Hi
			}
			return grid >= bounds.Hi
		})
	}
	if start > end {
		return end, end
	}
	return start, end
}

// detachOwned clones a query result when it could alias store-owned mmap
// state. Only a result whose bytes live in the mapped segment is not
// heap-owned; corrected postings, tiny postings, and fresh unions and
// intersections are heap-owned and are returned without copying. This keeps
// the lifetime safety of the old unconditional clone while removing a full
// posting copy from every query that materialized its own result.
func (i *Index) detachOwned(result *roar.Bitmap, owned bool) {
	if i.owner != nil && !owned {
		*result = result.Clone()
	}
}

// AppendTo appends every live ImageID in the namespace to dst in ascending
// order. It is a full-namespace scan for diagnostics and benchmarks; prefer a
// typed namespace query. It materializes merged deltas.
func (i *Index) AppendTo(dst []common.ImageID) []common.ImageID {
	token := i.readers.Acquire()
	defer i.readers.Release(token)
	if i.closed.Load() {
		return dst
	}
	view := i.view.Load()
	start := len(dst)
	for _, key := range view.allKeys() {
		if posting, ok := view.effective(key); ok && !posting.isEmpty() {
			dst = posting.bitmap().AppendTo(dst)
		}
	}
	slices.Sort(dst[start:])
	tail := slices.Compact(dst[start:])
	return dst[:start+len(tail)]
}
