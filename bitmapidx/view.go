package bitmapidx

import "github.com/dgraph-io/sroar"

type BitmapDelta struct {
	Additions BitmapRef
	Removals  BitmapRef
}

func NewBitmapDelta(additions, removals []uint64) *BitmapDelta {
	if len(additions) == 0 && len(removals) == 0 {
		return nil
	}
	add := NewBitmapRef(additions)
	remove := NewBitmapRef(removals)
	// Public construction preserves the disjoint-set invariant. As with Write,
	// removal wins when malformed input names an ID in both sets.
	if add.bitmap != nil && remove.bitmap != nil {
		copy := cloneBitmap(add)
		copy.AndNot(remove.bitmap)
		add = BitmapRef{bitmap: copy}
	}
	if add.IsEmpty() && remove.IsEmpty() {
		return nil
	}
	return &BitmapDelta{Additions: add, Removals: remove}
}

func (d *BitmapDelta) Len() int {
	if d == nil {
		return 0
	}
	return int(d.Additions.Cardinality() + d.Removals.Cardinality())
}

func (d *BitmapDelta) state(id uint64) (present, touched bool) {
	if d == nil {
		return false, false
	}
	if d.Additions.Contains(id) {
		return true, true
	}
	if d.Removals.Contains(id) {
		return false, true
	}
	return false, false
}

type BitmapIndexView struct {
	Version   uint16
	IndexType IndexType
	Flags     Flags

	IndexID uint64
	ValueID uint64

	// Cardinality is the Base cardinality. Query planning intentionally does
	// not inspect delta cardinalities.
	Cardinality uint64
	Generation  uint64

	Base   BitmapRef
	Frozen *BitmapDelta
	Active *BitmapDelta
}

func (v *BitmapIndexView) Contains(id uint64) bool {
	if v == nil {
		return false
	}
	if present, ok := v.Active.state(id); ok {
		return present
	}
	if present, ok := v.Frozen.state(id); ok {
		return present
	}
	return v.Base.Contains(id)
}

func (v *BitmapIndexView) effectiveBitmap() BitmapRef {
	if v == nil {
		return EmptyBitmap()
	}
	result := cloneBitmap(v.Base)
	applyDelta(result, v.Frozen)
	applyDelta(result, v.Active)
	return BitmapRef{bitmap: result}
}

func applyDelta(dst interface {
	Or(*sroar.Bitmap)
	AndNot(*sroar.Bitmap)
}, delta *BitmapDelta) {
	if delta == nil {
		return
	}
	if delta.Additions.bitmap != nil {
		dst.Or(delta.Additions.bitmap)
	}
	if delta.Removals.bitmap != nil {
		dst.AndNot(delta.Removals.bitmap)
	}
}
