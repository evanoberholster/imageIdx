package roar

import (
	"unsafe"
)

// View is an immutable bitmap over a serialized byte region. The backing
// bytes may be an mmap or any other byte slice; they must not be modified or
// unmapped while a View referencing them remains in use.
//
// View is safe for concurrent reads. Its methods do not lock.
type View struct {
	data []byte
}

// OpenTrusted wraps already-validated bytes. The caller guarantees that the
// bytes form a valid version-1 bitmap, that the base address is 8-byte
// aligned, and that the host is little-endian. It performs no checks.
func OpenTrusted(data []byte) View {
	return View{data: data}
}

// header returns a zero-copy view of the leading header.
func (v View) header() *header {
	return (*header)(unsafe.Pointer(unsafe.SliceData(v.data)))
}

// keyEntries returns a zero-copy view of the key table. The key table
// begins at HeaderBytes, which the format guarantees is 8-byte aligned.
func (v View) keyEntries() []keyEntry {
	h := v.header()
	return unsafe.Slice(
		(*keyEntry)(unsafe.Pointer(unsafe.SliceData(v.data[h.HeaderBytes:]))),
		int(h.KeyCount),
	)
}

// Cardinality sums the container cardinalities stored in the headers.
func (v View) Cardinality() uint64 {
	var total uint64
	for _, entry := range v.keyEntries() {
		total += uint64(containerHeaderAt(v.data[entry.Offset:]).Card)
	}
	return total
}

// KeyCount returns the number of containers.
func (v View) KeyCount() int {
	return int(v.header().KeyCount)
}

// IsEmpty reports whether the bitmap contains no containers.
func (v View) IsEmpty() bool {
	return v.header().KeyCount == 0
}
