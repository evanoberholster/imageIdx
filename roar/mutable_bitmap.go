package roar

// MutableBitmap is a single-owner roaring bitmap that shares the exact
// serialized format of Bitmap. It supports in-place whole-set operations
// (Or, And, AndNot) against immutable Bitmaps and seeds itself from a
// Bitmap or a sorted ID list.
//
// Unlike DeltaBuilder, which defers raw ID additions/removals, MutableBitmap
// applies whole-set operations immediately and always keeps its backing in
// the canonical format, so Freeze is zero-copy.
//
// A MutableBitmap is not safe for concurrent reads during mutation; use it to
// build a result that will be shared only after Freeze. Confine each
// MutableBitmap to one goroutine: it has no locks, atomics, or reader
// tracking. Do not copy a MutableBitmap after first use: its Builder scratch
// may alias its own inline buffer.
type MutableBitmap struct {
	bitmap  Bitmap
	builder Builder
	// owned reports whether bitmap's buffer is exclusively owned by this
	// MutableBitmap and can be recycled as the destination of the next op. A
	// seeded Bitmap is shared and must not be overwritten.
	owned bool
}

// NewMutableBitmap returns an empty MutableBitmap.
func NewMutableBitmap() *MutableBitmap {
	m := &MutableBitmap{}
	m.builder.scratchValues = m.builder.valuesBuf[:0]
	return m
}

// NewMutableBitmapFrom returns a MutableBitmap seeded with b. The seed is
// retained by reference, not copied.
func NewMutableBitmapFrom(b Bitmap) *MutableBitmap {
	m := NewMutableBitmap()
	m.bitmap = b
	return m
}

// FromSortedList replaces the bitmap's content with a strictly increasing
// list of ImageIDs. It returns the same errors as FromSorted (zero or
// unsorted IDs).
func (m *MutableBitmap) FromSortedList(ids []uint64) error {
	bitmap, err := FromSorted(ids)
	if err != nil {
		return err
	}
	m.bitmap = bitmap
	m.owned = true
	return nil
}

// Or unions b into the bitmap in place.
func (m *MutableBitmap) Or(b Bitmap) {
	m.builder.prepareResult(m.bitmap.KeyCount()+b.KeyCount(), m.bitmap.LenInBytes()+b.LenInBytes())
	OrInto(&m.builder, m.bitmap, b)
	m.commit()
}

// And intersects b with the bitmap in place.
func (m *MutableBitmap) And(b Bitmap) {
	m.builder.prepareResult(min(m.bitmap.KeyCount(), b.KeyCount()), min(m.bitmap.LenInBytes(), b.LenInBytes()))
	AndInto(&m.builder, m.bitmap, b)
	m.commit()
}

// AndNot subtracts b from the bitmap in place.
func (m *MutableBitmap) AndNot(b Bitmap) {
	m.builder.prepareResult(m.bitmap.KeyCount(), m.bitmap.LenInBytes())
	AndNotInto(&m.builder, m.bitmap, b)
	m.commit()
}

// commit materializes the builder's result into m.bitmap, recycling m.bitmap's
// buffer only when the MutableBitmap owns it (a seeded Bitmap may be shared).
func (m *MutableBitmap) commit() {
	m.bitmap = Bitmap{data: m.builder.finishResult(m.bitmap.Bytes(), m.owned)}
	m.owned = true
}

// Freeze returns the current bitmap as an immutable Bitmap without copying.
func (m *MutableBitmap) Freeze() Bitmap {
	return m.bitmap
}
