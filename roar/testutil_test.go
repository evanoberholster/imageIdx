package roar

import (
	"math/rand/v2"
	"sort"
)

// rngForTest returns a deterministic test generator.
func rngForTest() *rand.Rand { return rand.New(rand.NewPCG(99, 42)) }

// distinctValues returns count distinct non-zero uint16 values in ascending
// order. Zero is excluded so no produced ImageID is ever zero.
func distinctValues(rng *rand.Rand, count int) []uint16 {
	if count > 65535 {
		count = 65535
	}
	seen := make(map[uint16]struct{}, count)
	for len(seen) < count {
		seen[uint16(rng.IntN(65535)+1)] = struct{}{}
	}
	values := make([]uint16, 0, count)
	for value := range seen {
		values = append(values, value)
	}
	sort.Slice(values, func(i, j int) bool { return values[i] < values[j] })
	return values
}

// keyPool returns distinct deterministic keys spread over the 48-bit key
// space.
func keyPool(rng *rand.Rand, count int) []uint64 {
	base := rng.Uint64() &^ lowMask
	keys := make([]uint64, 0, count)
	for index := range count {
		key := base + uint64(index)*3*(1<<16)
		if key > ^uint64(0)-(1<<16) {
			key = base
		}
		keys = append(keys, key)
	}
	return keys
}

// randomSortedIDs returns sorted unique IDs whose containers mix tiny,
// medium, and dense shapes.
func randomSortedIDs(rng *rand.Rand, keys int) []uint64 {
	return randomSetOver(rng, keyPool(rng, keys))
}

// denseSortedIDs returns sorted IDs with consecutive values across
// consecutive keys, producing dense bitmap containers.
func denseSortedIDs(rng *rand.Rand, keys int) []uint64 {
	base := rng.Uint64() &^ lowMask
	ids := make([]uint64, 0, keys*40000)
	value := uint16(rng.IntN(1000) + 1)
	for index := range keys {
		key := base + uint64(index)*(1<<16)
		for count := 30000 + rng.IntN(35000); count > 0 && value != 0; count-- {
			ids = append(ids, key|uint64(value))
			value++
		}
		if value == 0 {
			value = 1
		}
	}
	return ids
}

// randomSetOver returns sorted unique IDs drawn from a shared key pool so
// independent sets overlap.
func randomSetOver(rng *rand.Rand, keys []uint64) []uint64 {
	ids := make([]uint64, 0, len(keys)*64)
	for _, key := range keys {
		if rng.IntN(3) == 0 {
			continue
		}
		var values []uint16
		switch rng.IntN(4) {
		case 0:
			values = distinctValues(rng, 1+rng.IntN(4))
		case 1, 2:
			values = distinctValues(rng, 16+rng.IntN(2000))
		default:
			values = distinctValues(rng, ArrayToBitmapThreshold+1+rng.IntN(20000))
		}
		for _, value := range values {
			ids = append(ids, key|uint64(value))
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}
