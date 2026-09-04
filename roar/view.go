package roar

import (
	"unsafe"
)

// Bitmap is an immutable roaring bitmap over a serialized byte region. The
// backing bytes may be an mmap or any other byte slice; they must not be
// modified or unmapped while a Bitmap referencing them remains in use.
//
// Bitmap values are cheap to copy and safe for concurrent reads. Their methods
// do not lock. Construct a Bitmap with FromSortedList, Open, or OpenTrusted.
// The zero value represents an empty bitmap.
type Bitmap struct {
	data []byte
}

// View is an alias for Bitmap.
//
// Deprecated: use Bitmap.
type View = Bitmap

// OpenTrusted wraps already-validated bytes. The caller guarantees that the
// bytes form a valid version-1 bitmap, that the base address is 8-byte
// aligned, and that the host is little-endian. It performs no checks.
func OpenTrusted(data []byte) Bitmap {
	return Bitmap{data: data}
}

// header returns a zero-copy view of the leading header.
func (v Bitmap) header() *header {
	return (*header)(unsafe.Pointer(unsafe.SliceData(v.data)))
}

// keyEntries returns a zero-copy view of the key table. The key table
// begins at HeaderBytes, which the format guarantees is 8-byte aligned.
func (v Bitmap) keyEntries() []keyEntry {
	if len(v.data) == 0 {
		return nil
	}
	h := v.header()
	return unsafe.Slice(
		(*keyEntry)(unsafe.Pointer(unsafe.SliceData(v.data[h.HeaderBytes:]))),
		int(h.KeyCount),
	)
}

// Cardinality sums the container cardinalities stored in the headers.
func (v Bitmap) Cardinality() uint64 {
	var total uint64
	for _, entry := range v.keyEntries() {
		total += uint64(containerHeaderAt(v.data[entry.Offset:]).Card)
	}
	return total
}

// KeyCount returns the number of containers.
func (v Bitmap) KeyCount() int {
	if len(v.data) == 0 {
		return 0
	}
	return int(v.header().KeyCount)
}

// IsEmpty reports whether the bitmap contains no containers.
func (v Bitmap) IsEmpty() bool {
	return len(v.data) == 0 || v.header().KeyCount == 0
}
