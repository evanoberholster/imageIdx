package roar

import (
	"sync"
)

const (
	// keyUnits is the number of distinct container keys: the upper 48 bits of
	// a uint64, i.e. values u<<16 for u in [0, 2^48).
	keyUnits = uint64(1) << 48
	// maxKeyBoundary is a sentinel greater than every representable key,
	// standing in for the unbounded upper end of the last partition.
	maxKeyBoundary = ^uint64(0)
)

// keyBoundaries partitions the key domain into numGo ascending, unit-aligned
// ranges, returning numGo+1 boundaries. The final boundary is maxKeyBoundary.
func keyBoundaries(numGo int) []uint64 {
	if numGo <= 1 {
		return []uint64{0, maxKeyBoundary}
	}
	step := (keyUnits + uint64(numGo) - 1) / uint64(numGo)
	boundaries := make([]uint64, numGo+1)
	for index := 0; index < numGo; index++ {
		boundaries[index] = uint64(index) * step << 16
	}
	boundaries[numGo] = maxKeyBoundary
	return boundaries
}

// keyRange returns the half-open index range [start, end) of entries whose
// keys fall within [lo, hi). It reuses findKey so small tables stay linear,
// matching the lookup policy on the Contains hot path.
func keyRange(entries []keyEntry, lo, hi uint64) (int, int) {
	start, _ := findKey(entries, lo)
	end, _ := findKey(entries, hi)
	return start, end
}

// combineBuilders concatenates builders whose key sets are disjoint and
// ascending into one serialization. Payload bytes are copied once, directly
// into the final buffer.
func combineBuilders(builders []*Builder) []byte {
	totalKeys, totalData := 0, 0
	var totalCard uint64
	for _, builder := range builders {
		totalKeys += len(builder.keys)
		totalData += len(builder.data)
		totalCard += builder.cardinality
	}
	keys := make([]uint64, 0, totalKeys)
	offsets := make([]uint64, 0, totalKeys)
	rel := uint64(0)
	for _, builder := range builders {
		keys = append(keys, builder.keys...)
		for _, off := range builder.offsets {
			offsets = append(offsets, rel+off)
		}
		rel += uint64(len(builder.data))
	}
	containerBase := HeaderSize + totalKeys*KeyEntrySize
	out := make([]byte, containerBase+totalData)
	requireAligned(out)
	writeHeaderKeyTable(out, keys, offsets, totalCard)
	pos := containerBase
	for _, builder := range builders {
		pos += copy(out[pos:], builder.data)
	}
	return out
}

// minParallelKeys is the minimum total container count for parallel execution
// to pay off. Smaller inputs run sequentially to avoid goroutine, sync, and
// per-range Builder allocation overhead.
const minParallelKeys = 32

// shouldParallelize reports whether numGo-way parallel execution is worthwhile
// for totalKeys containers.
func shouldParallelize(totalKeys, numGo int) bool {
	return numGo > 1 && totalKeys >= minParallelKeys
}

// runParallel executes fn over numGo contiguous key ranges and merges the
// results. With numGo <= 1 it runs inline.
func runParallel(numGo int, fn func(dst *Builder, lo, hi uint64)) Bitmap {
	if numGo <= 1 {
		builder := NewBuilder(0)
		fn(builder, 0, maxKeyBoundary)
		return Bitmap{data: builder.Compact()}
	}
	boundaries := keyBoundaries(numGo)
	builders := make([]*Builder, numGo)
	for index := range builders {
		builders[index] = NewBuilder(0)
	}
	var wg sync.WaitGroup
	for index := 0; index < numGo; index++ {
		lo, hi := boundaries[index], boundaries[index+1]
		if lo >= hi {
			continue
		}
		wg.Add(1)
		go func(dst *Builder, lo, hi uint64) {
			defer wg.Done()
			fn(dst, lo, hi)
		}(builders[index], lo, hi)
	}
	wg.Wait()
	return Bitmap{data: combineBuilders(builders)}
}

// AndConc returns the intersection of a and b, computing container pairs in
// parallel across up to maxConcurrency key ranges. Small inputs run
// sequentially.
func AndConc(a, b Bitmap, maxConcurrency int) Bitmap {
	if !shouldParallelize(a.KeyCount()+b.KeyCount(), maxConcurrency) {
		return And(a, b)
	}
	return runParallel(maxConcurrency, func(dst *Builder, lo, hi uint64) {
		andIntoRange(dst, a, b, lo, hi)
	})
}

// OrConc returns the union of a and b, computing container pairs in parallel
// across up to maxConcurrency key ranges. Small inputs run sequentially.
func OrConc(a, b Bitmap, maxConcurrency int) Bitmap {
	if !shouldParallelize(a.KeyCount()+b.KeyCount(), maxConcurrency) {
		return Or(a, b)
	}
	return runParallel(maxConcurrency, func(dst *Builder, lo, hi uint64) {
		orIntoRange(dst, a, b, lo, hi)
	})
}

// AndNotConc returns a minus b, computed in parallel across up to
// maxConcurrency key ranges. Small inputs run sequentially.
func AndNotConc(a, b Bitmap, maxConcurrency int) Bitmap {
	if !shouldParallelize(a.KeyCount()+b.KeyCount(), maxConcurrency) {
		return AndNot(a, b)
	}
	return runParallel(maxConcurrency, func(dst *Builder, lo, hi uint64) {
		andNotIntoRange(dst, a, b, lo, hi)
	})
}

// FastParOr returns the union of bitmaps, computing it in parallel across
// numGo key ranges. Small inputs fold sequentially.
func FastParOr(numGo int, bitmaps ...Bitmap) Bitmap {
	switch len(bitmaps) {
	case 0:
		return Bitmap{}
	case 1:
		return bitmaps[0]
	}
	total := 0
	for _, bm := range bitmaps {
		total += bm.KeyCount()
	}
	if !shouldParallelize(total, numGo) {
		result := bitmaps[0]
		for _, bm := range bitmaps[1:] {
			result = Or(result, bm)
		}
		return result
	}
	return runParallel(numGo, func(dst *Builder, lo, hi uint64) {
		orManyIntoRange(dst, lo, hi, bitmaps...)
	})
}

// andIntoRange is AndInto restricted to keys in [lo, hi).
func andIntoRange(dst *Builder, a, b Bitmap, lo, hi uint64) {
	aEntries, bEntries := a.keyEntries(), b.keyEntries()
	ai, aEnd := keyRange(aEntries, lo, hi)
	bi, bEnd := keyRange(bEntries, lo, hi)
	andEntriesInto(dst, a.data, b.data, aEntries[ai:aEnd], bEntries[bi:bEnd])
}

// orIntoRange is OrInto restricted to keys in [lo, hi).
func orIntoRange(dst *Builder, a, b Bitmap, lo, hi uint64) {
	aEntries, bEntries := a.keyEntries(), b.keyEntries()
	ai, aEnd := keyRange(aEntries, lo, hi)
	bi, bEnd := keyRange(bEntries, lo, hi)
	orEntriesInto(dst, a.data, b.data, aEntries[ai:aEnd], bEntries[bi:bEnd])
}

// andNotIntoRange is AndNotInto restricted to keys in [lo, hi).
func andNotIntoRange(dst *Builder, a, b Bitmap, lo, hi uint64) {
	aEntries, bEntries := a.keyEntries(), b.keyEntries()
	ai, aEnd := keyRange(aEntries, lo, hi)
	bi, bEnd := keyRange(bEntries, lo, hi)
	andNotEntriesInto(dst, a.data, b.data, aEntries[ai:aEnd], bEntries[bi:bEnd])
}

// orManyIntoRange is the N-way union restricted to keys in [lo, hi).
func orManyIntoRange(dst *Builder, lo, hi uint64, bitmaps ...Bitmap) {
	count := len(bitmaps)
	var stackEntries [maxStackViews][]keyEntry
	var stackPositions [maxStackViews]int
	var stackEnds [maxStackViews]int
	var stackContainers [maxStackViews][]byte
	entries := stackEntries[:0]
	positions := stackPositions[:0]
	ends := stackEnds[:0]
	var containers [][]byte
	if count > maxStackViews {
		entries = make([][]keyEntry, count)
		positions = make([]int, count)
		ends = make([]int, count)
		containers = make([][]byte, 0, count)
	} else {
		containers = stackContainers[:0]
	}
	entries = entries[:count]
	positions = positions[:count]
	ends = ends[:count]
	for index, bm := range bitmaps {
		list := bm.keyEntries()
		start, end := keyRange(list, lo, hi)
		entries[index] = list
		positions[index] = start
		ends[index] = end
	}
	for {
		minKey, found := minKey(entries, positions, ends)
		if !found {
			return
		}
		containers = containers[:0]
		for index := 0; index < count; index++ {
			if positions[index] < ends[index] && entries[index][positions[index]].Key == minKey {
				containers = append(containers, bitmaps[index].data[entries[index][positions[index]].Offset:])
				positions[index]++
			}
		}
		orManyContainersInto(dst, minKey, containers)
	}
}
