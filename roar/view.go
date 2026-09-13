package roar

import (
	"fmt"
	"unsafe"
)

// Bitmap is an immutable roaring bitmap over a serialized byte region. The
// backing bytes may be an mmap or any other byte slice; they must not be
// modified or unmapped while a Bitmap referencing them remains in use.
//
// Bitmap values are cheap to copy and safe for concurrent reads. Their methods
// do not lock. Construct a Bitmap with FromSorted, Open, or OpenTrusted. The
// zero value represents an empty bitmap.
type Bitmap struct {
	data []byte
}

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

// Cardinality returns the number of IDs. When the bitmap was built with the
// stored-cardinality flag (all constructors emit it), it reads the total
// directly from the header in O(1); otherwise it falls back to summing the
// container headers.
func (v Bitmap) Cardinality() uint64 {
	if len(v.data) == 0 {
		return 0
	}
	h := v.header()
	if h.Flags&FlagStoredCardinality != 0 {
		return h.Cardinality
	}
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

// linearKeySearchThreshold switches key lookup from linear scan to binary
// search. It is a runtime policy that benchmarks may tune.
const linearKeySearchThreshold = 16

// findEntry locates the entry for an upper-48-bit key.
func (v Bitmap) findEntry(key uint64) (keyEntry, bool) {
	entries := v.keyEntries()
	index, found := findKey(entries, key)
	if !found {
		return keyEntry{}, false
	}
	return entries[index], true
}

// keysOverlap reports whether a and b share any container key. It walks
// both key tables without touching container payloads, so it is much cheaper
// than a full operation when the answer is false.
func keysOverlap(a, b Bitmap) bool {
	ae, be := a.keyEntries(), b.keyEntries()
	i, j := 0, 0
	for i < len(ae) && j < len(be) {
		switch {
		case ae[i].Key < be[j].Key:
			i++
		case ae[i].Key > be[j].Key:
			j++
		default:
			return true
		}
	}
	return false
}

// findKey locates key in entries. It returns the insertion position when key
// is absent. The binary path is a manual loop so the closure passed to
// sort.Search does not block inlining on the Contains hot path.
func findKey(entries []keyEntry, key uint64) (int, bool) {
	if len(entries) <= linearKeySearchThreshold {
		for index := range entries {
			current := entries[index].Key
			if current == key {
				return index, true
			}
			if current > key {
				return index, false
			}
		}
		return len(entries), false
	}
	lo, hi := 0, len(entries)
	for lo < hi {
		mid := int(uint(lo+hi) >> 1)
		if entries[mid].Key < key {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	return lo, lo < len(entries) && entries[lo].Key == key
}

// Stats summarizes the physical layout of a Bitmap in one header-table walk.
// It allocates nothing and performs no popcounts: cardinality comes from the
// stored header total when present, otherwise from the container headers.
type Stats struct {
	Containers  int
	Arrays      int
	Bitmaps     int
	Cardinality uint64
	Bytes       int
}

// Stats returns the layout summary of the bitmap. The zero Bitmap reports
// zero Stats.
func (b Bitmap) Stats() Stats {
	if len(b.data) == 0 {
		return Stats{}
	}
	var arrays, bitmaps int
	for _, entry := range b.keyEntries() {
		if ContainerType(containerHeaderAt(b.data[entry.Offset:]).Type) == ContainerArray {
			arrays++
		} else {
			bitmaps++
		}
	}
	return Stats{
		Containers:  b.KeyCount(),
		Arrays:      arrays,
		Bitmaps:     bitmaps,
		Cardinality: b.Cardinality(),
		Bytes:       len(b.data),
	}
}

// Version returns the serialized format version, or 0 for the empty Bitmap.
func (b Bitmap) Version() uint16 {
	if len(b.data) == 0 {
		return 0
	}
	return b.header().Version
}

// Flags returns the serialized header flags, or 0 for the empty Bitmap.
func (b Bitmap) Flags() uint16 {
	if len(b.data) == 0 {
		return 0
	}
	return b.header().Flags
}

// String returns a compact human-readable summary of the bitmap's statistics:
// the number of containers, the total cardinality, the serialized size in
// bytes, and the count of array and bitmap containers. It is intended for
// debugging and diagnostics, not for parsing.
func (b Bitmap) String() string {
	s := b.Stats()
	return fmt.Sprintf("Bitmap{containers=%d, cardinality=%d, bytes=%d, arrays=%d, bitmaps=%d}",
		s.Containers, s.Cardinality, s.Bytes, s.Arrays, s.Bitmaps)
}
