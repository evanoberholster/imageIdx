package bitmapidx

import (
	"math/bits"

	common "github.com/evanoberholster/imageIdx/common"
	"github.com/evanoberholster/imageIdx/roar"
)

// BSI (bit-sliced index) storage reuses the existing posting table: a BSI
// namespace is a set of reserved SliceKeys, one for existence and one per
// value bit, each a roaring bitmap of ValueIDs. This means deltas, rotation,
// compaction, checkpointing, and mmap reopen all work unchanged; only the
// write expansion (logical value -> slice memberships) and the queries are
// BSI-specific.
//
//   - slice 0 is the existence bitmap: a ValueID is present iff the namespace
//     holds a value for it. Zero is a valid value, distinguished by existence.
//   - slice 1+bit is the bitmap of ValueIDs whose value has that bit set.
//
// BSI16 uses bits 0..15; bits 16..31 must stay empty. No bit slice contains a
// ValueID absent from existence, and no ValueID appears in more than one
// value slot (its single value is the bit pattern).
const (
	bsiExistsSlice uint64 = 0
	bsiBitSlice0   uint64 = 1 // slice for value bit 0
	bsiMaxSlices          = 33
)

// bsiSliceKey returns the posting key for one BSI slice. Slot is always zero:
// BSI values are never hashed and never collide.
func bsiSliceKey(ns common.Namespace, slice uint64) NamespaceKey {
	return NamespaceKey{Namespace: ns, Hash: slice}
}

// namespaceUsesDictionary reports whether a kind stores canonical bytes in the
// value dictionary. Continuous and BSI values have no canonical bytes.
func namespaceUsesDictionary(kind common.NamespaceKind) bool {
	switch kind {
	case common.KindBitmapEnum, common.KindBitmapBool, common.KindBitmapString:
		return true
	default:
		return false
	}
}

// bsiMaxValue returns the largest value representable at width. Width is
// validated at namespace construction, so 16 and 32 are the only inputs.
func bsiMaxValue(width uint8) uint64 {
	if width >= 64 {
		return ^uint64(0)
	}
	return (uint64(1) << width) - 1
}

// checkBSIValue validates value against the namespace width without truncating.
func checkBSIValue(width uint8, value uint64) error {
	if value > bsiMaxValue(width) {
		return ErrBSIValueRange
	}
	return nil
}

// bsiSlice returns the effective bitmap for one slice, reporting false when
// empty. The bitmap may alias store-owned mmap state; callers must not retain
// it past the reader guard.
func (v *namespaceView) bsiSlice(key NamespaceKey) (roar.Bitmap, bool) {
	p, ok := v.effective(key)
	if !ok || p.isEmpty() {
		return roar.Bitmap{}, false
	}
	return p.bitmap(), true
}

// bsiExists returns the existence bitmap of a BSI namespace.
func (v *namespaceView) bsiExists(ns common.Namespace) (roar.Bitmap, bool) {
	return v.bsiSlice(bsiSliceKey(ns, bsiExistsSlice))
}

// bsiValueAt reconstructs the value stored for id, if any, from the bit slices.
func (v *namespaceView) bsiValueAt(ns common.Namespace, width uint8, id common.ValueID) (uint64, bool) {
	if !v.contains(bsiSliceKey(ns, bsiExistsSlice), id) {
		return 0, false
	}
	var value uint64
	for bit := uint8(0); bit < width; bit++ {
		if v.contains(bsiSliceKey(ns, uint64(bit)+1), id) {
			value |= uint64(1) << bit
		}
	}
	return value, true
}

// bsiEqual returns the ValueIDs whose value equals value exactly.
func (v *namespaceView) bsiEqual(ns common.Namespace, width uint8, value uint64) roar.Bitmap {
	equal, ok := v.bsiExists(ns)
	if !ok {
		return roar.Bitmap{}
	}
	for bit := uint8(0); bit < width; bit++ {
		if equal.IsEmpty() {
			break
		}
		slice, has := v.bsiSlice(bsiSliceKey(ns, uint64(bit)+1))
		if value&(uint64(1)<<bit) != 0 {
			if !has {
				return roar.Bitmap{}
			}
			equal = roar.And(equal, slice)
		} else if has {
			equal = roar.AndNot(equal, slice)
		}
	}
	return equal
}

// bsiLess returns the ValueIDs whose value is strictly less than value.
// Slices are processed most-significant first; equal tracks IDs still equal to
// the processed prefix and less accumulates IDs already known to be smaller.
// Processing stops as soon as equal is empty.
func (v *namespaceView) bsiLess(ns common.Namespace, width uint8, value uint64) roar.Bitmap {
	equal, ok := v.bsiExists(ns)
	if !ok {
		return roar.Bitmap{}
	}
	// Values at or above the width maximum are all smaller. Callers rely on
	// this for the max+1 bound of LessOrEqual and Range.
	if value > bsiMaxValue(width) {
		return equal
	}
	var less roar.Bitmap
	for bit := int(width) - 1; bit >= 0; bit-- {
		if equal.IsEmpty() {
			break
		}
		slice, has := v.bsiSlice(bsiSliceKey(ns, uint64(bit)+1))
		if value&(uint64(1)<<uint(bit)) != 0 {
			// Equal-prefix IDs with a clear bit here are strictly smaller.
			if has {
				zero := roar.AndNot(equal, slice)
				if !zero.IsEmpty() {
					less = roar.Or(less, zero)
				}
				equal = roar.And(equal, slice)
			} else {
				less = roar.Or(less, equal)
				equal = roar.Bitmap{}
			}
		} else if has {
			equal = roar.AndNot(equal, slice)
		}
	}
	return less
}

// bsiRange returns the ValueIDs inside r, restricted to existence.
func (v *namespaceView) bsiRange(ns common.Namespace, width uint8, r ValueRange) roar.Bitmap {
	exists, ok := v.bsiExists(ns)
	if !ok || r.empty() {
		return roar.Bitmap{}
	}
	result := exists
	if r.HasLo {
		if r.LoInclusive {
			lower := v.bsiLess(ns, width, r.Lo)
			result = roar.AndNot(result, lower)
		} else {
			// value > Lo  <=>  NOT (value <= Lo) = NOT (value < Lo+1)
			lower := v.bsiLess(ns, width, r.Lo+1)
			result = roar.AndNot(result, lower)
		}
	}
	if result.IsEmpty() {
		return result
	}
	if r.HasHi {
		var upper roar.Bitmap
		if r.HiInclusive {
			upper = v.bsiLess(ns, width, r.Hi+1)
		} else {
			upper = v.bsiLess(ns, width, r.Hi)
		}
		result = roar.And(result, upper)
	}
	return result
}

// ValueRange is an unsigned, inclusive/exclusive value range over a BSI
// namespace. Open ends and inclusivity are explicit, so empty ranges are
// representable and no bound arithmetic can overflow the query.
type ValueRange struct {
	Lo          uint64
	Hi          uint64
	HasLo       bool
	HasHi       bool
	LoInclusive bool
	HiInclusive bool
}

// ValuesBetween returns the inclusive range [lo, hi].
func ValuesBetween(lo, hi uint64) ValueRange {
	return ValueRange{Lo: lo, Hi: hi, HasLo: true, HasHi: true, LoInclusive: true, HiInclusive: true}
}

// ValuesAtLeast returns [lo, +inf).
func ValuesAtLeast(lo uint64) ValueRange {
	return ValueRange{Lo: lo, HasLo: true, LoInclusive: true}
}

// ValuesAtMost returns [0, hi].
func ValuesAtMost(hi uint64) ValueRange {
	return ValueRange{Hi: hi, HasHi: true, HiInclusive: true}
}

// empty reports whether the range cannot match any value: inverted bounds, or
// equal bounds without full inclusivity.
func (r ValueRange) empty() bool {
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

// contains reports whether value is inside the range.
func (r ValueRange) contains(value uint64) bool {
	if r.HasLo {
		if r.LoInclusive {
			if value < r.Lo {
				return false
			}
		} else if value <= r.Lo {
			return false
		}
	}
	if r.HasHi {
		if r.HiInclusive {
			if value > r.Hi {
				return false
			}
		} else if value >= r.Hi {
			return false
		}
	}
	return true
}

// bsiRead acquires the reader guard and returns the published view for a BSI
// namespace. callers must release the token. ok is false when the index is
// closed.
func (i *Index) bsiRead() (view *namespaceView, token uint32, ok bool) {
	token = i.readers.Acquire()
	view = i.view.Load()
	if i.closed.Load() {
		i.readers.Release(token)
		return nil, 0, false
	}
	return view, token, true
}

// bsiSpec returns the namespace definition, requiring a BSI kind.
func (i *Index) bsiSpec() (NamespaceSpec, error) {
	spec := i.spec()
	if spec.Kind != common.KindBitmapBSI {
		return NamespaceSpec{}, ErrBSINotBSI
	}
	return spec, nil
}

// detachBSI clones a query result when the index is store-owned, because a
// result can alias the mmap mapping that a checkpoint may unmap.
func (i *Index) detachBSI(result roar.Bitmap) roar.Bitmap {
	if i.owner != nil && !result.IsEmpty() {
		return result.Clone()
	}
	return result
}

// Equal returns the ValueIDs whose BSI value equals value. The result is a
// roaring bitmap of common.ValueID, ready for use in the unified search API.
func (i *Index) Equal(value uint64) (roar.Bitmap, error) {
	spec, err := i.bsiSpec()
	if err != nil {
		return roar.Bitmap{}, err
	}
	if err := checkBSIValue(spec.BSIWidth, value); err != nil {
		return roar.Bitmap{}, err
	}
	view, token, ok := i.bsiRead()
	if !ok {
		return roar.Bitmap{}, nil
	}
	result := view.bsiEqual(spec.Namespace, spec.BSIWidth, value)
	i.readers.Release(token)
	return i.detachBSI(result), nil
}

// Less returns the ValueIDs whose BSI value is strictly less than value.
func (i *Index) Less(value uint64) (roar.Bitmap, error) {
	spec, err := i.bsiSpec()
	if err != nil {
		return roar.Bitmap{}, err
	}
	if err := checkBSIValue(spec.BSIWidth, value); err != nil {
		return roar.Bitmap{}, err
	}
	view, token, ok := i.bsiRead()
	if !ok {
		return roar.Bitmap{}, nil
	}
	result := view.bsiLess(spec.Namespace, spec.BSIWidth, value)
	i.readers.Release(token)
	return i.detachBSI(result), nil
}

// LessOrEqual returns the ValueIDs whose BSI value is at most value.
func (i *Index) LessOrEqual(value uint64) (roar.Bitmap, error) {
	spec, err := i.bsiSpec()
	if err != nil {
		return roar.Bitmap{}, err
	}
	if err := checkBSIValue(spec.BSIWidth, value); err != nil {
		return roar.Bitmap{}, err
	}
	view, token, ok := i.bsiRead()
	if !ok {
		return roar.Bitmap{}, nil
	}
	// <= value is < value+1; no stored value can equal the (width-fit) bound.
	result := view.bsiLess(spec.Namespace, spec.BSIWidth, value+1)
	i.readers.Release(token)
	return i.detachBSI(result), nil
}

// Greater returns the ValueIDs whose BSI value is strictly greater than value.
func (i *Index) Greater(value uint64) (roar.Bitmap, error) {
	spec, err := i.bsiSpec()
	if err != nil {
		return roar.Bitmap{}, err
	}
	if err := checkBSIValue(spec.BSIWidth, value); err != nil {
		return roar.Bitmap{}, err
	}
	view, token, ok := i.bsiRead()
	if !ok {
		return roar.Bitmap{}, nil
	}
	exists, has := view.bsiExists(spec.Namespace)
	var result roar.Bitmap
	if has {
		result = roar.AndNot(exists, view.bsiLess(spec.Namespace, spec.BSIWidth, value+1))
	}
	i.readers.Release(token)
	return i.detachBSI(result), nil
}

// GreaterOrEqual returns the ValueIDs whose BSI value is at least value.
func (i *Index) GreaterOrEqual(value uint64) (roar.Bitmap, error) {
	spec, err := i.bsiSpec()
	if err != nil {
		return roar.Bitmap{}, err
	}
	if err := checkBSIValue(spec.BSIWidth, value); err != nil {
		return roar.Bitmap{}, err
	}
	view, token, ok := i.bsiRead()
	if !ok {
		return roar.Bitmap{}, nil
	}
	exists, has := view.bsiExists(spec.Namespace)
	var result roar.Bitmap
	if has {
		result = roar.AndNot(exists, view.bsiLess(spec.Namespace, spec.BSIWidth, value))
	}
	i.readers.Release(token)
	return i.detachBSI(result), nil
}

// NotEqual returns the ValueIDs whose BSI value differs from value.
func (i *Index) NotEqual(value uint64) (roar.Bitmap, error) {
	spec, err := i.bsiSpec()
	if err != nil {
		return roar.Bitmap{}, err
	}
	if err := checkBSIValue(spec.BSIWidth, value); err != nil {
		return roar.Bitmap{}, err
	}
	view, token, ok := i.bsiRead()
	if !ok {
		return roar.Bitmap{}, nil
	}
	exists, has := view.bsiExists(spec.Namespace)
	var result roar.Bitmap
	if has {
		result = roar.AndNot(exists, view.bsiEqual(spec.Namespace, spec.BSIWidth, value))
	}
	i.readers.Release(token)
	return i.detachBSI(result), nil
}

// Range returns the ValueIDs whose BSI value lies in the inclusive range
// [min, max].
func (i *Index) BSIRange(min, max uint64) (roar.Bitmap, error) {
	return i.BSIRangeQuery(ValuesBetween(min, max))
}

// RangeQuery returns the ValueIDs whose BSI value lies inside r, honoring
// inclusive/exclusive ends.
func (i *Index) BSIRangeQuery(r ValueRange) (roar.Bitmap, error) {
	spec, err := i.bsiSpec()
	if err != nil {
		return roar.Bitmap{}, err
	}
	if r.HasLo {
		if err := checkBSIValue(spec.BSIWidth, r.Lo); err != nil {
			return roar.Bitmap{}, err
		}
	}
	if r.HasHi {
		if err := checkBSIValue(spec.BSIWidth, r.Hi); err != nil {
			return roar.Bitmap{}, err
		}
	}
	if r.empty() {
		return roar.Bitmap{}, nil
	}
	view, token, ok := i.bsiRead()
	if !ok {
		return roar.Bitmap{}, nil
	}
	result := view.bsiRange(spec.Namespace, spec.BSIWidth, r)
	i.readers.Release(token)
	return i.detachBSI(result), nil
}

// CountRange returns the number of ValueIDs inside r without appending them.
func (i *Index) BSICountRange(r ValueRange) (uint64, error) {
	result, err := i.BSIRangeQuery(r)
	if err != nil {
		return 0, err
	}
	return result.Cardinality(), nil
}

// bsiCandidateFactor selects between reconstructing candidate values and
// materializing the range bitmap in FilterRange. Reconstructing one candidate
// costs about `width` contains probes; building the bitmap costs about one
// pass over the existence bitmap. Below this factor of (existence cardinality
// / width) the candidate strategy wins. Tuned with BenchmarkBSIFilterRange.
const bsiCandidateFactor = 2

// FilterRange appends the candidates whose BSI value lies in r to dst,
// preserving candidate order and duplicates. dst=candidates[:0] is safe.
// Small candidate sets reconstruct each value; larger sets materialize the
// range bitmap once and probe it.
func (i *Index) BSIFilterRange(dst, candidates []common.ValueID, r ValueRange) ([]common.ValueID, error) {
	spec, err := i.bsiSpec()
	if err != nil {
		return dst, err
	}
	if r.empty() || len(candidates) == 0 {
		return dst, nil
	}
	view, token, ok := i.bsiRead()
	if !ok {
		return dst, nil
	}
	defer i.readers.Release(token)

	exists, has := view.bsiExists(spec.Namespace)
	if !has {
		return dst, nil
	}
	threshold := exists.Cardinality() / uint64(bsiCandidateFactor) / uint64(spec.BSIWidth)
	if uint64(len(candidates)) <= threshold {
		for _, id := range candidates {
			if value, present := view.bsiValueAt(spec.Namespace, spec.BSIWidth, id); present && r.contains(value) {
				dst = append(dst, id)
			}
		}
		return dst, nil
	}
	bit := view.bsiRange(spec.Namespace, spec.BSIWidth, r)
	return filterBitmap(dst, candidates, bit), nil
}

// Set stores value for id, inserting or replacing as needed. ValueID zero is
// reserved; a value wider than the namespace BSI width is rejected rather than
// truncated. Set is durable under DurabilitySync once it returns.
func (i *Index) Set(id common.ValueID, value uint64) error {
	spec, err := i.bsiSpec()
	if err != nil {
		return err
	}
	if id == 0 {
		return ErrInvalidImageID
	}
	if err := checkBSIValue(spec.BSIWidth, value); err != nil {
		return err
	}
	return i.applyBSIOps(bsiOp{id: id, to: uint32(value), flags: bsiFlagUpsert})
}

// Delete removes id's value. Deleting an absent ValueID is a no-op.
func (i *Index) Delete(id common.ValueID) error {
	if _, err := i.bsiSpec(); err != nil {
		return err
	}
	if id == 0 {
		return ErrInvalidImageID
	}
	return i.applyBSIOps(bsiOp{id: id, flags: bsiFlagRemove})
}

// applyBSIOps commits prepared ops through the batch pipeline and waits for
// them to become durable, mirroring the store's single-write path.
func (i *Index) applyBSIOps(ops ...bsiOp) error {
	if i.closed.Load() {
		return ErrClosed
	}
	batch := i.NewBatch()
	defer batch.Close()
	batch.bsi = append(batch.bsi, ops...)
	if err := batch.Apply(); err != nil {
		return err
	}
	return i.Barrier()
}

// Get returns the value stored for id. ok is false when id holds no value.
// ValueID zero is reserved and reported with ErrInvalidImageID.
func (i *Index) Get(id common.ValueID) (value uint64, ok bool, err error) {
	spec, err := i.bsiSpec()
	if err != nil {
		return 0, false, err
	}
	if id == 0 {
		return 0, false, ErrInvalidImageID
	}
	view, token, open := i.bsiRead()
	if !open {
		return 0, false, nil
	}
	value, ok = view.bsiValueAt(spec.Namespace, spec.BSIWidth, id)
	i.readers.Release(token)
	return value, ok, nil
}

// bsiOp is one prepared logical BSI transition. flags carries the presence and
// mode bits.
type bsiOp struct {
	id    common.ValueID
	from  uint32
	to    uint32
	flags uint8
}

const (
	bsiFlagHasFrom uint8 = 1 << iota
	bsiFlagHasTo
	bsiFlagUpsert // set `to` regardless of the current value
	bsiFlagRemove // delete regardless of the current value
)

func (op bsiOp) literal() bool { return op.flags&(bsiFlagUpsert|bsiFlagRemove) == 0 }
func (op bsiOp) hasOld() bool  { return op.literal() && op.flags&bsiFlagHasFrom != 0 }
func (op bsiOp) hasNew() bool {
	return op.flags&bsiFlagRemove == 0 && op.flags&(bsiFlagHasTo|bsiFlagUpsert) != 0
}

// bsiSliceDelta accumulates slice membership changes for one batch. add and
// remove hold unsorted ValueIDs; existence is index zero.
type bsiSliceDelta struct {
	add    []common.ValueID
	remove []common.ValueID
}

// bsiExpand validates op against the current value and folds its slice
// membership changes into deltas. It returns the resolved old/new state so the
// caller can encode a logical WAL record.
func bsiExpand(deltas *[bsiMaxSlices]bsiSliceDelta, op bsiOp, cur uint64, present bool) error {
	var (
		newPresent bool
		newValue   uint64
	)
	switch {
	case op.flags&bsiFlagUpsert != 0:
		newPresent = true
		newValue = uint64(op.to)
	case op.flags&bsiFlagRemove != 0:
		newPresent = false
	default:
		if op.flags&bsiFlagHasFrom != 0 {
			if !present || cur != uint64(op.from) {
				return ErrBSIStale
			}
		} else if op.flags&bsiFlagHasTo != 0 && present {
			return ErrBSIExists
		}
		newPresent = op.flags&bsiFlagHasTo != 0
		newValue = uint64(op.to)
	}

	switch {
	case present && newPresent:
		// Only the bits that changed are touched; a replacement that reuses most
		// of the value never scans all 16 or 32 slices.
		changed := cur ^ newValue
		for changed != 0 {
			bit := bits.TrailingZeros64(changed)
			slice := bsiBitSlice0 + uint64(bit)
			if newValue&(uint64(1)<<uint(bit)) != 0 {
				deltas[slice].add = append(deltas[slice].add, op.id)
			} else {
				deltas[slice].remove = append(deltas[slice].remove, op.id)
			}
			changed &= changed - 1
		}
	case present && !newPresent:
		for cur != 0 {
			slice := bsiBitSlice0 + uint64(bits.TrailingZeros64(cur))
			deltas[slice].remove = append(deltas[slice].remove, op.id)
			cur &= cur - 1
		}
		deltas[bsiExistsSlice].remove = append(deltas[bsiExistsSlice].remove, op.id)
	case !present && newPresent:
		for newValue != 0 {
			slice := bsiBitSlice0 + uint64(bits.TrailingZeros64(newValue))
			deltas[slice].add = append(deltas[slice].add, op.id)
			newValue &= newValue - 1
		}
		deltas[bsiExistsSlice].add = append(deltas[bsiExistsSlice].add, op.id)
	}
	return nil
}

// bsiChanges folds a batch's slice deltas into staged changes, one per
// non-empty slice. The returned changes carry nil canonical (no dictionary) and
// a resolved key so the generic delta composer can consume them.
func bsiChanges(spec NamespaceSpec, deltas *[bsiMaxSlices]bsiSliceDelta) []stagedChange {
	changes := make([]stagedChange, 0, bsiMaxSlices)
	for slice := range deltas {
		d := &deltas[slice]
		if len(d.add) == 0 && len(d.remove) == 0 {
			continue
		}
		key := bsiSliceKey(spec.Namespace, uint64(slice))
		changes = append(changes, stagedChange{
			base:   key,
			key:    key,
			next:   -1,
			add:    d.add,
			remove: d.remove,
		})
	}
	return changes
}
