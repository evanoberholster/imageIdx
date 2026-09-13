package bitmapidx

import (
	"cmp"
	"math"
	"slices"
	"sort"

	"github.com/evanoberholster/imageIdx/roar"
	"github.com/golang/geo/s2"
)

const (
	// GeoS2Level is the fixed cell level of a KindGeo field.
	GeoS2Level = 16
	// DefaultGeoCoverCells bounds CoverRegion when MaxCells is zero.
	DefaultGeoCoverCells = 32

	geoKeyShift       = 28
	earthRadiusMeters = 6_371_008.8
)

// GeoRange is an inclusive interval of level-16 S2 keys.
type GeoRange struct {
	Start uint64
	End   uint64 // inclusive
}

// S2Key is a level-16 S2 cell key (a full cell ID shifted right by 28 bits).
type S2Key = uint64

// EncodeS2Key converts a full S2 cell ID to its level-16 key.
func EncodeS2Key(cellID uint64) S2Key { return cellID >> geoKeyShift }

// DecodeS2Key expands a level-16 S2 key back to its full cell ID.
func DecodeS2Key(key S2Key) uint64 { return uint64(key) << geoKeyShift }

// GeoEntry is one image coordinate used to seed a KindGeo field.
type GeoEntry struct {
	ImageID   ImageID
	Latitude  float64
	Longitude float64
}

// NewGeoField constructs a KindGeo field index seeded with entries. Use
// NewFieldIndex directly for full control of the field definition.
func NewGeoField(t IndexType, entries []GeoEntry, config FieldConfig) (*FieldIndex, error) {
	field := Field{Type: t, Kind: KindGeo, Storage: StoragePerType, Geo: GeoCodec{Version: 1}}
	index, err := NewFieldIndex(field, config)
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
	if err := index.Add(adds...); err != nil {
		_ = index.Close()
		return nil, err
	}
	return index, nil
}

// level16CellFromLatLng converts a coordinate to its level-16 S2 cell,
// rejecting non-finite and out-of-range input.
func level16CellFromLatLng(latitude, longitude float64) (s2.CellID, error) {
	if math.IsNaN(latitude) || math.IsNaN(longitude) || math.IsInf(latitude, 0) || math.IsInf(longitude, 0) ||
		latitude < -90 || latitude > 90 || longitude < -180 || longitude > 180 {
		return 0, ErrInvalidCoordinate
	}
	return s2.CellIDFromLatLng(s2.LatLngFromDegrees(latitude, longitude)).Parent(GeoS2Level), nil
}

// CellIDFromLatLng converts a coordinate to its level-16 S2 cell ID, ready for
// RangesFromCellIDs or search.WithinCells. Non-finite or out-of-range
// coordinates return zero, which those APIs reject.
func CellIDFromLatLng(latitude, longitude float64) uint64 {
	cell, err := level16CellFromLatLng(latitude, longitude)
	if err != nil {
		return 0
	}
	return uint64(cell)
}

// KeyFromLatLng converts a coordinate to its level-16 S2 key.
func KeyFromLatLng(latitude, longitude float64) (uint64, error) {
	cell, err := level16CellFromLatLng(latitude, longitude)
	if err != nil {
		return 0, err
	}
	return uint64(cell) >> geoKeyShift, nil
}

// KeyFromCellID converts a level-16 S2 cell ID to its key.
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

// CellIDFromKey converts a level-16 S2 key back to its cell ID.
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

// RangeOptions bounds an S2 range expansion. The zero value accepts every
// level with no cell budget.
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
	// smallest size the index stores; the field exists so an internal
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

// RangesFromCellIDs appends normalized level-16 descendant ranges for cells.
// Cells coarser than GeoS2Level expand to their level-16 descendants; finer
// cells (for example a raw CellIDFromLatLng result) snap to their level-16
// ancestor, which is a safe superset of the requested area. Overlapping or
// adjacent ranges merge; ranges with a gap stay split, so the result covers
// exactly the requested buckets. Pass RangeOptions to reject overly coarse
// cells or to cap the number of buckets covered.
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

// geoRangesMerge reports whether the inclusive range ending at prevEnd absorbs
// a range starting at nextStart: on overlap or exact adjacency. A gap of even
// one key stays split, so normalization never covers buckets the caller did
// not request.
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
		if ranges[i].Start <= previous.End || geoRangesMerge(previous.End, ranges[i].Start) {
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
		if current.Start <= previous.End || geoRangesMerge(previous.End, current.Start) {
			return false
		}
	}
	return true
}

// geoKeys returns the occupied keys inside ranges from every layer.
func (v *fieldView) geoKeys(ranges []GeoRange) []FieldKey {
	var keys []FieldKey
	collect := func(entries []FieldKey) {
		for _, keyRange := range ranges {
			start, end := geoBaseRangeIn(entries, keyRange)
			keys = append(keys, entries[start:end]...)
		}
	}
	collect(v.base.keys)
	for _, delta := range []*fieldDelta{v.frozen, v.active} {
		if delta == nil {
			continue
		}
		collect(delta.keys)
	}
	if len(keys) < 2 {
		return keys
	}
	slices.SortFunc(keys, compareFieldKey)
	return slices.Compact(keys)
}

// allKeys returns one copy of every occupied key in the field.
func (v *fieldView) allKeys() []FieldKey {
	var keys []FieldKey
	keys = append(keys, v.base.keys...)
	for _, delta := range []*fieldDelta{v.frozen, v.active} {
		if delta == nil {
			continue
		}
		keys = append(keys, delta.keys...)
	}
	if len(keys) < 2 {
		return keys
	}
	slices.SortFunc(keys, compareFieldKey)
	return slices.Compact(keys)
}

// geoBaseRangeIn returns the sorted index span inside an inclusive key range.
func geoBaseRangeIn(entries []FieldKey, keyRange GeoRange) (int, int) {
	if keyRange.End < keyRange.Start {
		return 0, 0
	}
	start := sort.Search(len(entries), func(i int) bool { return entries[i].Hash >= keyRange.Start })
	end := sort.Search(len(entries), func(i int) bool { return entries[i].Hash > keyRange.End })
	if start > end {
		return end, end
	}
	return start, end
}

// GeoRanges returns images inside any inclusive key range. Ranges may be
// unnormalized and overlapping; the result is sorted and unique.
func (i *FieldIndex) GeoRanges(ranges []GeoRange) (result roar.Bitmap, resultErr error) {
	if i.Field().Kind != KindGeo {
		return roar.Bitmap{}, ErrFieldMismatch
	}
	if len(ranges) == 0 {
		return roar.Bitmap{}, nil
	}
	token := i.readers.Acquire()
	defer i.readers.Release(token)
	defer i.ownResult(&result)
	view := i.view.Load()
	if i.closed.Load() {
		return roar.Bitmap{}, nil
	}
	if !view.hasDeltas() {
		postings := acquirePostings(view.countGeoKeys(ranges))
		for _, keyRange := range ranges {
			start, end := geoBaseRangeIn(view.base.keys, keyRange)
			for index := start; index < end; index++ {
				*postings = append(*postings, &view.base.postings[index])
			}
		}
		out := unionPostings(*postings)
		releasePostings(postings)
		return out, nil
	}
	keys := view.geoKeys(ranges)
	if len(keys) == 0 {
		return roar.Bitmap{}, nil
	}
	postings := acquirePostings(len(keys))
	defer releasePostings(postings)
	for _, key := range keys {
		if p, ok := view.effective(key); ok {
			*postings = append(*postings, p)
		}
	}
	return unionPostings(*postings), nil
}

// FilterGeoRanges appends candidates inside any key range to dst. It probes
// each candidate against the covered buckets instead of materializing the
// union, so it allocates nothing.
func (i *FieldIndex) FilterGeoRanges(dst, candidates []ImageID, ranges []GeoRange) ([]ImageID, error) {
	if i.Field().Kind != KindGeo {
		return dst, ErrFieldMismatch
	}
	token := i.readers.Acquire()
	view := i.view.Load()
	if !i.closed.Load() {
		for _, id := range candidates {
			if view.geoContains(ranges, id) {
				dst = append(dst, id)
			}
		}
	}
	i.readers.Release(token)
	return dst, nil
}

// EstimateGeoRanges returns the effective candidate count for the occupied
// buckets inside ranges. It is a planning hint, not a materialized count.
func (i *FieldIndex) EstimateGeoRanges(ranges []GeoRange) (uint64, error) {
	if i.Field().Kind != KindGeo {
		return 0, ErrFieldMismatch
	}
	token := i.readers.Acquire()
	view := i.view.Load()
	if i.closed.Load() {
		i.readers.Release(token)
		return 0, nil
	}
	if view.hasDeltas() {
		i.readers.Release(token)
		return view.estimateKeys(view.geoKeys(ranges)), nil
	}
	var estimate uint64
	for _, keyRange := range ranges {
		start, end := geoBaseRangeIn(view.base.keys, keyRange)
		for _, p := range view.base.postings[start:end] {
			estimate += p.cardinality()
		}
	}
	i.readers.Release(token)
	return estimate, nil
}

// CountGeoRanges returns the number of images inside any key range without
// materializing the union.
func (i *FieldIndex) CountGeoRanges(ranges []GeoRange) (uint64, error) {
	if i.Field().Kind != KindGeo {
		return 0, ErrFieldMismatch
	}
	token := i.readers.Acquire()
	defer i.readers.Release(token)
	view := i.view.Load()
	if i.closed.Load() {
		return 0, nil
	}
	mutable := acquireMutable()
	defer releaseMutable(mutable)
	if !view.hasDeltas() {
		for _, keyRange := range ranges {
			start, end := geoBaseRangeIn(view.base.keys, keyRange)
			for _, p := range view.base.postings[start:end] {
				p.addTo(mutable)
			}
		}
		return mutable.Cardinality(), nil
	}
	view.forEachGeoKey(ranges, func(key FieldKey) {
		if p, ok := view.effective(key); ok {
			p.addTo(mutable)
		}
	})
	return mutable.Cardinality(), nil
}

// CollectGeoRanges folds every effective posting inside ranges into m. The
// caller resets m first and materializes once with AppendTo.
func (i *FieldIndex) CollectGeoRanges(m *roar.MutableBitmap, ranges []GeoRange) {
	if m == nil || len(ranges) == 0 || i.Field().Kind != KindGeo {
		return
	}
	token := i.readers.Acquire()
	view := i.view.Load()
	if !i.closed.Load() {
		if !view.hasDeltas() {
			for _, keyRange := range ranges {
				start, end := geoBaseRangeIn(view.base.keys, keyRange)
				for index := start; index < end; index++ {
					view.base.postings[index].addTo(m)
				}
			}
		} else {
			view.forEachGeoKey(ranges, func(key FieldKey) {
				if p, ok := view.effective(key); ok {
					p.addTo(m)
				}
			})
		}
	}
	i.readers.Release(token)
}

// CountGeoCells returns the number of occupied level-16 buckets inside any
// key range. It is the geo verify-versus-collect planning probe; visible
// corrections may be counted in more than one layer, so it is an upper bound.
func (i *FieldIndex) CountGeoCells(ranges []GeoRange) int {
	if len(ranges) == 0 || i.Field().Kind != KindGeo {
		return 0
	}
	token := i.readers.Acquire()
	view := i.view.Load()
	count := 0
	if !i.closed.Load() {
		count = view.countGeoKeys(ranges)
	}
	i.readers.Release(token)
	return count
}

// forEachGeoKey calls fn for every occupied key inside ranges without
// allocating. Keys may repeat across layers; callers must be idempotent.
func (v *fieldView) forEachGeoKey(ranges []GeoRange, fn func(FieldKey)) {
	for _, keyRange := range ranges {
		if keyRange.End < keyRange.Start {
			continue
		}
		start, end := geoBaseRangeIn(v.base.keys, keyRange)
		for _, key := range v.base.keys[start:end] {
			fn(key)
		}
	}
	for _, delta := range []*fieldDelta{v.frozen, v.active} {
		if delta == nil {
			continue
		}
		for _, key := range delta.keys {
			if key.Slot != 0 {
				continue
			}
			for _, keyRange := range ranges {
				if key.Hash >= keyRange.Start && key.Hash <= keyRange.End {
					fn(key)
					break
				}
			}
		}
	}
}

// countGeoKeys counts occupied keys inside ranges without allocating.
func (v *fieldView) countGeoKeys(ranges []GeoRange) int {
	count := 0
	for _, keyRange := range ranges {
		if keyRange.End < keyRange.Start {
			continue
		}
		start, end := geoBaseRangeIn(v.base.keys, keyRange)
		count += end - start
	}
	for _, delta := range []*fieldDelta{v.frozen, v.active} {
		if delta == nil {
			continue
		}
		for _, key := range delta.keys {
			if key.Slot != 0 {
				continue
			}
			for _, keyRange := range ranges {
				if key.Hash >= keyRange.Start && key.Hash <= keyRange.End {
					count++
					break
				}
			}
		}
	}
	return count
}

// geoContains reports whether id holds any bucket inside ranges, walking the
// view layers per key without materializing postings.
func (v *fieldView) geoContains(ranges []GeoRange, id ImageID) bool {
	for _, keyRange := range ranges {
		if keyRange.End < keyRange.Start {
			continue
		}
		if !v.hasDeltas() {
			start, end := geoBaseRangeIn(v.base.keys, keyRange)
			for _, p := range v.base.postings[start:end] {
				if p.contains(id) {
					return true
				}
			}
			continue
		}
		start, end := geoBaseRangeIn(v.base.keys, keyRange)
		for _, key := range v.base.keys[start:end] {
			if v.contains(key, id) {
				return true
			}
		}
	}
	for _, delta := range []*fieldDelta{v.frozen, v.active} {
		if delta == nil {
			continue
		}
		for _, key := range delta.keys {
			if key.Slot != 0 {
				continue
			}
			inRange := false
			for _, keyRange := range ranges {
				if key.Hash >= keyRange.Start && key.Hash <= keyRange.End {
					inRange = true
					break
				}
			}
			if inRange && v.contains(key, id) {
				return true
			}
		}
	}
	return false
}

// AppendTo appends every indexed image ID in ascending order. Capacity is
// reserved once from the effective estimate so per-key appends do not regrow
// the destination.
func (i *FieldIndex) AppendTo(dst []ImageID) []ImageID {
	token := i.readers.Acquire()
	view := i.view.Load()
	closed := i.closed.Load()
	i.readers.Release(token)
	if closed {
		return dst
	}
	if !view.hasDeltas() {
		var estimate uint64
		for index := range view.base.postings {
			estimate += view.base.postings[index].cardinality()
		}
		if estimate > 0 {
			dst = slices.Grow(dst, int(estimate))
		}
		start := len(dst)
		for index := range view.base.postings {
			dst = view.base.postings[index].appendTo(dst)
		}
		values := dst[start:]
		slices.Sort(values)
		values = slices.Compact(values)
		return dst[:start+len(values)]
	}
	keys := view.allKeys()
	if estimate := view.estimateKeys(keys); estimate > 0 {
		dst = slices.Grow(dst, int(estimate))
	}
	start := len(dst)
	for _, key := range keys {
		if p, ok := view.effective(key); ok {
			dst = p.appendTo(dst)
		}
	}
	values := dst[start:]
	slices.Sort(values)
	values = slices.Compact(values)
	return dst[:start+len(values)]
}

// validateGeoChanges enforces one cell per image before publication. Removals
// are validated first so a stale From reports ErrInvalidMutation rather than
// tripping the target-exists check.
func validateGeoChanges(view *fieldView, changes []stagedChange) error {
	if view.field.Kind != KindGeo {
		return nil
	}
	for _, change := range changes {
		for _, id := range change.remove {
			if !view.contains(change.key, id) {
				return ErrInvalidMutation
			}
		}
	}
	added := make(map[ImageID]struct{}, len(changes))
	for _, change := range changes {
		for _, id := range change.add {
			if _, ok := added[id]; ok {
				return ErrDuplicateImageID
			}
			added[id] = struct{}{}
			if view.contains(change.key, id) {
				return ErrDuplicateImageID
			}
		}
	}
	return nil
}

// GeoCoordinateLookup supplies canonical coordinates for nearest search.
type GeoCoordinateLookup func(ImageID) (latitude, longitude float64, ok bool)

// NearestResult is one named nearest match.
type NearestResult struct {
	ImageID        ImageID
	DistanceMeters float64
}

// Nearest returns the exact top K among indexed images for which lookup
// supplies canonical coordinates. It scans the whole effective set and sorts
// every candidate, so cost is O(N log N) in the field size; it suits modest
// fields, not full-catalog nearest search.
func (i *FieldIndex) Nearest(dst []NearestResult, latitude, longitude float64, k int, lookup GeoCoordinateLookup) []NearestResult {
	if i.Field().Kind != KindGeo || k <= 0 || lookup == nil {
		return dst
	}
	if _, err := KeyFromLatLng(latitude, longitude); err != nil {
		return dst
	}
	start := len(dst)
	for _, id := range i.AppendTo(nil) {
		lat, lng, ok := lookup(id)
		if !ok {
			continue
		}
		dst = append(dst, NearestResult{ImageID: id, DistanceMeters: haversineMeters(latitude, longitude, lat, lng)})
	}
	slices.SortFunc(dst[start:], func(a, b NearestResult) int {
		return cmp.Compare(a.DistanceMeters, b.DistanceMeters)
	})
	if len(dst)-start > k {
		dst = dst[:start+k]
	}
	return dst
}

func haversineMeters(lat1, lng1, lat2, lng2 float64) float64 {
	const radians = math.Pi / 180
	lat1 *= radians
	lat2 *= radians
	dLat := lat2 - lat1
	dLng := (lng2 - lng1) * radians
	a := math.Sin(dLat/2)*math.Sin(dLat/2) + math.Cos(lat1)*math.Cos(lat2)*math.Sin(dLng/2)*math.Sin(dLng/2)
	return earthRadiusMeters * 2 * math.Atan2(math.Sqrt(a), math.Sqrt(1-a))
}

type geoCoverCandidate struct {
	cell     s2.CellID
	terminal bool
}

// GeoCoverWorkspace owns reusable covering scratch. It is not safe for
// concurrent use and never grows during CoverRegion.
type GeoCoverWorkspace struct {
	candidates []geoCoverCandidate
}

// NewGeoCoverWorkspace allocates scratch for subsequent covers. Capacity
// should be at least the requested MaxCells and six.
func NewGeoCoverWorkspace(capacity int) *GeoCoverWorkspace {
	if capacity < 0 {
		capacity = 0
	}
	return &GeoCoverWorkspace{candidates: make([]geoCoverCandidate, 0, capacity)}
}

// GeoCoverOptions controls region covering.
type GeoCoverOptions struct {
	MaxCells  int
	Workspace *GeoCoverWorkspace
}

// CoverRegion appends sorted, normalized level-16 descendant ranges covering
// region. With a sufficiently sized Workspace and dst the coverer allocates
// nothing; a Region implementation may allocate inside its own methods.
func CoverRegion(dst []GeoRange, region s2.Region, options GeoCoverOptions) ([]GeoRange, error) {
	if region == nil {
		return dst, ErrInvalidRegion
	}
	maxCells := options.MaxCells
	if maxCells == 0 {
		maxCells = DefaultGeoCoverCells
	}
	if maxCells < 1 {
		return dst, ErrInvalidConfig
	}
	workspace := options.Workspace
	noGrow := workspace != nil
	if workspace == nil {
		workspace = NewGeoCoverWorkspace(max(maxCells, 6))
	}
	candidates, err := geoCoverCandidates(region, maxCells, workspace)
	if err != nil {
		return dst, err
	}
	if noGrow && cap(dst)-len(dst) < len(candidates) {
		return dst, ErrCoverBufferFull
	}
	start := len(dst)
	for _, candidate := range candidates {
		first, last := candidate.cell, candidate.cell
		if candidate.cell.Level() < GeoS2Level {
			first = candidate.cell.ChildBeginAtLevel(GeoS2Level)
			last = candidate.cell.ChildEndAtLevel(GeoS2Level).Prev()
		}
		keyRange := GeoRange{Start: uint64(first) >> geoKeyShift, End: uint64(last) >> geoKeyShift}
		if len(dst) != start {
			previous := &dst[len(dst)-1]
			if keyRange.Start <= previous.End || geoRangesMerge(previous.End, keyRange.Start) {
				if keyRange.End > previous.End {
					previous.End = keyRange.End
				}
				continue
			}
		}
		dst = append(dst, keyRange)
	}
	return dst, nil
}

func geoCoverCandidates(region s2.Region, maxCells int, workspace *GeoCoverWorkspace) ([]geoCoverCandidate, error) {
	candidates := workspace.candidates[:0]
	appendCandidate := func(candidate geoCoverCandidate) bool {
		if len(candidates) == cap(candidates) {
			return false
		}
		candidates = append(candidates, candidate)
		return true
	}
	for face := 0; face < 6; face++ {
		cellID := s2.CellIDFromFace(face)
		cell := s2.CellFromCellID(cellID)
		if region.IntersectsCell(cell) && !appendCandidate(geoCoverCandidate{cell: cellID, terminal: region.ContainsCell(cell)}) {
			return nil, ErrCoverWorkspaceFull
		}
	}
	for {
		selected := -1
		selectedLevel := GeoS2Level + 1
		for index, candidate := range candidates {
			level := candidate.cell.Level()
			if !candidate.terminal && level < selectedLevel {
				selected, selectedLevel = index, level
			}
		}
		if selected < 0 {
			break
		}
		parent := candidates[selected]
		if parent.cell.Level() >= GeoS2Level {
			candidates[selected].terminal = true
			continue
		}
		var children [4]geoCoverCandidate
		childCount := 0
		for _, childID := range parent.cell.Children() {
			child := s2.CellFromCellID(childID)
			if !region.IntersectsCell(child) {
				continue
			}
			children[childCount] = geoCoverCandidate{
				cell: childID, terminal: childID.Level() == GeoS2Level || region.ContainsCell(child),
			}
			childCount++
		}
		if childCount == 0 {
			candidates[selected] = candidates[len(candidates)-1]
			candidates = candidates[:len(candidates)-1]
			continue
		}
		if childCount != 1 && len(candidates)+childCount-1 > maxCells {
			candidates[selected].terminal = true
			continue
		}
		if len(candidates)+childCount-1 > cap(candidates) {
			return nil, ErrCoverWorkspaceFull
		}
		candidates[selected] = children[0]
		candidates = append(candidates, children[1:childCount]...)
	}
	slices.SortFunc(candidates, func(left, right geoCoverCandidate) int {
		return cmp.Compare(left.cell, right.cell)
	})
	workspace.candidates = candidates[:0]
	return candidates, nil
}
