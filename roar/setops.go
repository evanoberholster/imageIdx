package roar

// And returns the intersection of a and b as a new Bitmap. It is a
// convenience over AndInto plus Builder.Compact; prefer the Builder-based
// forms when the result buffer can be reused.
func And(a, b Bitmap) Bitmap {
	builder := NewBuilder(min(a.KeyCount(), b.KeyCount()))
	AndInto(builder, a, b)
	return Bitmap{data: builder.Compact()}
}

// Or returns the union of a and b as a new Bitmap.
func Or(a, b Bitmap) Bitmap {
	builder := NewBuilder(a.KeyCount() + b.KeyCount())
	OrInto(builder, a, b)
	return Bitmap{data: builder.Compact()}
}

// AndNot returns a minus b as a new Bitmap.
func AndNot(a, b Bitmap) Bitmap {
	builder := NewBuilder(a.KeyCount())
	AndNotInto(builder, a, b)
	return Bitmap{data: builder.Compact()}
}

// AndMany returns the N-way intersection of bitmaps as a new Bitmap.
func AndMany(bitmaps ...Bitmap) Bitmap {
	if len(bitmaps) == 0 {
		return Bitmap{}
	}
	capacity := bitmaps[0].KeyCount()
	for _, bm := range bitmaps[1:] {
		if n := bm.KeyCount(); n < capacity {
			capacity = n
		}
	}
	builder := NewBuilder(capacity)
	AndManyInto(builder, bitmaps...)
	return Bitmap{data: builder.Compact()}
}
