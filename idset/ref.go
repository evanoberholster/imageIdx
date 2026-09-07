// Package idset provides immutable image-ID sets backed by roar bitmaps. It
// owns representation and serialization mechanics but no logical index,
// publication, or persistence policy.
package idset

import (
	"sort"

	imageidx "github.com/evanoberholster/imageIdx"
	"github.com/evanoberholster/imageIdx/roar"
)

// Ref is an immutable image-ID set. The underlying roar bitmap may reference
// an owned buffer or an mmap, which the caller must keep alive and unchanged
// for the Ref's lifetime.
type Ref struct {
	bitmap roar.Bitmap
}

func Empty() Ref {
	bitmap, err := roar.FromSorted(nil)
	if err != nil {
		return Ref{}
	}
	return Ref{bitmap: bitmap}
}

func New(ids []imageidx.ImageID) Ref {
	if len(ids) == 0 {
		return Empty()
	}
	values := append([]uint64(nil), ids...)
	sort.Slice(values, func(left, right int) bool { return values[left] < values[right] })
	values = compact(values)
	if len(values) != 0 && values[0] == 0 {
		values = values[1:]
	}
	bitmap, err := roar.FromSorted(values)
	if err != nil {
		return Empty()
	}
	return Ref{bitmap: bitmap}
}

// FromBuffer creates an immutable view over a serialized roar buffer. The
// buffer is validated; the caller must keep its contents unchanged for the
// Ref's lifetime.
func FromBuffer(buffer []byte) (Ref, error) {
	bitmap, err := roar.Open(buffer)
	if err != nil {
		return Ref{}, err
	}
	return Ref{bitmap: bitmap}, nil
}

// Valid reports whether the Ref holds a real bitmap. The zero Ref is invalid.
func (ref Ref) Valid() bool { return ref.bitmap.Bytes() != nil }

func (ref Ref) Contains(id imageidx.ImageID) bool {
	return ref.bitmap.Contains(id)
}

func (ref Ref) Cardinality() uint64 {
	return ref.bitmap.Cardinality()
}

func (ref Ref) IsEmpty() bool {
	return ref.bitmap.IsEmpty()
}

func (ref Ref) ToArray() []imageidx.ImageID {
	return ref.bitmap.AppendTo(nil)
}

// AppendTo emits IDs in ascending order without materializing the set. The
// caller owns dst; no allocation occurs for a sufficient-capacity dst.
func (ref Ref) AppendTo(dst []imageidx.ImageID) []imageidx.ImageID {
	return ref.bitmap.AppendTo(dst)
}

// FilterSorted appends the candidates present in the set to dst. Candidates
// must arrive in non-decreasing order. A sticky cursor reuses the loaded
// container across runs of IDs that share a key, so sorted probes avoid the
// key-table search that Contains pays per ID. It allocates nothing and
// supports in-place filtering (dst may alias candidates' backing store).
func (ref Ref) FilterSorted(dst, candidates []imageidx.ImageID) []imageidx.ImageID {
	return ref.bitmap.FilterSorted(dst, candidates)
}

// MarshalBinary returns an owned serialized roar image.
func (ref Ref) MarshalBinary() []byte {
	data, err := ref.bitmap.MarshalBinary()
	if err != nil {
		return nil
	}
	return data
}

// Bytes returns the underlying serialized bytes without copying. The caller
// must not modify the returned slice; the Ref retains ownership.
func (ref Ref) Bytes() []byte {
	return ref.bitmap.Bytes()
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
