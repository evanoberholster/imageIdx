package roar

// And returns the intersection of a and b as a new Bitmap. It is a
// convenience over AndInto plus Builder.Compact; prefer the Builder-based
// forms when the result buffer can be reused. Empty operands short-circuit
// to an empty Bitmap.
func And(a, b Bitmap) Bitmap {
	if a.IsEmpty() || b.IsEmpty() {
		return Bitmap{}
	}
	builder := NewBuilder(min(a.KeyCount(), b.KeyCount()))
	builder.reserveData(min(a.LenInBytes(), b.LenInBytes()))
	AndInto(builder, a, b)
	return Bitmap{data: builder.Compact()}
}

// Or returns the union of a and b as a new Bitmap. Empty operands are
// returned directly without building, since Bitmaps are immutable.
func Or(a, b Bitmap) Bitmap {
	if a.IsEmpty() {
		return b
	}
	if b.IsEmpty() {
		return a
	}
	builder := NewBuilder(a.KeyCount() + b.KeyCount())
	builder.reserveData(a.LenInBytes() + b.LenInBytes())
	OrInto(builder, a, b)
	return Bitmap{data: builder.Compact()}
}

// AndNot returns a minus b as a new Bitmap. Empty operands short-circuit:
// an empty a yields empty, and an empty b yields a unchanged.
func AndNot(a, b Bitmap) Bitmap {
	if a.IsEmpty() {
		return Bitmap{}
	}
	if b.IsEmpty() {
		return a
	}
	builder := NewBuilder(a.KeyCount())
	builder.reserveData(a.LenInBytes())
	AndNotInto(builder, a, b)
	return Bitmap{data: builder.Compact()}
}

// AndMany returns the N-way intersection of bitmaps as a new Bitmap. Any
// empty operand short-circuits to an empty Bitmap.
func AndMany(bitmaps ...Bitmap) Bitmap {
	if len(bitmaps) == 0 {
		return Bitmap{}
	}
	for _, bm := range bitmaps {
		if bm.IsEmpty() {
			return Bitmap{}
		}
	}
	capacity := bitmaps[0].KeyCount()
	bytes := bitmaps[0].LenInBytes()
	for _, bm := range bitmaps[1:] {
		if n := bm.KeyCount(); n < capacity {
			capacity = n
		}
		if n := bm.LenInBytes(); n < bytes {
			bytes = n
		}
	}
	builder := NewBuilder(capacity)
	builder.reserveData(bytes)
	AndManyInto(builder, bitmaps...)
	return Bitmap{data: builder.Compact()}
}
