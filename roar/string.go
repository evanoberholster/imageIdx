package roar

import "fmt"

// String returns a compact human-readable summary of the bitmap's statistics:
// the number of containers, the total cardinality, the serialized size in
// bytes, and the count of array and bitmap containers. It is intended for
// debugging and diagnostics, not for parsing.
func (b Bitmap) String() string {
	if len(b.data) == 0 {
		return "Bitmap{containers=0, cardinality=0, bytes=0, arrays=0, bitmaps=0}"
	}
	var arrays, bitmaps int
	for _, entry := range b.keyEntries() {
		if ContainerType(containerHeaderAt(b.data[entry.Offset:]).Type) == ContainerArray {
			arrays++
		} else {
			bitmaps++
		}
	}
	return fmt.Sprintf("Bitmap{containers=%d, cardinality=%d, bytes=%d, arrays=%d, bitmaps=%d}",
		b.KeyCount(), b.Cardinality(), b.LenInBytes(), arrays, bitmaps)
}
