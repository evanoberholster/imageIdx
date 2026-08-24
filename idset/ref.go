// Package idset provides immutable image-ID sets backed by roaring bitmaps.
// It owns representation and serialization mechanics but no logical index,
// publication, or persistence policy.
package idset

import (
	"sort"

	"github.com/dgraph-io/sroar"
	imageidx "github.com/evanoberholster/imageIdx"
)

// Ref is an immutable image-ID set. backing retains an owned or mmap-backed
// serialized buffer for as long as the set is reachable.
type Ref struct {
	bitmap  *sroar.Bitmap
	backing []byte
}

func Empty() Ref { return Ref{bitmap: sroar.NewBitmap()} }

func New(ids []imageidx.ImageID) Ref {
	if len(ids) == 0 {
		return Empty()
	}
	values := append([]uint64(nil), ids...)
	sort.Slice(values, func(left, right int) bool { return values[left] < values[right] })
	values = compact(values)
	return Ref{bitmap: sroar.FromSortedList(values)}
}

// FromBuffer creates an immutable view over a serialized sroar buffer. The
// caller must keep the buffer contents unchanged for the Ref's lifetime.
func FromBuffer(buffer []byte) Ref {
	return Ref{bitmap: sroar.FromBuffer(buffer), backing: buffer}
}

func (ref Ref) Valid() bool { return ref.bitmap != nil }

func (ref Ref) Contains(id imageidx.ImageID) bool {
	return ref.bitmap != nil && ref.bitmap.Contains(id)
}

func (ref Ref) Cardinality() uint64 {
	if ref.bitmap == nil {
		return 0
	}
	return uint64(ref.bitmap.GetCardinality())
}

func (ref Ref) IsEmpty() bool {
	return ref.bitmap == nil || ref.bitmap.IsEmpty()
}

func (ref Ref) ToArray() []imageidx.ImageID {
	if ref.bitmap == nil {
		return nil
	}
	return ref.bitmap.ToArray()
}

// AppendTo emits IDs in ascending order without materializing the set. The
// caller owns dst; the only allocation is the roaring iterator itself.
func (ref Ref) AppendTo(dst []imageidx.ImageID) []imageidx.ImageID {
	if ref.bitmap == nil {
		return dst
	}
	cardinality := ref.bitmap.GetCardinality()
	if cardinality == 0 {
		return dst
	}
	// sroar's iterator uses 0 as its end sentinel, which is ambiguous for a
	// set that contains ID 0, so iterate exactly cardinality times instead of
	// stopping on the sentinel.
	iterator := ref.bitmap.NewIterator()
	for range cardinality {
		dst = append(dst, iterator.Next())
	}
	return dst
}

// MarshalBinary returns an owned serialized sroar image.
func (ref Ref) MarshalBinary() []byte {
	if ref.bitmap == nil {
		return Empty().bitmap.ToBufferWithCopy()
	}
	return ref.bitmap.ToBufferWithCopy()
}

func compact(values []uint64) []uint64 {
	if len(values) < 2 {
		return values
	}
	out := 1
	for index := 1; index < len(values); index++ {
		if values[index] != values[out-1] {
			values[out] = values[index]
			out++
		}
	}
	return values[:out]
}
