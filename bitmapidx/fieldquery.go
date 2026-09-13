package bitmapidx

import (
	"slices"
	"sort"

	"github.com/evanoberholster/imageIdx/roar"
)

// NumericRange is an explicit integer range over a continuous field's stored
// grid domain. Open ends and inclusivity are explicit, so no bound arithmetic
// can overflow.
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

// empty reports whether the range cannot match any integer.
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

// queryKey deduplicates query values by hash-level key and canonical bytes.
type queryKey struct {
	base      FieldKey
	canonical string
}

// MatchValues returns images holding every supplied value (logical AND).
func (i *FieldIndex) MatchValues(values ...Value) (roar.Bitmap, error) {
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
	return view.matchResolved(keys), nil
}

// MatchText analyzes query text and returns images holding every resulting
// token (logical AND). A query producing no tokens matches nothing.
func (i *FieldIndex) MatchText(query string) (roar.Bitmap, error) {
	field := i.Field()
	if field.Kind != KindText {
		return roar.Bitmap{}, ErrFieldMismatch
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
		values = append(values, Value{Kind: KindText, Bytes: []byte(text)})
	}
	return i.MatchValues(values...)
}

// resolveQueryKeys maps query values to dictionary keys. ok is false when a
// hashed value is absent, which makes a conjunction empty.
func (i *FieldIndex) resolveQueryKeys(view *fieldView, values []Value) ([]FieldKey, bool, error) {
	field := i.Field()
	keys := make([]FieldKey, 0, len(values))
	seen := make(map[queryKey]struct{}, len(values))
	for _, value := range values {
		base, canonical, err := i.keyForValue(field, value)
		if err != nil {
			return nil, false, err
		}
		signature := queryKey{base: base, canonical: string(canonical)}
		if _, ok := seen[signature]; ok {
			continue
		}
		seen[signature] = struct{}{}
		if canonical == nil {
			keys = append(keys, base)
			continue
		}
		key, ok := view.dict.resolve(base, canonical)
		if !ok {
			return nil, false, nil
		}
		keys = append(keys, key)
	}
	return keys, true, nil
}

// matchResolved intersects the effective posting of every resolved key.
func (v *fieldView) matchResolved(keys []FieldKey) roar.Bitmap {
	postings := make([]roar.Bitmap, 0, len(keys))
	for _, key := range keys {
		bit, ok := v.effective(key)
		if !ok {
			return roar.Bitmap{}
		}
		postings = append(postings, bit)
	}
	switch len(postings) {
	case 0:
		return roar.Bitmap{}
	case 1:
		return postings[0]
	default:
		return roar.AndMany(postings...)
	}
}

// ValueIDs appends every image holding value to dst in ascending order.
func (i *FieldIndex) ValueIDs(dst []ImageID, value Value) ([]ImageID, error) {
	field := i.Field()
	base, canonical, err := i.keyForValue(field, value)
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
	if bit, ok := view.effective(key); ok {
		dst = bit.AppendTo(dst)
	}
	i.readers.Release(token)
	return dst, nil
}

// Range returns images whose continuous value falls inside bounds. It unions
// the effective postings of every occupied grid key in the span.
func (i *FieldIndex) Range(bounds NumericRange) (roar.Bitmap, error) {
	field := i.Field()
	if field.Kind != KindContinuous {
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
	keys := view.continuousKeys(field.Type, bounds)
	if len(keys) == 0 {
		return roar.Bitmap{}, nil
	}
	postings := make([]roar.Bitmap, 0, len(keys))
	for _, key := range keys {
		if bit, ok := view.effective(key); ok {
			postings = append(postings, bit)
		}
	}
	switch len(postings) {
	case 0:
		return roar.Bitmap{}, nil
	case 1:
		return postings[0], nil
	default:
		return roar.OrMany(postings...), nil
	}
}

// EstimateRange returns a planning hint: the summed base cardinality of the
// occupied grid keys inside bounds. It is not an exact effective count.
func (i *FieldIndex) EstimateRange(bounds NumericRange) (uint64, error) {
	field := i.Field()
	if field.Kind != KindContinuous {
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
	var estimate uint64
	start, end := view.baseRangeIndexes(bounds)
	for _, bit := range view.base.bits[start:end] {
		estimate += bit.Cardinality()
	}
	return estimate, nil
}

// CountValues returns the number of images holding every supplied value.
func (i *FieldIndex) CountValues(values ...Value) (uint64, error) {
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
	postings := make([]roar.Bitmap, 0, len(keys))
	for _, key := range keys {
		bit, ok := view.effective(key)
		if !ok {
			return 0, nil
		}
		postings = append(postings, bit)
	}
	switch len(postings) {
	case 0:
		return 0, nil
	case 1:
		return postings[0].Cardinality(), nil
	default:
		return roar.AndManyCardinality(postings...), nil
	}
}

// CountRange returns the number of images inside bounds.
func (i *FieldIndex) CountRange(bounds NumericRange) (uint64, error) {
	match, err := i.Range(bounds)
	if err != nil {
		return 0, err
	}
	return match.Cardinality(), nil
}

// FilterValues appends candidates holding every supplied value to dst.
func (i *FieldIndex) FilterValues(dst, candidates []ImageID, values ...Value) ([]ImageID, error) {
	match, err := i.MatchValues(values...)
	if err != nil {
		return dst, err
	}
	return filterBitmap(dst, candidates, match), nil
}

// FilterRange appends candidates inside bounds to dst.
func (i *FieldIndex) FilterRange(dst, candidates []ImageID, bounds NumericRange) ([]ImageID, error) {
	match, err := i.Range(bounds)
	if err != nil {
		return dst, err
	}
	return filterBitmap(dst, candidates, match), nil
}

// filterBitmap appends candidates present in bit, tolerating unsorted input.
func filterBitmap(dst, candidates []ImageID, bit roar.Bitmap) []ImageID {
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

// continuousKeys returns the occupied grid keys inside bounds from all layers.
func (v *fieldView) continuousKeys(t IndexType, bounds NumericRange) []FieldKey {
	start, end := v.baseRangeIndexes(bounds)
	var keys []FieldKey
	keys = append(keys, v.base.keys[start:end]...)
	for _, delta := range []*fieldDelta{v.frozen, v.active} {
		if delta == nil {
			continue
		}
		for _, key := range delta.keys {
			if key.Type != t || key.Slot != 0 {
				continue
			}
			if bounds.contains(decodeOrderKey(key.Hash)) {
				keys = append(keys, key)
			}
		}
	}
	if len(keys) < 2 {
		return keys
	}
	slices.SortFunc(keys, compareFieldKey)
	return slices.Compact(keys)
}

// baseRangeIndexes returns the contiguous base key span inside bounds. Keys
// are sorted by (Type, Hash, Slot); continuous keys share Type and Slot zero.
func (v *fieldView) baseRangeIndexes(bounds NumericRange) (int, int) {
	keys := v.base.keys
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
