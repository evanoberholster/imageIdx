package roar

import (
	"cmp"
	"slices"
)

// IDRange is a half-open interval [Lo, Hi) of image IDs.
type IDRange struct {
	// Lo is the inclusive lower bound.
	Lo uint64
	// Hi is the exclusive upper bound.
	Hi uint64
}

// RemoveRange returns a new Bitmap with all IDs in the half-open interval
// [lo, hi) removed. If hi <= lo the receiver is returned unchanged. The
// receiver is never modified.
func (b Bitmap) RemoveRange(lo, hi uint64) Bitmap {
	if hi <= lo {
		return b
	}
	var ranges [1]IDRange
	ranges[0] = IDRange{Lo: lo, Hi: hi}
	builder := NewBuilder(b.KeyCount())
	removeRangesInto(builder, b, ranges[:])
	return Bitmap{data: builder.Compact()}
}

// RemoveRanges returns a new Bitmap with all IDs in any of the half-open
// intervals removed. Empty intervals are ignored; overlapping or adjacent
// intervals are merged. Removing N ranges costs one pass over the bitmap,
// unlike N RemoveRange calls which copy the survivors N times. The receiver
// is never modified.
func (b Bitmap) RemoveRanges(ranges []IDRange) Bitmap {
	norm := normalizeIDRanges(ranges)
	if len(norm) == 0 {
		return b
	}
	if len(norm) == 1 {
		return b.RemoveRange(norm[0].Lo, norm[0].Hi)
	}
	builder := NewBuilder(b.KeyCount())
	removeRangesInto(builder, b, norm)
	return Bitmap{data: builder.Compact()}
}

// normalizeIDRanges drops empty intervals and returns the rest sorted and
// merged. Adjacent intervals merge too since the union is identical.
func normalizeIDRanges(ranges []IDRange) []IDRange {
	norm := make([]IDRange, 0, len(ranges))
	for _, r := range ranges {
		if r.Hi > r.Lo {
			norm = append(norm, r)
		}
	}
	if len(norm) == 0 {
		return nil
	}
	slices.SortFunc(norm, func(a, b IDRange) int {
		if a.Lo != b.Lo {
			return cmp.Compare(a.Lo, b.Lo)
		}
		return cmp.Compare(a.Hi, b.Hi)
	})
	out := norm[:1]
	for _, r := range norm[1:] {
		last := &out[len(out)-1]
		if r.Lo <= last.Hi {
			if r.Hi > last.Hi {
				last.Hi = r.Hi
			}
			continue
		}
		out = append(out, r)
	}
	return out
}

// removeRangesInto appends src minus the IDs in any of ranges to dst.
// ranges must be normalized: sorted, merged, and non-empty.
func removeRangesInto(dst *Builder, src Bitmap, ranges []IDRange) {
	const containerSize = uint64(1) << 16
	ri := 0
	var istack [4][2]int
	for _, entry := range src.keyEntries() {
		key := entry.Key
		container := src.data[entry.Offset:]
		// Advance past ranges ending at or before this container.
		for ri < len(ranges) && ranges[ri].Hi <= key {
			ri++
		}
		// Gather value intervals overlapping this container. The bound
		// arithmetic is overflow-safe: containment is tested with Lo-key
		// instead of key+containerSize, which would wrap on the top key.
		intervals := istack[:0]
		for rj := ri; rj < len(ranges); rj++ {
			if ranges[rj].Lo > key && ranges[rj].Lo-key >= containerSize {
				break
			}
			lo, hi := 0, 1<<16
			if ranges[rj].Lo > key {
				lo = int(lowValue(ranges[rj].Lo))
			}
			if ranges[rj].Hi-key < containerSize {
				hi = int(lowValue(ranges[rj].Hi))
			}
			if lo < hi {
				intervals = append(intervals, [2]int{lo, hi})
			}
		}
		switch len(intervals) {
		case 0:
			copyContainerInto(dst, key, container)
		case 1:
			removeValuesInto(dst, key, container, intervals[0][0], intervals[0][1])
		default:
			removeIntervalsInto(dst, key, container, intervals)
		}
	}
}

// removeValuesInto appends container minus the values in the half-open range
// [lo, hi) to dst. hi may equal 1<<16 to mean "through the end of the
// container".
func removeValuesInto(dst *Builder, key uint64, container []byte, lo, hi int) {
	header := containerHeaderAt(container)
	if ContainerType(header.Type) == ContainerArray {
		values := arrayValuesAt(container)
		dst.scratchValues = dst.scratchValues[:0]
		for _, value := range values {
			v := int(value)
			if v >= lo && v < hi {
				continue
			}
			dst.scratchValues = append(dst.scratchValues, value)
		}
		dst.appendArray(key, dst.scratchValues)
		return
	}
	words := bitmapWordsAt(container)
	scratch := dst.wordsScratch()
	copy(scratch, words)
	clearRangeWords(scratch, lo, hi)
	dst.emitWords(key, uint32(wordsCardinality(scratch)))
}

// removeIntervalsInto appends container minus the half-open value intervals
// to dst. intervals must be sorted and disjoint.
func removeIntervalsInto(dst *Builder, key uint64, container []byte, intervals [][2]int) {
	header := containerHeaderAt(container)
	if ContainerType(header.Type) == ContainerArray {
		values := arrayValuesAt(container)
		dst.scratchValues = dst.scratchValues[:0]
		iv := 0
		for _, value := range values {
			v := int(value)
			for iv < len(intervals) && v >= intervals[iv][1] {
				iv++
			}
			if iv < len(intervals) && v >= intervals[iv][0] {
				continue
			}
			dst.scratchValues = append(dst.scratchValues, value)
		}
		dst.appendArray(key, dst.scratchValues)
		return
	}
	words := bitmapWordsAt(container)
	scratch := dst.wordsScratch()
	copy(scratch, words)
	for _, iv := range intervals {
		clearRangeWords(scratch, iv[0], iv[1])
	}
	dst.emitWords(key, uint32(wordsCardinality(scratch)))
}

// clearRangeWords clears words covering the half-open value range [lo, hi)
// with word-edge masks so full words are cleared without per-value loops.
func clearRangeWords(words []uint64, lo, hi int) {
	if lo >= hi {
		return
	}
	first, last := lo>>6, (hi-1)>>6
	if first == last {
		words[first] &^= ^uint64(0) << uint(lo&63) & (^uint64(0) >> uint(-hi&63))
		return
	}
	words[first] &^= ^uint64(0) << uint(lo&63)
	for w := first + 1; w < last; w++ {
		words[w] = 0
	}
	words[last] &^= ^uint64(0) >> uint(-hi&63)
}
