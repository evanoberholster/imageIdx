package geoindex

import (
	"sort"

	"github.com/golang/geo/s2"
)

type S2KeyRange struct {
	Start S2Key
	End   S2Key // inclusive
}

func RangesFromCellUnion(cells s2.CellUnion) ([]S2KeyRange, error) {
	ranges := make([]S2KeyRange, 0, len(cells))
	for _, cell := range cells {
		if !cell.IsValid() {
			return nil, ErrInvalidS2Cell
		}
		if cell.Level() > GeoS2Level {
			cell = cell.Parent(GeoS2Level)
		}
		var first, last s2.CellID
		if cell.Level() == GeoS2Level {
			first, last = cell, cell
		} else {
			first = cell.ChildBeginAtLevel(GeoS2Level)
			last = cell.ChildEndAtLevel(GeoS2Level).Prev()
		}
		ranges = append(ranges, S2KeyRange{Start: EncodeS2Key(uint64(first)), End: EncodeS2Key(uint64(last))})
	}
	return NormalizeRanges(ranges), nil
}

func NormalizeRanges(ranges []S2KeyRange) []S2KeyRange {
	if len(ranges) == 0 {
		return nil
	}
	if rangesNormalized(ranges) {
		return ranges
	}
	result := append([]S2KeyRange(nil), ranges...)
	for i := range result {
		if result[i].End < result[i].Start {
			result[i].Start, result[i].End = result[i].End, result[i].Start
		}
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Start != result[j].Start {
			return result[i].Start < result[j].Start
		}
		return result[i].End < result[j].End
	})
	out := 1
	for i := 1; i < len(result); i++ {
		previous := &result[out-1]
		// Valid level-16 keys differ by two. Joining an adjacent Hilbert
		// interval therefore allows one unused integer between ranges.
		if result[i].Start <= previous.End || previous.End <= ^S2Key(0)-2 && result[i].Start <= previous.End+2 {
			if result[i].End > previous.End {
				previous.End = result[i].End
			}
			continue
		}
		result[out] = result[i]
		out++
	}
	return result[:out]
}

func rangesNormalized(ranges []S2KeyRange) bool {
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
