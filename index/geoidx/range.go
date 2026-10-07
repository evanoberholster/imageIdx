package geoidx

import (
	"cmp"
	"slices"

	"github.com/golang/geo/s1"
	"github.com/golang/geo/s2"
)

// s1AngleFromMeters converts a surface distance in meters to a central angle.
func s1AngleFromMeters(meters float64) s1.Angle { return s1.Angle(meters / earthRadiusMeters) }

// Range is an inclusive interval of S2 keys. Ranges supplied by callers may be
// unsorted, overlapping, or inverted; query paths normalize them before use.
type Range struct {
	Start S2Key
	End   S2Key // inclusive
}

// DefaultMaxCells bounds a region cover when MaxCells is unset. It balances
// cover fidelity against the number of ranges a query scans.
const DefaultMaxCells = 32

// CoverOptions configures region covers.
type CoverOptions struct {
	// MaxCells is the desired maximum number of cover cells. Values <= 0 select
	// DefaultMaxCells.
	MaxCells int
}

func (o CoverOptions) withDefaults() CoverOptions {
	if o.MaxCells <= 0 {
		o.MaxCells = DefaultMaxCells
	}
	return o
}

// RangeOptions bounds an S2 range expansion in RangesFromCellIDs. The zero
// value accepts every level with no cell budget.
type RangeOptions struct {
	// MaxLevel is the coarsest accepted input cell level (smallest number).
	// Cells coarser than MaxLevel are rejected as too broad. Zero accepts all.
	MaxLevel uint8
	// MaxCells bounds the number of level-S2Level descendants the expansion may
	// cover, summed per input cell before normalization. Zero means unlimited.
	MaxCells int

	// minLevel is the finest tracked cell level. Zero defaults to S2Level.
	// Inputs finer than minLevel snap to their minLevel ancestor.
	minLevel uint8
}

func (o RangeOptions) withDefaults() (RangeOptions, error) {
	if o.minLevel == 0 {
		o.minLevel = S2Level
	}
	if o.MaxLevel > o.minLevel {
		return RangeOptions{}, ErrInvalidS2Level
	}
	if o.MaxCells < 0 {
		return RangeOptions{}, ErrInvalidConfig
	}
	return o, nil
}

// RangesFromCellIDs appends normalized S2Level descendant ranges for cellIDs,
// preserving dst's prefix and reordering only the appended tail in place. Cells
// coarser than S2Level expand to their descendants; finer cells snap to their
// S2Level ancestor (a safe superset). Overlap and exact adjacency merge.
// Failures truncate dst back to its prefix length.
func RangesFromCellIDs(dst []Range, cellIDs []uint64, options ...RangeOptions) ([]Range, error) {
	var opts RangeOptions
	if len(options) != 0 {
		opts = options[0]
	}
	opts, err := opts.withDefaults()
	if err != nil {
		return dst, err
	}
	start := len(dst)
	total := uint64(0)
	for _, value := range cellIDs {
		cell := s2.CellID(value)
		if !cell.IsValid() {
			return dst[:start], ErrInvalidS2Cell
		}
		if cell.Level() < int(opts.MaxLevel) {
			return dst[:start], ErrInvalidS2Level
		}
		if cell.Level() > int(opts.minLevel) {
			cell = cell.Parent(int(opts.minLevel))
		}
		if opts.MaxCells > 0 {
			descendants := uint64(1)
			if cell.Level() < S2Level {
				descendants = uint64(1) << (2 * uint(S2Level-cell.Level()))
			}
			if total+descendants > uint64(opts.MaxCells) {
				return dst[:start], ErrTooManyCells
			}
			total += descendants
		}
		dst = append(dst, cellKeyRange(cell))
	}
	return normalizeRanges(dst, start), nil
}

// cellKeyRange returns the inclusive key interval covered by cell and all of its
// S2Level descendants.
func cellKeyRange(cell s2.CellID) Range {
	first, last := cell, cell
	if cell.Level() < S2Level {
		first = cell.ChildBeginAtLevel(S2Level)
		last = cell.ChildEndAtLevel(S2Level).Prev()
	}
	return Range{Start: EncodeKey(uint64(first)), End: EncodeKey(uint64(last))}
}

// rangesMerge reports whether the inclusive range ending at prevEnd absorbs a
// range starting at nextStart. Overlap and exact integer adjacency always merge.
// A single-key gap also merges when prevEnd is odd: valid S2Level keys are
// always odd (the level bit is set), so the sole gap key is even and can never be
// stored. Merging saturates at ^uint64(0) so MaxUint64 ranges never overflow.
func rangesMerge(prevEnd, nextStart S2Key) bool {
	if prevEnd == S2Key(^uint64(0)) {
		return true
	}
	if nextStart <= prevEnd+1 {
		return true
	}
	return prevEnd&1 == 1 && nextStart == prevEnd+2
}

// normalizeRanges sorts, un-inverts, and merges the tail dst[start:], returning
// dst with the merged tail. It never allocates: the merge is in place.
func normalizeRanges(dst []Range, start int) []Range {
	prefix := dst[:start]
	ranges := dst[start:]
	if len(ranges) == 0 {
		return prefix
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
		if rangesMerge(previous.End, ranges[i].Start) {
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

// CoverRegion appends the normalized S2Level key ranges covering region.
func CoverRegion(dst []Range, region s2.Region, options ...CoverOptions) []Range {
	var opts CoverOptions
	if len(options) != 0 {
		opts = options[0]
	}
	opts = opts.withDefaults()
	coverer := &s2.RegionCoverer{MinLevel: 0, MaxLevel: S2Level, LevelMod: 1, MaxCells: opts.MaxCells}
	covering := coverer.Covering(region)
	start := len(dst)
	for _, cell := range covering {
		dst = append(dst, cellKeyRange(cell))
	}
	return normalizeRanges(dst, start)
}

// CoverCap appends the normalized S2Level key ranges covering a spherical cap.
func CoverCap(dst []Range, c s2.Cap, options ...CoverOptions) []Range {
	return CoverRegion(dst, c, options...)
}

// CoverRect appends the normalized S2Level key ranges covering a rectangle.
func CoverRect(dst []Range, rect s2.Rect, options ...CoverOptions) []Range {
	return CoverRegion(dst, rect, options...)
}

// CoverPolygon appends the normalized S2Level key ranges covering a polygon.
func CoverPolygon(dst []Range, polygon *s2.Polygon, options ...CoverOptions) []Range {
	return CoverRegion(dst, polygon, options...)
}

// CapFromMeters returns the spherical cap centered on center with the given
// surface radius in meters.
func CapFromMeters(center s2.LatLng, radiusMeters float64) s2.Cap {
	return s2.CapFromCenterAngle(s2.PointFromLatLng(center), s1AngleFromMeters(radiusMeters))
}
