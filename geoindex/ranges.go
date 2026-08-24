package geoindex

import (
	"cmp"
	"slices"
	"sort"

	"github.com/golang/geo/s2"
)

// Range is an inclusive interval of fixed-level S2 keys.
type Range struct {
	Start S2Key
	End   S2Key // inclusive
}

// RangesFromCellIDs appends normalized level-16 descendant ranges for cells.
// Cells may be at any valid level up to GeoS2Level.
func RangesFromCellIDs(dst []Range, cellIDs []uint64) ([]Range, error) {
	start := len(dst)
	for _, value := range cellIDs {
		cell := s2.CellID(value)
		if !cell.IsValid() {
			return dst[:start], ErrInvalidS2Cell
		}
		if cell.Level() > GeoS2Level {
			return dst[:start], ErrInvalidS2Level
		}
		var first, last s2.CellID
		if cell.Level() == GeoS2Level {
			first, last = cell, cell
		} else {
			first = cell.ChildBeginAtLevel(GeoS2Level)
			last = cell.ChildEndAtLevel(GeoS2Level).Prev()
		}
		dst = append(dst, Range{Start: EncodeS2Key(uint64(first)), End: EncodeS2Key(uint64(last))})
	}
	return normalizeRanges(dst, start), nil
}

func normalizeRanges(ranges []Range, start int) []Range {
	prefix := ranges[:start]
	ranges = ranges[start:]
	if len(ranges) == 0 {
		return prefix
	}
	if rangesNormalized(ranges) {
		return append(prefix, ranges...)
	}
	for i := range ranges {
		if ranges[i].End < ranges[i].Start {
			ranges[i].Start, ranges[i].End = ranges[i].End, ranges[i].Start
		}
	}
	slices.SortFunc(ranges, func(left, right Range) int {
		if order := cmp.Compare(left.Start, right.Start); order != 0 {
			return order
		}
		return cmp.Compare(left.End, right.End)
	})
	out := 1
	for i := 1; i < len(ranges); i++ {
		previous := &ranges[out-1]
		// Valid level-16 keys differ by two. Joining an adjacent Hilbert
		// interval therefore allows one unused integer between ranges.
		if ranges[i].Start <= previous.End || previous.End <= ^S2Key(0)-2 && ranges[i].Start <= previous.End+2 {
			if ranges[i].End > previous.End {
				previous.End = ranges[i].End
			}
			continue
		}
		ranges[out] = ranges[i]
		out++
	}
	return append(prefix, ranges[:out]...)
}

func rangesNormalized(ranges []Range) bool {
	for index, current := range ranges {
		if current.End < current.Start {
			return false
		}
		if index == 0 {
			continue
		}
		previous := ranges[index-1]
		if current.Start <= previous.End || previous.End <= ^S2Key(0)-2 && current.Start <= previous.End+2 {
			return false
		}
	}
	return true
}

func lowerBound(keys []S2Key, target S2Key) int {
	return sort.Search(len(keys), func(i int) bool { return keys[i] >= target })
}

func upperBound(keys []S2Key, target S2Key) int {
	return sort.Search(len(keys), func(i int) bool { return keys[i] > target })
}
