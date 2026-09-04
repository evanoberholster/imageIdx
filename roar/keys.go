package roar

import "sort"

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
// is absent.
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
	index := sort.Search(len(entries), func(i int) bool {
		return entries[i].Key >= key
	})
	return index, index < len(entries) && entries[index].Key == key
}
