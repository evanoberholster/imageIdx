package roar

import "slices"

// inlineDeltaCapacity is the number of pending additions and removals each
// DeltaBuilder stores without allocating. Most mutations are small; the
// buffers fall back to heap slices only when a batch exceeds this size.
const inlineDeltaCapacity = 64

// DeltaBuilder is a writer-owned builder that accumulates raw additions and
// removals against an immutable Bitmap and materializes the result on demand.
// It backs small, high-frequency corrections such as the delta layers of a
// copy-on-write index, where rebuilding an entire bitmap on every change
// would be wasteful.
//
// Unlike MutableBitmap, which applies whole-set operations (Or, And, AndNot)
// directly, DeltaBuilder defers raw ID additions and removals and folds them
// once at Freeze.
//
// Pending additions are applied before pending removals, so a removal wins
// when the same ID is both added and removed within one batch. This matches
// the "removal wins" convention used by bitmapidx mutation handling.
//
// Set operations (Or, And, AndNot) flush pending additions/removals first and
// then combine with the current bitmap, preserving sequential semantics.
//
// Whole-set combinations reuse an embedded result Builder, mirroring
// MutableBitmap's scratch policy. Final bitmaps are always freshly assembled
// so Freeze stays independent of later reuse.
//
// DeltaBuilder is not safe for concurrent use. Do not copy a DeltaBuilder
// after first use: pending slices and Builder scratch may alias its own
// inline buffers.
type DeltaBuilder struct {
	bitmap    Bitmap
	adds      []uint64
	removes   []uint64
	addBuf    [inlineDeltaCapacity]uint64
	removeBuf [inlineDeltaCapacity]uint64
	fold      Builder
	dirty     bool
}

// NewDeltaBuilder returns an empty DeltaBuilder.
func NewDeltaBuilder() *DeltaBuilder {
	m := &DeltaBuilder{}
	m.adds = m.addBuf[:0]
	m.removes = m.removeBuf[:0]
	m.fold.scratchValues = m.fold.valuesBuf[:0]
	return m
}

// NewDeltaBuilderFrom returns a DeltaBuilder seeded with b. b is retained
// by reference and never mutated; the builder only replaces it.
func NewDeltaBuilderFrom(b Bitmap) *DeltaBuilder {
	m := &DeltaBuilder{bitmap: b}
	m.adds = m.addBuf[:0]
	m.removes = m.removeBuf[:0]
	m.fold.scratchValues = m.fold.valuesBuf[:0]
	return m
}

// AddMany stages additions of ids. They are merged into the bitmap lazily at
// the next set operation or Freeze.
func (m *DeltaBuilder) AddMany(ids []uint64) {
	if len(ids) == 0 {
		return
	}
	m.adds = append(m.adds, ids...)
	m.dirty = true
}

// Remove stages a removal of id. InvalidImageID is ignored.
func (m *DeltaBuilder) Remove(id uint64) {
	if id == InvalidImageID {
		return
	}
	m.removes = append(m.removes, id)
	m.dirty = true
}

// Or unions b into the builder's current bitmap.
func (m *DeltaBuilder) Or(b Bitmap) {
	m.foldSet(b, m.bitmap.KeyCount()+b.KeyCount(), m.bitmap.LenInBytes()+b.LenInBytes(), OrInto)
}

// And intersects b with the builder's current bitmap.
func (m *DeltaBuilder) And(b Bitmap) {
	m.foldSet(b, min(m.bitmap.KeyCount(), b.KeyCount()), min(m.bitmap.LenInBytes(), b.LenInBytes()), AndInto)
}

// AndNot subtracts b from the builder's current bitmap.
func (m *DeltaBuilder) AndNot(b Bitmap) {
	m.foldSet(b, m.bitmap.KeyCount(), m.bitmap.LenInBytes(), AndNotInto)
}

// foldSet applies one whole-set operation with the shared result Builder. The
// result is freshly assembled, so a bitmap returned by Freeze stays
// independent of later reuse.
func (m *DeltaBuilder) foldSet(operand Bitmap, keyCount, dataBytes int, op func(dst *Builder, a, b Bitmap)) {
	m.flush()
	m.fold.prepareResult(keyCount, dataBytes)
	op(&m.fold, m.bitmap, operand)
	m.bitmap = Bitmap{data: m.fold.Compact()}
}

// Reset clears the builder and returns its pending slices to the inline
// buffers.
func (m *DeltaBuilder) Reset() {
	m.bitmap = Bitmap{}
	m.adds = m.addBuf[:0]
	m.removes = m.removeBuf[:0]
	m.fold.Reset()
	m.dirty = false
}

// Freeze materializes pending additions/removals and returns the immutable
// Bitmap. The builder remains usable afterward; the returned Bitmap is
// independent of it.
func (m *DeltaBuilder) Freeze() Bitmap {
	m.flush()
	return m.bitmap
}

// flush merges pending additions (then removals) into the current bitmap in a
// single pass, materializing no intermediate union bitmap.
func (m *DeltaBuilder) flush() {
	if !m.dirty {
		return
	}
	m.dirty = false
	add := bitmapFromUnique(m.adds)
	remove := bitmapFromUnique(m.removes)
	m.adds = m.adds[:0]
	m.removes = m.removes[:0]
	if add.IsEmpty() && remove.IsEmpty() {
		return
	}
	m.fold.prepareResult(m.bitmap.KeyCount()+add.KeyCount(), m.bitmap.LenInBytes()+add.LenInBytes())
	applyInto(&m.fold, m.bitmap, add, remove)
	m.bitmap = Bitmap{data: m.fold.Compact()}
}

// bitmapFromUnique sorts ids in place, removes duplicates and the reserved
// zero ID, and returns a Bitmap over the remaining strictly increasing IDs.
func bitmapFromUnique(ids []uint64) Bitmap {
	slices.Sort(ids)
	ids = slices.Compact(ids)
	if len(ids) != 0 && ids[0] == InvalidImageID {
		ids = ids[1:]
	}
	if len(ids) == 0 {
		return Bitmap{}
	}
	bitmap, err := FromSorted(ids)
	if err != nil {
		// Unreachable: ids are sorted, unique, and free of zero.
		return Bitmap{}
	}
	return bitmap
}
