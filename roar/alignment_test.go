package roar

import (
	"testing"
	"unsafe"
)

// TestConstructorsProduceAlignedBuffers asserts that every constructor that
// allocates a fresh serialized region yields an 8-byte-aligned base pointer,
// the invariant zero-copy uint64 access depends on.
func TestConstructorsProduceAlignedBuffers(t *testing.T) {
	ids := denseSortedIDs(rngForTest(), 4)

	fromSorted, err := FromSorted(ids)
	if err != nil {
		t.Fatal(err)
	}
	requireAligned(fromSorted)

	bitmap, err := FromSortedList(ids)
	if err != nil {
		t.Fatal(err)
	}
	requireAligned(bitmap.data)

	marshaled, err := bitmap.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	requireAligned(marshaled)

	cloned := bitmap.Clone()
	requireAligned(cloned.data)

	combined := Or(bitmap, bitmap)
	requireAligned(combined.data)
}

// TestRequireAlignedPanicsOnMisalignedBuffer verifies the guard fails loudly
// on a slice whose base address is deliberately offset by one byte.
func TestRequireAlignedPanicsOnMisalignedBuffer(t *testing.T) {
	backing := make([]byte, Alignment+1)
	misaligned := backing[1:]
	if uintptr(unsafe.Pointer(unsafe.SliceData(misaligned)))%Alignment == 0 {
		t.Skip("allocator returned a base that makes offset 1 aligned; cannot construct a misaligned slice")
	}
	defer func() {
		if recover() == nil {
			t.Fatal("requireAligned did not panic on a misaligned buffer")
		}
	}()
	requireAligned(misaligned)
}
