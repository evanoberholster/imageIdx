package roar

// MutableBitmap is a single-owner roaring bitmap that shares the exact
// serialized format of Bitmap. It supports in-place whole-set operations
// (Or, And, AndNot) against immutable Bitmaps and seeds itself from a
// Bitmap or a sorted ID list.
//
// The mutable bitmap keeps its backing in the canonical format at all times,
// so Freeze is zero-copy: it simply returns the current buffer as an
// immutable Bitmap. The receiver should not be used after Freeze.
//
// A MutableBitmap is not safe for concurrent reads during mutation; use it to
// build a result that will be shared only after Freeze.
type MutableBitmap struct {
	bitmap  Bitmap
	builder Builder
	owned   bool
}

// NewMutableBitmap returns an empty MutableBitmap.
func NewMutableBitmap() *MutableBitmap {
	return &MutableBitmap{builder: *NewBuilder(0)}
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
	m.builder.Reset()
	m.builder.reserveKeys(m.bitmap.KeyCount() + b.KeyCount())
	m.builder.reserveData(m.bitmap.LenInBytes() + b.LenInBytes())
	OrInto(&m.builder, m.bitmap, b)
	m.commit()
}

// And intersects b with the bitmap in place.
func (m *MutableBitmap) And(b Bitmap) {
	m.builder.Reset()
	m.builder.reserveKeys(min(m.bitmap.KeyCount(), b.KeyCount()))
	m.builder.reserveData(min(m.bitmap.LenInBytes(), b.LenInBytes()))
	AndInto(&m.builder, m.bitmap, b)
	m.commit()
}

// AndNot subtracts b from the bitmap in place.
func (m *MutableBitmap) AndNot(b Bitmap) {
	m.builder.Reset()
	m.builder.reserveKeys(m.bitmap.KeyCount())
	m.builder.reserveData(m.bitmap.LenInBytes())
	AndNotInto(&m.builder, m.bitmap, b)
	m.commit()
}

// commit materializes the builder's result into m.bitmap, recycling m.bitmap's
// buffer only when the MutableBitmap owns it (a seeded Bitmap may be shared).
func (m *MutableBitmap) commit() {
	if m.owned {
		m.bitmap = Bitmap{data: m.builder.CompactInto(m.bitmap.Bytes())}
		return
	}
	m.bitmap = Bitmap{data: m.builder.Compact()}
	m.owned = true
}

// Freeze returns the current bitmap as an immutable Bitmap without copying.
func (m *MutableBitmap) Freeze() Bitmap {
	return m.bitmap
}
