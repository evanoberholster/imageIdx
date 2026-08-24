package roar

import "sort"

// linearKeySearchThreshold switches key lookup from linear scan to binary
// search. It is a runtime policy that benchmarks may tune.
const linearKeySearchThreshold = 16

// findEntry locates the key entry for an upper-48-bit key. The returned
// index is the insertion position when the key is absent.
func (v View) findEntry(key uint64) (keyEntry, bool) {
	entries := v.keyEntries()
	index := sort.Search(len(entries), func(i int) bool {
		return entries[i].Key >= key
	})
	if index < len(entries) && entries[index].Key == key {
		return entries[index], true
	}
	return keyEntry{}, false
}

// findKey locates the key entry index for an upper-48-bit key.
func (v View) findKey(key uint64) (int, bool) {
	entries := v.keyEntries()
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
