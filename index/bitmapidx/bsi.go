package bitmapidx

import (
	"math/bits"
	"sync"

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

// bsiSlices holds the resolved postings for one BSI namespace: existence and
// each value bit. A query resolves them once, so it never repeats the generic
// posting lookup (binary search plus delta folding) for the same slice, and it
// reuses the resolved references across every bit operation.
type bsiSlices struct {
	exists    roar.Bitmap
	hasExists bool
	bits      [32]roar.Bitmap
	hasBit    [32]bool
	width     uint8
}

// resolveBSISlices fills out with the namespace's effective slice postings.
// Absent slices stay zero, which the comparison kernels treat as an empty set.
func (v *namespaceView) resolveBSISlices(ns common.Namespace, width uint8, out *bsiSlices) {
	out.width = width
	out.hasExists = false
	out.exists = roar.Bitmap{}
	for bit := range out.hasBit {
		out.hasBit[bit] = false
		out.bits[bit] = roar.Bitmap{}
	}
	if b, ok := v.bsiSlice(bsiSliceKey(ns, bsiExistsSlice)); ok {
		out.exists, out.hasExists = b, true
	}
	for bit := uint8(0); bit < width; bit++ {
		if b, ok := v.bsiSlice(bsiSliceKey(ns, uint64(bit)+1)); ok {
			out.bits[bit], out.hasBit[bit] = b, true
		}
	}
}

// bitAt returns id's value bit, treating an absent slice as all zeros.
func (s *bsiSlices) bitAt(id common.ValueID, bit uint8) bool {
	if bit >= s.width || !s.hasBit[bit] {
		return false
	}
	return s.bits[bit].Contains(id)
}

// bsiScratch is one reusable query workspace: recycled accumulators plus the
// resolved slice view. Accumulators recycle a single owned result buffer, so
// repeated bit operations allocate only on growth instead of once per bit.
// eqLo/lessLo drive a lower bound, eqHi/lessHi an upper bound; a range query
// evaluates both in one traversal.
type bsiScratch struct {
	slices bsiSlices
	res    *roar.MutableBitmap
	eqLo   *roar.MutableBitmap
	lessLo *roar.MutableBitmap
	eqHi   *roar.MutableBitmap
	lessHi *roar.MutableBitmap
	zero   *roar.MutableBitmap
}

// bsiScratchMaxRetained bounds a pooled workspace so one huge query cannot pin
// megabytes process-wide.
const bsiScratchMaxRetained = 1 << 20

var bsiScratchPool = sync.Pool{New: func() any {
	return &bsiScratch{
		res:    roar.NewMutableBitmap(),
		eqLo:   roar.NewMutableBitmap(),
		lessLo: roar.NewMutableBitmap(),
		eqHi:   roar.NewMutableBitmap(),
		lessHi: roar.NewMutableBitmap(),
		zero:   roar.NewMutableBitmap(),
	}
}}

func acquireBSIScratch() *bsiScratch {
	s := bsiScratchPool.Get().(*bsiScratch)
	s.res.Reset()
	s.eqLo.Reset()
	s.lessLo.Reset()
	s.eqHi.Reset()
	s.lessHi.Reset()
	s.zero.Reset()
	return s
}

func releaseBSIScratch(s *bsiScratch) {
	retained := s.res.RetainedBytes() + s.eqLo.RetainedBytes() + s.lessLo.RetainedBytes() +
		s.eqHi.RetainedBytes() + s.lessHi.RetainedBytes() + s.zero.RetainedBytes()
	if retained > bsiScratchMaxRetained {
		return
	}
	bsiScratchPool.Put(s)
}

// ownBSI detaches a scratch-aliasing result: empty stays empty, otherwise a
// clone owned by the caller.
func ownBSI(b roar.Bitmap) roar.Bitmap {
	if b.IsEmpty() {
		return roar.Bitmap{}
	}
	return b.Clone()
}

// bsiEqualInto fills m with the IDs whose value equals value. The result aliases
// m and is valid until m is reused.
func bsiEqualInto(s *bsiSlices, value uint64, m *roar.MutableBitmap) roar.Bitmap {
	m.Reset()
	if !s.hasExists {
		return roar.Bitmap{}
	}
	m.Or(s.exists)
	for bit := uint8(0); bit < s.width; bit++ {
		if value&(uint64(1)<<bit) != 0 {
			if !s.hasBit[bit] {
				return roar.Bitmap{}
			}
			m.And(s.bits[bit])
		} else if s.hasBit[bit] {
			m.AndNot(s.bits[bit])
		}
		if m.Freeze().IsEmpty() {
			break
		}
	}
	return m.Freeze()
}

// bsiLessInto fills less with the IDs whose value is strictly less than value.
// Slices are processed most-significant first; equal tracks IDs still equal to
// the processed prefix and less accumulates IDs already known to be smaller.
// Processing stops as soon as equal is empty. The result aliases less.
func bsiLessInto(s *bsiSlices, value uint64, equal, less, zero *roar.MutableBitmap) roar.Bitmap {
	equal.Reset()
	less.Reset()
	zero.Reset()
	if !s.hasExists {
		return roar.Bitmap{}
	}
	equal.Or(s.exists)
	// Values at or above the width maximum are all smaller. Callers rely on
	// this for the max+1 bound of LessOrEqual and Range.
	if value > bsiMaxValue(s.width) {
		return equal.Freeze()
	}
	for bit := int(s.width) - 1; bit >= 0; bit-- {
		if equal.Freeze().IsEmpty() {
			break
		}
		if value&(uint64(1)<<uint(bit)) != 0 {
			// Equal-prefix IDs with a clear bit here are strictly smaller.
			if s.hasBit[bit] {
				zero.Or(equal.Freeze())
				zero.AndNot(s.bits[bit])
				less.Or(zero.Freeze())
				equal.And(s.bits[bit])
			} else {
				less.Or(equal.Freeze())
				equal.Reset()
			}
		} else if s.hasBit[bit] {
			equal.AndNot(s.bits[bit])
		}
	}
	return less.Freeze()
}

// bsiRangeInto fills res with the IDs whose value lies in r. The result aliases
// res.
//
// It evaluates both bounds in one most-significant-first traversal: eqLo/lessLo
// track {value < loBound} and eqHi/lessHi track {value < hiBound}, and the
// result is exists minus both. A lower bound of 0 and an upper bound past the
// width are unconditional, so the traversal is skipped entirely for the full
// domain. Folding both bounds into one pass measured ~40% faster than two
// separate comparisons plus an intersection, and ~50x faster for the full
// domain (see BenchmarkBSIRangeCombined).
func bsiRangeInto(s *bsiSlices, r ValueRange, sc *bsiScratch) roar.Bitmap {
	sc.res.Reset()
	if !s.hasExists || r.empty() {
		return roar.Bitmap{}
	}
	loBound := r.Lo
	if r.HasLo && !r.LoInclusive {
		loBound++
	}
	hiBound := r.Hi
	if r.HasHi && r.HiInclusive {
		hiBound++
	}
	loActive := r.HasLo && loBound != 0
	hiActive := r.HasHi && hiBound <= bsiMaxValue(s.width)
	if !loActive && !hiActive {
		sc.res.Or(s.exists)
		return sc.res.Freeze()
	}
	sc.eqLo.Reset()
	sc.lessLo.Reset()
	sc.eqHi.Reset()
	sc.lessHi.Reset()
	if loActive {
		sc.eqLo.Or(s.exists)
	}
	if hiActive {
		sc.eqHi.Or(s.exists)
	}
	for bit := int(s.width) - 1; bit >= 0; bit-- {
		has := s.hasBit[bit]
		var slice roar.Bitmap
		if has {
			slice = s.bits[bit]
		}
		if loActive && !sc.eqLo.Freeze().IsEmpty() {
			if loBound&(uint64(1)<<uint(bit)) != 0 {
				if has {
					sc.zero.Reset()
					sc.zero.Or(sc.eqLo.Freeze())
					sc.zero.AndNot(slice)
					sc.lessLo.Or(sc.zero.Freeze())
					sc.eqLo.And(slice)
				} else {
					sc.lessLo.Or(sc.eqLo.Freeze())
					sc.eqLo.Reset()
				}
			} else if has {
				sc.eqLo.AndNot(slice)
			}
		}
		if hiActive && !sc.eqHi.Freeze().IsEmpty() {
			if hiBound&(uint64(1)<<uint(bit)) != 0 {
				if has {
					sc.zero.Reset()
					sc.zero.Or(sc.eqHi.Freeze())
					sc.zero.AndNot(slice)
					sc.lessHi.Or(sc.zero.Freeze())
					sc.eqHi.And(slice)
				} else {
					sc.lessHi.Or(sc.eqHi.Freeze())
					sc.eqHi.Reset()
				}
			} else if has {
				sc.eqHi.AndNot(slice)
			}
		}
	}
	res := sc.res
	res.Or(s.exists)
	if loActive {
		res.AndNot(sc.lessLo.Freeze())
		if res.Freeze().IsEmpty() {
			return roar.Bitmap{}
		}
	}
	if hiActive {
		res.And(sc.lessHi.Freeze())
	}
	return res.Freeze()
}

// bsiCandidateInRange reports whether one candidate's value lies in r without
// materializing a range bitmap or reconstructing the full value: each bound is
// tested most-significant-bit first and stops at the first differing bit.
func bsiCandidateInRange(s *bsiSlices, id common.ValueID, r ValueRange) bool {
	if !s.hasExists || !s.exists.Contains(id) {
		return false
	}
	if r.HasLo && !bsiCandidateAtLeast(s, id, r.Lo, r.LoInclusive) {
		return false
	}
	if r.HasHi && !bsiCandidateAtMost(s, id, r.Hi, r.HiInclusive) {
		return false
	}
	return true
}

// bsiCandidateAtLeast reports value >= bound (or > bound when !inclusive).
func bsiCandidateAtLeast(s *bsiSlices, id common.ValueID, bound uint64, inclusive bool) bool {
	for bit := int(s.width) - 1; bit >= 0; bit-- {
		c := s.bitAt(id, uint8(bit))
		b := bound&(uint64(1)<<uint(bit)) != 0
		if c != b {
			return c
		}
	}
	return inclusive
}

// bsiCandidateAtMost reports value <= bound (or < bound when !inclusive).
func bsiCandidateAtMost(s *bsiSlices, id common.ValueID, bound uint64, inclusive bool) bool {
	for bit := int(s.width) - 1; bit >= 0; bit-- {
		c := s.bitAt(id, uint8(bit))
		b := bound&(uint64(1)<<uint(bit)) != 0
		if c != b {
			return !c
		}
	}
	return inclusive
}

// bsiBitAt reports id's value bit by probing the view directly, touching only
// that bit's slice. It resolves no slice directory and allocates nothing.
func (v *namespaceView) bsiBitAt(ns common.Namespace, bit uint8, id common.ValueID) bool {
	return v.contains(bsiSliceKey(ns, uint64(bit)+1), id)
}

// bsiCompareToBound compares id's value against bound, returning -1, 0, or 1.
// It reads bits most-significant first and stops at the first difference.
func (v *namespaceView) bsiCompareToBound(ns common.Namespace, width uint8, id common.ValueID, bound uint64) int {
	for bit := int(width) - 1; bit >= 0; bit-- {
		c := v.bsiBitAt(ns, uint8(bit), id)
		b := bound&(uint64(1)<<uint(bit)) != 0
		if c != b {
			if c {
				return 1
			}
			return -1
		}
	}
	return 0
}

// bsiCandidateInRangeLazy tests one candidate against r by reading only the
// bits a comparison needs, most-significant first, stopping at the first
// difference. It is the small-candidate path: no slice resolution, no scratch,
// no allocation when dst has capacity.
func (v *namespaceView) bsiCandidateInRangeLazy(ns common.Namespace, width uint8, id common.ValueID, r ValueRange) bool {
	if !v.contains(bsiSliceKey(ns, bsiExistsSlice), id) {
		return false
	}
	if r.HasLo {
		if cmp := v.bsiCompareToBound(ns, width, id, r.Lo); cmp < 0 || (cmp == 0 && !r.LoInclusive) {
			return false
		}
	}
	if r.HasHi {
		if cmp := v.bsiCompareToBound(ns, width, id, r.Hi); cmp > 0 || (cmp == 0 && !r.HiInclusive) {
			return false
		}
	}
	return true
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

// bsiAcquire resolves the namespace's slice postings into pooled scratch. The
// caller must release the returned workspace.
func (i *Index) bsiAcquire(spec NamespaceSpec, view *namespaceView) *bsiScratch {
	sc := acquireBSIScratch()
	view.resolveBSISlices(spec.Namespace, spec.BSIWidth, &sc.slices)
	return sc
}

// bsiValidateSpec returns the BSI namespace definition after checking that
// value fits its width.
func (i *Index) bsiValidateSpec(value uint64) (NamespaceSpec, error) {
	spec, err := i.bsiSpec()
	if err != nil {
		return NamespaceSpec{}, err
	}
	if err := checkBSIValue(spec.BSIWidth, value); err != nil {
		return NamespaceSpec{}, err
	}
	return spec, nil
}

// bsiLessQuery returns ids whose value is strictly less than bound. The bound
// is not width-checked: callers pass a validated value or value+1 <= max+1.
func (i *Index) bsiLessQuery(spec NamespaceSpec, bound uint64) (roar.Bitmap, error) {
	view, token, ok := i.bsiRead()
	if !ok {
		return roar.Bitmap{}, nil
	}
	sc := i.bsiAcquire(spec, view)
	result := ownBSI(bsiLessInto(&sc.slices, bound, sc.eqLo, sc.lessLo, sc.zero))
	releaseBSIScratch(sc)
	i.readers.Release(token)
	return result, nil
}

// bsiExcludeLess returns exists minus the set of ids with value < bound.
func (i *Index) bsiExcludeLess(spec NamespaceSpec, bound uint64) (roar.Bitmap, error) {
	view, token, ok := i.bsiRead()
	if !ok {
		return roar.Bitmap{}, nil
	}
	sc := i.bsiAcquire(spec, view)
	sc.res.Reset()
	if sc.slices.hasExists {
		sc.res.Or(sc.slices.exists)
		sc.res.AndNot(bsiLessInto(&sc.slices, bound, sc.eqLo, sc.lessLo, sc.zero))
	}
	result := ownBSI(sc.res.Freeze())
	releaseBSIScratch(sc)
	i.readers.Release(token)
	return result, nil
}

// bsiExcludeEqual returns exists minus the set of ids with value == value.
func (i *Index) bsiExcludeEqual(spec NamespaceSpec, value uint64) (roar.Bitmap, error) {
	view, token, ok := i.bsiRead()
	if !ok {
		return roar.Bitmap{}, nil
	}
	sc := i.bsiAcquire(spec, view)
	sc.res.Reset()
	if sc.slices.hasExists {
		sc.res.Or(sc.slices.exists)
		sc.res.AndNot(bsiEqualInto(&sc.slices, value, sc.eqLo))
	}
	result := ownBSI(sc.res.Freeze())
	releaseBSIScratch(sc)
	i.readers.Release(token)
	return result, nil
}

// Equal returns the ValueIDs whose BSI value equals value. The result is a
// roaring bitmap of common.ValueID, ready for use in the unified search API.
func (i *Index) Equal(value uint64) (roar.Bitmap, error) {
	spec, err := i.bsiValidateSpec(value)
	if err != nil {
		return roar.Bitmap{}, err
	}
	view, token, ok := i.bsiRead()
	if !ok {
		return roar.Bitmap{}, nil
	}
	sc := i.bsiAcquire(spec, view)
	result := ownBSI(bsiEqualInto(&sc.slices, value, sc.eqLo))
	releaseBSIScratch(sc)
	i.readers.Release(token)
	return result, nil
}

// Less returns the ValueIDs whose BSI value is strictly less than value.
func (i *Index) Less(value uint64) (roar.Bitmap, error) {
	spec, err := i.bsiValidateSpec(value)
	if err != nil {
		return roar.Bitmap{}, err
	}
	return i.bsiLessQuery(spec, value)
}

// LessOrEqual returns the ValueIDs whose BSI value is at most value.
func (i *Index) LessOrEqual(value uint64) (roar.Bitmap, error) {
	spec, err := i.bsiValidateSpec(value)
	if err != nil {
		return roar.Bitmap{}, err
	}
	// <= value is < value+1; no stored value can equal the (width-fit) bound.
	return i.bsiLessQuery(spec, value+1)
}

// Greater returns the ValueIDs whose BSI value is strictly greater than value.
func (i *Index) Greater(value uint64) (roar.Bitmap, error) {
	spec, err := i.bsiValidateSpec(value)
	if err != nil {
		return roar.Bitmap{}, err
	}
	return i.bsiExcludeLess(spec, value+1)
}

// GreaterOrEqual returns the ValueIDs whose BSI value is at least value.
func (i *Index) GreaterOrEqual(value uint64) (roar.Bitmap, error) {
	spec, err := i.bsiValidateSpec(value)
	if err != nil {
		return roar.Bitmap{}, err
	}
	return i.bsiExcludeLess(spec, value)
}

// NotEqual returns the ValueIDs whose BSI value differs from value.
func (i *Index) NotEqual(value uint64) (roar.Bitmap, error) {
	spec, err := i.bsiValidateSpec(value)
	if err != nil {
		return roar.Bitmap{}, err
	}
	return i.bsiExcludeEqual(spec, value)
}

// Range returns the ValueIDs whose BSI value lies in the inclusive range
// [min, max].
func (i *Index) BSIRange(min, max uint64) (roar.Bitmap, error) {
	return i.BSIRangeQuery(ValuesBetween(min, max))
}

// bsiRangeSpec returns the BSI definition after validating the range bounds
// against the namespace width.
func (i *Index) bsiRangeSpec(r ValueRange) (NamespaceSpec, error) {
	spec, err := i.bsiSpec()
	if err != nil {
		return NamespaceSpec{}, err
	}
	if r.HasLo {
		if err := checkBSIValue(spec.BSIWidth, r.Lo); err != nil {
			return NamespaceSpec{}, err
		}
	}
	if r.HasHi {
		if err := checkBSIValue(spec.BSIWidth, r.Hi); err != nil {
			return NamespaceSpec{}, err
		}
	}
	return spec, nil
}

// bsiRangeUnrestricted reports whether r accepts every stored value, so a query
// reduces to the existence set and needs no bit slices resolved. A lower bound
// of 0 and an upper bound past the width are unconditional.
func bsiRangeUnrestricted(width uint8, r ValueRange) bool {
	if r.HasLo {
		lo := r.Lo
		if !r.LoInclusive {
			lo++
		}
		if lo != 0 {
			return false
		}
	}
	if r.HasHi {
		hi := r.Hi
		if r.HiInclusive {
			hi++
		}
		if hi <= bsiMaxValue(width) {
			return false
		}
	}
	return true
}

// BSIRangeQuery returns the ValueIDs whose BSI value lies inside r, honoring
// inclusive/exclusive ends.
func (i *Index) BSIRangeQuery(r ValueRange) (roar.Bitmap, error) {
	spec, err := i.bsiRangeSpec(r)
	if err != nil || r.empty() {
		return roar.Bitmap{}, err
	}
	view, token, ok := i.bsiRead()
	if !ok {
		return roar.Bitmap{}, nil
	}
	// An unrestricted range is the existence set: skip slice resolution.
	if bsiRangeUnrestricted(spec.BSIWidth, r) {
		exists, has := view.bsiExists(spec.Namespace)
		i.readers.Release(token)
		if !has {
			return roar.Bitmap{}, nil
		}
		return ownBSI(exists), nil
	}
	sc := i.bsiAcquire(spec, view)
	result := ownBSI(bsiRangeInto(&sc.slices, r, sc))
	releaseBSIScratch(sc)
	i.readers.Release(token)
	return result, nil
}

// BSICountRange returns the number of ValueIDs inside r without appending them
// or materializing a caller-owned result bitmap.
func (i *Index) BSICountRange(r ValueRange) (uint64, error) {
	spec, err := i.bsiRangeSpec(r)
	if err != nil || r.empty() {
		return 0, err
	}
	view, token, ok := i.bsiRead()
	if !ok {
		return 0, nil
	}
	// An unrestricted range is the existence cardinality: no bit slices needed.
	if bsiRangeUnrestricted(spec.BSIWidth, r) {
		exists, has := view.bsiExists(spec.Namespace)
		i.readers.Release(token)
		if !has {
			return 0, nil
		}
		return exists.Cardinality(), nil
	}
	sc := i.bsiAcquire(spec, view)
	count := bsiRangeInto(&sc.slices, r, sc).Cardinality()
	releaseBSIScratch(sc)
	i.readers.Release(token)
	return count, nil
}

// bsiCandidateFactor selects between reconstructing candidate values and
// materializing the range bitmap in BSIFilterRange. Reconstructing one candidate
// costs about `width` contains probes; building the bitmap costs about one
// pass over the existence bitmap. Below this factor of (existence cardinality
// / width) the candidate strategy wins. Tuned with BenchmarkBSIFilterRange.
const bsiCandidateFactor = 2

// bsiLazyCandidateThreshold bounds the candidate count for which
// BSIFilterRange probes the view directly instead of resolving the slice
// directory. At or below it, per-bit view probes (with early termination) cost
// less than resolving 17 or 33 slices; above it the resolved directory pays
// off. Tuned with BenchmarkBSIFilterRangeSmall: lazy wins at 1-4 candidates,
// resolving wins from 8 up.
const bsiLazyCandidateThreshold = 4

// BSIFilterRange appends the candidates whose BSI value lies in r to dst,
// preserving candidate order and duplicates. dst=candidates[:0] is safe. Small
// candidate sets probe the view directly; larger sets resolve the slice
// directory and either probe it or materialize the range bitmap once.
func (i *Index) BSIFilterRange(dst, candidates []common.ValueID, r ValueRange) ([]common.ValueID, error) {
	spec, err := i.bsiRangeSpec(r)
	if err != nil || r.empty() || len(candidates) == 0 {
		return dst, err
	}
	view, token, ok := i.bsiRead()
	if !ok {
		return dst, nil
	}
	defer i.readers.Release(token)
	// Tiny candidate sets probe only the bits the comparisons need, with no
	// slice resolution and no scratch; larger sets use the resolved directory.
	if len(candidates) <= bsiLazyCandidateThreshold {
		for _, id := range candidates {
			if view.bsiCandidateInRangeLazy(spec.Namespace, spec.BSIWidth, id, r) {
				dst = append(dst, id)
			}
		}
		return dst, nil
	}
	// An unrestricted range is existence membership: skip slice resolution.
	if bsiRangeUnrestricted(spec.BSIWidth, r) {
		exists, has := view.bsiExists(spec.Namespace)
		if !has {
			return dst, nil
		}
		return filterBitmap(dst, candidates, exists), nil
	}
	sc := i.bsiAcquire(spec, view)
	defer releaseBSIScratch(sc)
	s := &sc.slices
	if !s.hasExists {
		return dst, nil
	}
	threshold := s.exists.Cardinality() / uint64(bsiCandidateFactor) / uint64(spec.BSIWidth)
	if uint64(len(candidates)) <= threshold {
		for _, id := range candidates {
			if bsiCandidateInRange(s, id, r) {
				dst = append(dst, id)
			}
		}
		return dst, nil
	}
	bit := bsiRangeInto(s, r, sc)
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
