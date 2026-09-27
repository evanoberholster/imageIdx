package bitmapidx

import (
	"cmp"
	"math"
	"slices"

	common "github.com/evanoberholster/imageIdx/common"
	"github.com/golang/geo/s2"
)

const (
	// GeoS2Level is the fixed cell level of a KindBitmapGeo namespace. All geo
	// postings bucket by level-16 keys; coarser inputs expand and finer
	// inputs snap to their level-16 ancestor.
	GeoS2Level = 16
	// DefaultGeoCoverCells bounds CoverRegion when MaxCells is zero.
	// Thirty-two balances region fidelity against bucket fan-out.
	DefaultGeoCoverCells = 32

	geoKeyShift       = 28
	earthRadiusMeters = 6_371_008.8
)

// GeoRange is an inclusive interval of level-16 S2 keys. Ranges from
// callers may be unnormalized and overlapping; query paths handle that,
// while normalization merges overlap and exact adjacency only.
type GeoRange struct {
	Start uint64
	End   uint64 // inclusive
}

// S2Key is a level-16 S2 cell key: a full cell ID shifted right by
// geoKeyShift (28 bits). Keys fit in int64 for Value transport.
type S2Key = uint64

// EncodeS2Key converts a full S2 cell ID to its level-16 key by shifting.
func EncodeS2Key(cellID uint64) S2Key { return cellID >> geoKeyShift }

// DecodeS2Key expands a level-16 S2 key back to its full cell ID by
// shifting. It does not validate level; use CellIDFromKey for that.
func DecodeS2Key(key S2Key) uint64 { return uint64(key) << geoKeyShift }

// GeoEntry is one image coordinate used to seed a KindBitmapGeo namespace via
// NewGeoNamespace.
type GeoEntry struct {
	ImageID   ImageID
	Latitude  float64
	Longitude float64
}

// NewGeoNamespace constructs a KindBitmapGeo namespace index seeded with entries. It
// builds a StoragePerType definition with GeoCodec version 1 and converts
// every coordinate through KeyFromLatLng. Use NewNamespaceIndex directly for
// full control of the definition. Failures close the partial index.
func NewGeoNamespace(t common.Namespace, entries []GeoEntry, config NamespaceConfig) (*NamespaceIndex, error) {
	spec := NamespaceSpec{Namespace: t, Kind: common.KindBitmapGeo, Storage: StoragePerType, Geo: GeoCodec{Version: 1}}
	index, err := NewNamespaceIndex(spec, config)
	if err != nil {
		return nil, err
	}
	if len(entries) == 0 {
		return index, nil
	}
	adds := make([]Entry, 0, len(entries))
	for _, entry := range entries {
		key, err := KeyFromLatLng(entry.Latitude, entry.Longitude)
		if err != nil {
			_ = index.Close()
			return nil, err
		}
		adds = append(adds, Entry{ImageID: entry.ImageID, Value: GeoValue(key)})
	}
	batch := index.NewBatch()
	defer batch.Close()
	if err := batch.Add(adds...); err != nil {
		_ = index.Close()
		return nil, err
	}
	if err := batch.Apply(); err != nil {
		_ = index.Close()
		return nil, err
	}
	if err := index.Barrier(); err != nil {
		_ = index.Close()
		return nil, err
	}
	return index, nil
}

// level16CellFromLatLng converts a coordinate to its level-16 S2 cell,
// rejecting non-finite and out-of-range input with ErrInvalidCoordinate.
func level16CellFromLatLng(latitude, longitude float64) (s2.CellID, error) {
	if math.IsNaN(latitude) || math.IsNaN(longitude) || math.IsInf(latitude, 0) || math.IsInf(longitude, 0) ||
		latitude < -90 || latitude > 90 || longitude < -180 || longitude > 180 {
		return 0, ErrInvalidCoordinate
	}
	return s2.CellIDFromLatLng(s2.LatLngFromDegrees(latitude, longitude)).Parent(GeoS2Level), nil
}

// CellIDFromLatLng converts a coordinate to its level-16 S2 cell ID,
// ready for RangesFromCellIDs or search.InCells. Non-finite or
// out-of-range coordinates return zero, which those APIs reject as
// ErrInvalidS2Cell.
func CellIDFromLatLng(latitude, longitude float64) uint64 {
	cell, err := level16CellFromLatLng(latitude, longitude)
	if err != nil {
		return 0
	}
	return uint64(cell)
}

// KeyFromLatLng converts a coordinate to its level-16 S2 key for GeoValue
// and Move targets.
func KeyFromLatLng(latitude, longitude float64) (uint64, error) {
	cell, err := level16CellFromLatLng(latitude, longitude)
	if err != nil {
		return 0, err
	}
	return uint64(cell) >> geoKeyShift, nil
}

// KeyFromCellID converts a level-16 S2 cell ID to its key, rejecting
// invalid cells and wrong levels.
func KeyFromCellID(cellID uint64) (uint64, error) {
	cell := s2.CellID(cellID)
	if !cell.IsValid() {
		return 0, ErrInvalidS2Cell
	}
	if cell.Level() != GeoS2Level {
		return 0, ErrInvalidS2Level
	}
	return uint64(cell) >> geoKeyShift, nil
}

// CellIDFromKey converts a level-16 S2 key back to its cell ID,
// verifying the round-trip and level. It guards segment, WAL, and query
// inputs against truncated or shifted keys.
func CellIDFromKey(key uint64) (s2.CellID, error) {
	cell := s2.CellID(key << geoKeyShift)
	if (uint64(cell)>>geoKeyShift) != key || !cell.IsValid() {
		return 0, ErrInvalidS2Cell
	}
	if cell.Level() != GeoS2Level {
		return 0, ErrInvalidS2Level
	}
	return cell, nil
}

// RangeOptions bounds an S2 range expansion in RangesFromCellIDs. The
// zero value accepts every level with no cell budget.
type RangeOptions struct {
	// MaxLevel is the coarsest accepted input cell level (smallest number).
	// Cells coarser than MaxLevel are rejected as too broad.
	MaxLevel uint8
	// MaxCells bounds the number of level-16 descendant buckets the expansion
	// may cover, summed per input cell before normalization. Zero means
	// unlimited.
	MaxCells int

	// minLevel is the finest tracked cell level (largest number, smallest
	// cell size). Zero defaults to GeoS2Level. Inputs finer than minLevel snap
	// to their minLevel ancestor. It is private because GeoS2Level is the
	// smallest size the index stores; the namespace exists so an internal
	// bucket-level change has one definition.
	minLevel uint8
}

func (o RangeOptions) withDefaults() (RangeOptions, error) {
	if o.minLevel == 0 {
		o.minLevel = GeoS2Level
	}
	if o.MaxLevel > o.minLevel {
		return RangeOptions{}, ErrInvalidS2Level
	}
	if o.MaxCells < 0 {
		return RangeOptions{}, ErrInvalidConfig
	}
	return o, nil
}

// RangesFromCellIDs appends normalized level-16 descendant ranges for
// cells, preserving dst's prefix and reordering only the appended tail in
// place. Cells coarser than GeoS2Level expand to descendants; finer cells
// snap to their level-16 ancestor, a safe superset. Overlap and exact
// adjacency merge; any gap stays split, so the result covers exactly the
// requested buckets. Budgets count descendants per input before
// normalization, so overlapping inputs charge twice. Failures truncate dst
// back to its prefix length.
func RangesFromCellIDs(dst []GeoRange, cellIDs []uint64, options ...RangeOptions) ([]GeoRange, error) {
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
		first, last := cell, cell
		switch {
		case cell.Level() < GeoS2Level:
			first = cell.ChildBeginAtLevel(GeoS2Level)
			last = cell.ChildEndAtLevel(GeoS2Level).Prev()
		case cell.Level() > GeoS2Level:
			cell = cell.Parent(GeoS2Level)
			first, last = cell, cell
		}
		if opts.MaxCells > 0 {
			descendants := uint64(1)
			if cell.Level() < GeoS2Level {
				descendants = uint64(1) << (2 * uint(GeoS2Level-cell.Level()))
			}
			if total+descendants > uint64(opts.MaxCells) {
				return dst[:start], ErrTooManyCells
			}
			total += descendants
		}
		dst = append(dst, GeoRange{
			Start: uint64(first) >> geoKeyShift,
			End:   uint64(last) >> geoKeyShift,
		})
	}
	return normalizeGeoRanges(dst, start), nil
}

// geoRangesMerge reports whether the inclusive range ending at prevEnd
// absorbs a range starting at nextStart: overlap or exact adjacency
// merges, while a gap of even one key stays split. It saturates at
// ^uint64(0) so MaxUint64 ranges never overflow.
func geoRangesMerge(prevEnd, nextStart uint64) bool {
	return prevEnd == ^uint64(0) || nextStart <= prevEnd+1
}

func normalizeGeoRanges(ranges []GeoRange, start int) []GeoRange {
	prefix := ranges[:start]
	ranges = ranges[start:]
	if len(ranges) == 0 {
		return prefix
	}
	if geoRangesNormalized(ranges) {
		return append(prefix, ranges...)
	}
	for i := range ranges {
		if ranges[i].End < ranges[i].Start {
			ranges[i].Start, ranges[i].End = ranges[i].End, ranges[i].Start
		}
	}
	slices.SortFunc(ranges, func(left, right GeoRange) int {
		if order := cmp.Compare(left.Start, right.Start); order != 0 {
			return order
		}
		return cmp.Compare(left.End, right.End)
	})
	out := 1
	for i := 1; i < len(ranges); i++ {
		previous := &ranges[out-1]
		if geoRangesMerge(previous.End, ranges[i].Start) {
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

func geoRangesNormalized(ranges []GeoRange) bool {
	for index, current := range ranges {
		if current.End < current.Start {
			return false
		}
		if index == 0 {
			continue
		}
		previous := ranges[index-1]
		if geoRangesMerge(previous.End, current.Start) {
			return false
		}
	}
	return true
}
