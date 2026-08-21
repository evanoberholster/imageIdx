package bitmapidx

import (
	"sort"

	"github.com/dgraph-io/sroar"
)

// BitmapRef is an immutable bitmap handle. The concrete sroar representation
// is deliberately hidden so callers cannot mutate reader-visible state.
// backing retains an mmap or owned serialized buffer for the bitmap lifetime.
type BitmapRef struct {
	bitmap  *sroar.Bitmap
	backing []byte
}

func EmptyBitmap() BitmapRef { return BitmapRef{bitmap: sroar.NewBitmap()} }

func NewBitmapRef(ids []uint64) BitmapRef {
	if len(ids) == 0 {
		return EmptyBitmap()
	}
	values := append([]uint64(nil), ids...)
	sort.Slice(values, func(i, j int) bool { return values[i] < values[j] })
	values = compactUint64(values)
	return BitmapRef{bitmap: sroar.FromSortedList(values)}
}

func bitmapRefFromOwnedBuffer(buffer []byte) BitmapRef {
	return BitmapRef{bitmap: sroar.FromBuffer(buffer), backing: buffer}
}

func bitmapRefFromMappedBuffer(buffer []byte) BitmapRef {
	return BitmapRef{bitmap: sroar.FromBuffer(buffer), backing: buffer}
}

// BitmapRefFromBuffer creates an immutable view over a serialized sroar
// buffer. The caller must keep the buffer contents unchanged for the returned
// reference's lifetime. This is intended for mmap-backed sibling indexes.
func BitmapRefFromBuffer(buffer []byte) BitmapRef {
	return bitmapRefFromMappedBuffer(buffer)
}

func (r BitmapRef) valid() bool { return r.bitmap != nil }

func (r BitmapRef) Contains(id uint64) bool {
	return r.bitmap != nil && r.bitmap.Contains(id)
}

func (r BitmapRef) Cardinality() uint64 {
	if r.bitmap == nil {
		return 0
	}
	return uint64(r.bitmap.GetCardinality())
}

func (r BitmapRef) IsEmpty() bool { return r.bitmap == nil || r.bitmap.IsEmpty() }

func (r BitmapRef) ToArray() []uint64 {
	if r.bitmap == nil {
		return nil
	}
	return r.bitmap.ToArray()
}

// AppendTo emits IDs through the representation boundary. The current sroar
// iterator materializes internally; future bitmap kernels can remove that cost
// without changing sibling index packages.
func (r BitmapRef) AppendTo(dst []ImageID) []ImageID {
	if r.bitmap == nil {
		return dst
	}
	iterator := r.bitmap.ManyIterator()
	var buffer [256]uint64
	for {
		count := iterator.NextMany(buffer[:])
		for _, id := range buffer[:count] {
			dst = append(dst, ImageID(id))
		}
		if count != len(buffer) {
			return dst
		}
	}
}

// MarshalBinary returns an owned serialized sroar image.
func (r BitmapRef) MarshalBinary() []byte {
	if r.bitmap == nil {
		return EmptyBitmap().bitmap.ToBufferWithCopy()
	}
	return r.bitmap.ToBufferWithCopy()
}

func cloneBitmap(r BitmapRef) *sroar.Bitmap {
	if r.bitmap == nil {
		return sroar.NewBitmap()
	}
	return r.bitmap.Clone()
}

func compactUint64(values []uint64) []uint64 {
	if len(values) < 2 {
		return values
	}
	out := 1
	for i := 1; i < len(values); i++ {
		if values[i] != values[out-1] {
			values[out] = values[i]
			out++
		}
	}
	return values[:out]
}
