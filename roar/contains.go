package roar

// Contains reports whether id is present. It performs a key lookup followed
// by an array search or a direct bitmap bit test and allocates nothing.
func (v Bitmap) Contains(id uint64) bool {
	if id == InvalidImageID {
		return false
	}
	entry, ok := v.findEntry(highKey(id))
	if !ok {
		return false
	}
	return containsContainer(v.data[entry.Offset:], lowValue(id))
}
