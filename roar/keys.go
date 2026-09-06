package roar

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
