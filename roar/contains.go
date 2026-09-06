package roar

// Contains reports whether id is present. It performs a key lookup followed
// by an array search or a direct bitmap bit test and allocates nothing.
// Single-container bitmaps skip the key-table search.
func (v Bitmap) Contains(id uint64) bool {
	if id == InvalidImageID || len(v.data) == 0 {
		return false
	}
	if v.header().KeyCount == 1 {
		entries := v.keyEntries()
		if entries[0].Key != highKey(id) {
			return false
		}
		return containsContainer(v.data[entries[0].Offset:], lowValue(id))
	}
	entry, ok := v.findEntry(highKey(id))
	if !ok {
		return false
	}
	return containsContainer(v.data[entry.Offset:], lowValue(id))
}
