package bitmapidx

import (
	"math"
	"slices"
	"sync"
	"testing"

	"github.com/golang/geo/s1"
	"github.com/golang/geo/s2"
)

func geoFieldFor(t IndexType) Field {
	return Field{
		Type:    t,
		Kind:    KindGeo,
		Storage: StoragePerType,
		Geo:     GeoCodec{Version: 1},
	}
}

func sfKey(t *testing.T) uint64 {
	t.Helper()
	key, err := KeyFromLatLng(37.7749, -122.4194)
	if err != nil {
		t.Fatalf("KeyFromLatLng: %v", err)
	}
	return key
}

func TestGeoKeyConversion(t *testing.T) {
	key := sfKey(t)
	cell, err := CellIDFromKey(key)
	if err != nil {
		t.Fatalf("CellIDFromKey: %v", err)
	}
	if cell.Level() != GeoS2Level {
		t.Fatalf("level = %d, want %d", cell.Level(), GeoS2Level)
	}
	if uint64(cell)>>geoKeyShift != key {
		t.Fatalf("key round-trip failed: %d", key)
	}
	if _, err := KeyFromCellID(uint64(cell)); err != nil {
		t.Fatalf("KeyFromCellID: %v", err)
	}
	if _, err := KeyFromCellID(uint64(cell.Parent(10))); err != ErrInvalidS2Level {
		t.Fatalf("coarse cell error = %v, want ErrInvalidS2Level", err)
	}
	if got := CellIDFromLatLng(37.7749, -122.4194); got == 0 || got != DecodeS2Key(key) {
		t.Fatalf("CellIDFromLatLng = %#x, want %#x", got, DecodeS2Key(key))
	}
	if got := CellIDFromLatLng(math.NaN(), 0); got != 0 {
		t.Fatalf("invalid coordinate cell ID = %#x, want 0", got)
	}
	if _, err := CellIDFromKey(1 << 40); err == nil {
		t.Fatal("oversized key accepted")
	}
	for _, coordinate := range [][2]float64{{math.NaN(), 0}, {91, 0}, {0, 181}} {
		if _, err := KeyFromLatLng(coordinate[0], coordinate[1]); err != ErrInvalidCoordinate {
			t.Fatalf("coordinate %v error = %v, want ErrInvalidCoordinate", coordinate, err)
		}
	}
}

func TestGeoRangesFromCellIDs(t *testing.T) {
	cell := s2.CellIDFromLatLng(s2.LatLngFromDegrees(37.7749, -122.4194)).Parent(10)
	ranges, err := RangesFromCellIDs(nil, []uint64{uint64(cell)})
	if err != nil {
		t.Fatalf("RangesFromCellIDs: %v", err)
	}
	if len(ranges) != 1 || ranges[0].End < ranges[0].Start {
		t.Fatalf("ranges = %+v", ranges)
	}
	key := sfKey(t)
	if key < ranges[0].Start || key > ranges[0].End {
		t.Fatalf("city key %d outside its level-10 cell range %+v", key, ranges[0])
	}
	// Hilbert-adjacent level-10 cells tile bucket space with one unoccupiable
	// even key between them (even keys never decode to level 16), so exact
	// normalization keeps two ranges with identical query results.
	north := cell.Next()
	split, err := RangesFromCellIDs(nil, []uint64{uint64(cell), uint64(north)})
	if err != nil {
		t.Fatalf("RangesFromCellIDs: %v", err)
	}
	if len(split) != 2 {
		t.Fatalf("adjacent cells = %+v, want 2 exact ranges", split)
	}
	gap := split[0].End + 1
	if gap+1 != split[1].Start {
		t.Fatalf("ranges %+v are not one key apart", split)
	}
	// The seam key is even, so it can never be a stored bucket: valid
	// level-16 ids have exactly 28 trailing zeros (odd keys). The exact
	// rejection depends on trailing-zero parity, so accept either class.
	if _, err := CellIDFromKey(gap); err != ErrInvalidS2Cell && err != ErrInvalidS2Level {
		t.Fatalf("gap key error = %v, want ErrInvalidS2Cell or ErrInvalidS2Level", err)
	}
	if _, err := RangesFromCellIDs(nil, []uint64{0}); err != ErrInvalidS2Cell {
		t.Fatalf("invalid cell error = %v", err)
	}

	// A raw (level-30) cell snaps to its level-16 ancestor by default.
	raw := s2.CellIDFromLatLng(s2.LatLngFromDegrees(37.7749, -122.4194))
	snapped, err := RangesFromCellIDs(nil, []uint64{uint64(raw)})
	if err != nil {
		t.Fatalf("snap: %v", err)
	}
	wantSnap, err := RangesFromCellIDs(nil, []uint64{uint64(raw.Parent(GeoS2Level))})
	if err != nil {
		t.Fatalf("snap reference: %v", err)
	}
	if !slices.Equal(snapped, wantSnap) {
		t.Fatalf("snapped = %+v, want %+v", snapped, wantSnap)
	}

	// MaxLevel rejects cells that are too coarse for the caller.
	if _, err := RangesFromCellIDs(nil, []uint64{uint64(cell)}, RangeOptions{MaxLevel: 12}); err != ErrInvalidS2Level {
		t.Fatalf("too coarse error = %v, want ErrInvalidS2Level", err)
	}
	if _, err := RangesFromCellIDs(nil, []uint64{uint64(cell)}, RangeOptions{MaxLevel: 17}); err != ErrInvalidS2Level {
		t.Fatalf("oversized level error = %v, want ErrInvalidS2Level", err)
	}
	// MaxCells budgets the descendant buckets per input cell. A level-10 cell
	// expands to 4^6 = 4096 level-16 buckets.
	if _, err := RangesFromCellIDs(nil, []uint64{uint64(cell)}, RangeOptions{MaxCells: 4096}); err != nil {
		t.Fatalf("exact budget: %v", err)
	}
	if _, err := RangesFromCellIDs(nil, []uint64{uint64(cell)}, RangeOptions{MaxCells: 4095}); err != ErrTooManyCells {
		t.Fatalf("over budget error = %v, want ErrTooManyCells", err)
	}
	if _, err := RangesFromCellIDs(nil, []uint64{uint64(s2.CellIDFromFace(cell.Face()))}, RangeOptions{MaxCells: 1 << 20}); err != ErrTooManyCells {
		t.Fatalf("face budget error = %v, want ErrTooManyCells", err)
	}
	// Snapped cells count as one bucket.
	if _, err := RangesFromCellIDs(nil, []uint64{uint64(raw)}, RangeOptions{MaxCells: 1}); err != nil {
		t.Fatalf("snapped budget: %v", err)
	}
	if _, err := RangesFromCellIDs(nil, []uint64{uint64(cell)}, RangeOptions{MaxCells: -1}); err != ErrInvalidConfig {
		t.Fatalf("negative budget error = %v, want ErrInvalidConfig", err)
	}
	// minLevel is private and defaults to the bucket level.
	if opts, err := (RangeOptions{}).withDefaults(); err != nil || opts.minLevel != GeoS2Level {
		t.Fatalf("default minLevel = %d, %v", opts.minLevel, err)
	}
	// An internal finer minLevel snaps before expanding to bucket ranges.
	snappedFiner, err := RangesFromCellIDs(nil, []uint64{uint64(raw)}, RangeOptions{minLevel: 20})
	if err != nil {
		t.Fatalf("minLevel snap: %v", err)
	}
	wantFiner, err := RangesFromCellIDs(nil, []uint64{uint64(raw.Parent(GeoS2Level))})
	if err != nil {
		t.Fatalf("minLevel reference: %v", err)
	}
	if !slices.Equal(snappedFiner, wantFiner) {
		t.Fatalf("minLevel snap = %+v, want %+v", snappedFiner, wantFiner)
	}
}

// TestGeoRangeNormalizationExact locks the merge contract: overlapping and
// exactly adjacent ranges merge, but a gap of even one key stays split so
// normalization never covers buckets the caller did not request.
func TestGeoRangeNormalizationExact(t *testing.T) {
	cases := []struct {
		name string
		in   []GeoRange
		want []GeoRange
	}{
		{"overlap", []GeoRange{{10, 20}, {15, 25}}, []GeoRange{{10, 25}}},
		{"adjacent", []GeoRange{{10, 20}, {21, 25}}, []GeoRange{{10, 25}}},
		{"gap stays split", []GeoRange{{10, 20}, {22, 25}}, []GeoRange{{10, 20}, {22, 25}}},
		{"single gap stays split", []GeoRange{{10, 10}, {12, 12}}, []GeoRange{{10, 10}, {12, 12}}},
		{"unsorted", []GeoRange{{22, 25}, {10, 20}}, []GeoRange{{10, 20}, {22, 25}}},
		{"inverted", []GeoRange{{20, 10}}, []GeoRange{{10, 20}}},
		{"max end merges overlap", []GeoRange{{5, ^uint64(0)}, {7, 9}}, []GeoRange{{5, ^uint64(0)}}},
	}
	for _, tc := range cases {
		if got := normalizeGeoRanges(slices.Clone(tc.in), 0); !slices.Equal(got, tc.want) {
			t.Errorf("%s: got %+v, want %+v", tc.name, got, tc.want)
		}
	}
}

// TestGeoSplitRangesMatchMerged verifies that exactly-split ranges retrieve
// every image a gap-filled range would: the seam key between Hilbert-adjacent
// expansions is even and can never be stored, so splitting loses nothing.
func TestGeoSplitRangesMatchMerged(t *testing.T) {
	index := mustField(t, geoFieldFor(IndexTypeLocation), FieldConfig{})
	cell := s2.CellIDFromLatLng(s2.LatLngFromDegrees(37.7749, -122.4194)).Parent(10)
	north := cell.Next()
	keyA := uint64(cell.ChildBeginAtLevel(GeoS2Level)) >> geoKeyShift
	keyB := uint64(north.ChildBeginAtLevel(GeoS2Level)) >> geoKeyShift
	if err := index.Move(
		Move{ImageID: 1, To: int64(keyA)},
		Move{ImageID: 2, To: int64(keyB)},
	); err != nil {
		t.Fatalf("seed: %v", err)
	}
	ranges, err := RangesFromCellIDs(nil, []uint64{uint64(cell), uint64(north)})
	if err != nil {
		t.Fatalf("RangesFromCellIDs: %v", err)
	}
	if len(ranges) != 2 {
		t.Fatalf("ranges = %+v, want 2 exact ranges", ranges)
	}
	match, err := index.GeoRanges(ranges)
	if err != nil {
		t.Fatalf("GeoRanges: %v", err)
	}
	if ids := match.AppendTo(nil); !slices.Equal(ids, []ImageID{1, 2}) {
		t.Fatalf("split ranges matched %v, want [1 2]", ids)
	}
}

// TestGeoCoarseLevelQuery verifies that a query below the bucket level
// returns every image whose level-16 bucket descends from the coarse cell,
// across multiple buckets, while excluding sibling buckets.
func TestGeoCoarseLevelQuery(t *testing.T) {
	coarse := s2.CellIDFromLatLng(s2.LatLngFromDegrees(48.8566, 2.3522)).Parent(10)
	first := coarse.ChildBeginAtLevel(GeoS2Level)
	second := first.Next()
	insideEnd := coarse.ChildEndAtLevel(GeoS2Level).Prev()
	outside := coarse.ChildEndAtLevel(GeoS2Level)

	center := func(cell s2.CellID) (float64, float64) {
		latLng := cell.LatLng()
		return latLng.Lat.Degrees(), latLng.Lng.Degrees()
	}
	points := []struct {
		id   ImageID
		cell s2.CellID
	}{
		{1, first},
		{2, second},
		{3, second}, // duplicate bucket
		{4, insideEnd},
		{5, outside},      // first bucket of the next coarse cell
		{6, first.Prev()}, // last bucket of the previous coarse cell
	}
	entries := make([]GeoEntry, 0, len(points))
	for _, point := range points {
		latitude, longitude := center(point.cell)
		entries = append(entries, GeoEntry{ImageID: point.id, Latitude: latitude, Longitude: longitude})
	}
	field, err := NewGeoField(IndexTypeLocation, entries, FieldConfig{})
	if err != nil {
		t.Fatalf("NewGeoField: %v", err)
	}
	defer field.Close()

	ranges, err := RangesFromCellIDs(nil, []uint64{uint64(coarse)})
	if err != nil {
		t.Fatalf("RangesFromCellIDs: %v", err)
	}
	match, err := field.GeoRanges(ranges)
	if err != nil {
		t.Fatalf("GeoRanges: %v", err)
	}
	if got := match.AppendTo(nil); !slices.Equal(got, []ImageID{1, 2, 3, 4}) {
		t.Fatalf("level-10 query = %v, want [1 2 3 4]", got)
	}
	count, err := field.CountGeoRanges(ranges)
	if err != nil || count != 4 {
		t.Fatalf("CountGeoRanges = %d, %v", count, err)
	}
	filtered, err := field.FilterGeoRanges(nil, []ImageID{1, 2, 3, 4, 5, 6}, ranges)
	if err != nil || !slices.Equal(filtered, []ImageID{1, 2, 3, 4}) {
		t.Fatalf("FilterGeoRanges = %v, %v", filtered, err)
	}
	if err := field.Compact(); err != nil {
		t.Fatalf("Compact: %v", err)
	}
	estimate, err := field.EstimateGeoRanges(ranges)
	if err != nil || estimate != 4 {
		t.Fatalf("EstimateGeoRanges = %d, %v", estimate, err)
	}
	if cells := field.CountGeoCells(ranges); cells != 3 {
		t.Fatalf("CountGeoCells = %d, want 3 occupied buckets", cells)
	}

	// Exact bucket queries resolve to their own members, including siblings
	// just outside the coarse cell.
	for _, check := range []struct {
		name string
		cell s2.CellID
		want []ImageID
	}{
		{"first bucket", first, []ImageID{1}},
		{"second bucket", second, []ImageID{2, 3}},
		{"last bucket", insideEnd, []ImageID{4}},
		{"next coarse cell", outside, []ImageID{5}},
		{"previous coarse cell", first.Prev(), []ImageID{6}},
	} {
		exact, err := RangesFromCellIDs(nil, []uint64{uint64(check.cell)})
		if err != nil {
			t.Fatalf("%s ranges: %v", check.name, err)
		}
		match, err := field.GeoRanges(exact)
		if err != nil {
			t.Fatalf("%s query: %v", check.name, err)
		}
		if got := match.AppendTo(nil); !slices.Equal(got, check.want) {
			t.Fatalf("%s = %v, want %v", check.name, got, check.want)
		}
	}
}

func TestGeoFieldMoveAndValidation(t *testing.T) {
	index := mustField(t, geoFieldFor(IndexTypeLocation), FieldConfig{})
	sanFrancisco := sfKey(t)
	losAngeles, err := KeyFromLatLng(34.0522, -118.2437)
	if err != nil {
		t.Fatalf("KeyFromLatLng: %v", err)
	}

	// Geo presence flags may be omitted: a level-16 key is never zero.
	if err := index.Move(Move{ImageID: 1, To: int64(sanFrancisco)}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	// Re-inserting the same cell is rejected; a cross-cell insert without a
	// From is caller discipline because no reverse index is stored.
	if err := index.Add(Entry{ImageID: 1, Value: GeoValue(sanFrancisco)}); err != ErrDuplicateImageID {
		t.Fatalf("duplicate insert error = %v, want ErrDuplicateImageID", err)
	}
	// Explicit flags still work and report stale transitions.
	if err := index.Move(Move{ImageID: 1, From: int64(losAngeles), To: int64(sanFrancisco), HasFrom: true, HasTo: true}); err != ErrInvalidMutation {
		t.Fatalf("stale From error = %v, want ErrInvalidMutation", err)
	}
	if err := index.Move(Move{ImageID: 1, From: int64(sanFrancisco), To: int64(losAngeles)}); err != nil {
		t.Fatalf("move: %v", err)
	}
	// Batched double-add is rejected before publication.
	if err := index.Add(
		Entry{ImageID: 2, Value: GeoValue(sanFrancisco)},
		Entry{ImageID: 2, Value: GeoValue(losAngeles)},
	); err != ErrDuplicateImageID {
		t.Fatalf("batched double-add error = %v, want ErrDuplicateImageID", err)
	}
	if err := index.Move(Move{ImageID: 1, From: int64(sanFrancisco)}); err != ErrInvalidMutation {
		t.Fatalf("remove from wrong cell error = %v, want ErrInvalidMutation", err)
	}
	if err := index.Move(Move{ImageID: 1, From: int64(losAngeles)}); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if err := index.Move(Move{ImageID: 1, To: 0}); err != nil {
		t.Fatalf("zero-target no-op: %v", err)
	}
	if ids := appendIDs(t, index, GeoValue(losAngeles)); len(ids) != 0 {
		t.Fatalf("removed image still present: %v", ids)
	}
	// Continuous fields keep explicit flags because zero is a valid value.
	focals := mustField(t, continuousField(testFocalLength), FieldConfig{})
	if err := focals.Move(Move{ImageID: 1, To: 0}); err != nil {
		t.Fatalf("continuous no-op: %v", err)
	}
	if present, err := focals.Contains(1, IntValue(0)); err != nil || present {
		t.Fatalf("continuous zero inserted without a flag: present=%v err=%v", present, err)
	}
}

func TestFailedBatchDiscardsStagedDictionary(t *testing.T) {
	index := mustField(t, geoFieldFor(IndexTypeLocation), FieldConfig{})
	sanFrancisco := sfKey(t)
	if err := index.Move(Move{ImageID: 1, To: int64(sanFrancisco)}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	// Simulate a value staged by a batch that then fails validation: without
	// an abort the next successful write would seal it into the dictionary
	// even though its postings were never published.
	phantom := FieldKey{Hash: 0x12345678, Type: IndexTypeLocation}
	index.dict.stage(phantom, []byte("phantom"))
	losAngeles, err := KeyFromLatLng(34.0522, -118.2437)
	if err != nil {
		t.Fatalf("KeyFromLatLng: %v", err)
	}
	if err := index.Move(Move{ImageID: 1, From: int64(losAngeles), To: int64(sanFrancisco), HasFrom: true, HasTo: true}); err != ErrInvalidMutation {
		t.Fatalf("stale From error = %v, want ErrInvalidMutation", err)
	}
	if len(index.dict.pending) != 0 {
		t.Fatalf("failed batch left %d staged entries", len(index.dict.pending))
	}
	if err := index.Move(Move{ImageID: 1, From: int64(sanFrancisco), To: int64(losAngeles)}); err != nil {
		t.Fatalf("move: %v", err)
	}
	if _, ok := index.view.Load().dict.resolve(phantom, []byte("phantom")); ok {
		t.Fatal("failed batch value survived in the dictionary")
	}
}

func TestGeoFieldRangesCountsAndDeltas(t *testing.T) {
	config := FieldConfig{ActiveTarget: 2, HardDeltaLimit: 4, MaxDeltaValues: 2}
	index := mustField(t, geoFieldFor(IndexTypeLocation), config)
	type city struct {
		id   ImageID
		lat  float64
		lng  float64
		name string
	}
	cities := []city{
		{1, 37.7749, -122.4194, "San Francisco"},
		{2, 34.0522, -118.2437, "Los Angeles"},
		{3, 40.7128, -74.0060, "New York"},
		{4, 48.8566, 2.3522, "Paris"},
	}
	for _, c := range cities {
		key, err := KeyFromLatLng(c.lat, c.lng)
		if err != nil {
			t.Fatalf("KeyFromLatLng(%s): %v", c.name, err)
		}
		if err := index.Move(Move{ImageID: c.id, To: int64(key)}); err != nil {
			t.Fatalf("insert %s: %v", c.name, err)
		}
	}
	// Force delta rotation and compaction with the remaining moves.
	if err := index.Move(Move{ImageID: 5, To: int64(sfKey(t))}); err != nil {
		t.Fatalf("insert 5: %v", err)
	}
	if err := index.Compact(); err != nil {
		t.Fatalf("Compact: %v", err)
	}

	cell := s2.CellIDFromLatLng(s2.LatLngFromDegrees(37.7749, -122.4194)).Parent(10)
	ranges, err := RangesFromCellIDs(nil, []uint64{uint64(cell)})
	if err != nil {
		t.Fatalf("RangesFromCellIDs: %v", err)
	}
	match, err := index.GeoRanges(ranges)
	if err != nil {
		t.Fatalf("GeoRanges: %v", err)
	}
	if got := match.AppendTo(nil); !slices.Equal(got, []ImageID{1, 5}) {
		t.Fatalf("San Francisco cell = %v, want [1 5]", got)
	}
	count, err := index.CountGeoRanges(ranges)
	if err != nil || count != 2 {
		t.Fatalf("CountGeoRanges = %d, %v", count, err)
	}
	estimate, err := index.EstimateGeoRanges(ranges)
	if err != nil || estimate != 2 {
		t.Fatalf("EstimateGeoRanges = %d, %v", estimate, err)
	}
	candidates := []ImageID{1, 2, 3, 4, 5}
	filtered, err := index.FilterGeoRanges(nil, candidates, ranges)
	if err != nil || !slices.Equal(filtered, []ImageID{1, 5}) {
		t.Fatalf("FilterGeoRanges = %v, %v", filtered, err)
	}
	all := index.AppendTo(nil)
	if !slices.Equal(all, []ImageID{1, 2, 3, 4, 5}) {
		t.Fatalf("AppendTo = %v", all)
	}
	if err := index.Vacuum(); err != nil {
		t.Fatalf("Vacuum: %v", err)
	}
	match, err = index.GeoRanges(ranges)
	if err != nil {
		t.Fatalf("GeoRanges after vacuum: %v", err)
	}
	if got := match.AppendTo(nil); !slices.Equal(got, []ImageID{1, 5}) {
		t.Fatalf("after vacuum = %v", got)
	}
}

func TestGeoCoverAndNearest(t *testing.T) {
	index := mustField(t, geoFieldFor(IndexTypeLocation), FieldConfig{})
	keys := make(map[ImageID]uint64)
	for id, coordinate := range map[ImageID][2]float64{
		1: {37.7749, -122.4194}, // San Francisco
		2: {37.8044, -122.2712}, // Oakland
		3: {34.0522, -118.2437}, // Los Angeles
	} {
		key, err := KeyFromLatLng(coordinate[0], coordinate[1])
		if err != nil {
			t.Fatalf("KeyFromLatLng: %v", err)
		}
		keys[id] = key
		if err := index.Move(Move{ImageID: id, To: int64(key)}); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}

	bayArea := s2.RectFromLatLng(s2.LatLngFromDegrees(37.5, -122.6)).AddPoint(s2.LatLngFromDegrees(38.1, -122.0))
	workspace := NewGeoCoverWorkspace(64)
	ranges := make([]GeoRange, 0, 64)
	ranges, err := CoverRegion(ranges, bayArea, GeoCoverOptions{MaxCells: 32, Workspace: workspace})
	if err != nil {
		t.Fatalf("CoverRegion: %v", err)
	}
	match, err := index.GeoRanges(ranges)
	if err != nil {
		t.Fatalf("GeoRanges: %v", err)
	}
	if got := match.AppendTo(nil); !slices.Equal(got, []ImageID{1, 2}) {
		t.Fatalf("bay area = %v, want [1 2]", got)
	}
	if _, err := CoverRegion(nil, nil, GeoCoverOptions{}); err != ErrInvalidRegion {
		t.Fatalf("nil region error = %v, want ErrInvalidRegion", err)
	}
	capped, err := CoverRegion(nil, s2.CapFromCenterAngle(
		s2.PointFromLatLng(s2.LatLngFromDegrees(37.7749, -122.4194)), s1.Angle(0.001)), GeoCoverOptions{})
	if err != nil || len(capped) == 0 {
		t.Fatalf("cap cover = %+v, %v", capped, err)
	}

	lookup := func(id ImageID) (float64, float64, bool) {
		coordinate, ok := map[ImageID][2]float64{
			1: {37.7749, -122.4194},
			2: {37.8044, -122.2712},
			3: {34.0522, -118.2437},
		}[id]
		if !ok {
			return 0, 0, false
		}
		return coordinate[0], coordinate[1], true
	}
	nearest := index.Nearest(nil, 37.7749, -122.4194, 2, lookup)
	if len(nearest) != 2 || nearest[0].ImageID != 1 || nearest[1].ImageID != 2 {
		t.Fatalf("Nearest = %+v", nearest)
	}
	if nearest[0].DistanceMeters > nearest[1].DistanceMeters {
		t.Fatalf("Nearest not sorted: %+v", nearest)
	}
	if got := index.Nearest(nil, 37.7749, -122.4194, 0, lookup); len(got) != 0 {
		t.Fatalf("k=0 returned %+v", got)
	}
	_ = keys
}

// TestGeoReadersSurviveVacuum stresses AppendTo, EstimateGeoRanges, and
// GeoRanges against concurrent Vacuum generations. Each reader must hold its
// tracker token until the last snapshot access, or a retired mmap can be
// unmapped underneath it.
func TestGeoReadersSurviveVacuum(t *testing.T) {
	dir := t.TempDir()
	s, err := NewPersistentFields(dir, []Field{geoFieldFor(IndexTypeLocation)}, FieldConfig{}, DurabilityOS)
	if err != nil {
		t.Fatalf("NewPersistentFields: %v", err)
	}
	index, _ := s.Lookup(IndexTypeLocation)
	key := sfKey(t)
	for id := 1; id <= 64; id++ {
		if err := index.Move(Move{ImageID: ImageID(id), To: int64(key)}); err != nil {
			t.Fatalf("Move: %v", err)
		}
	}
	if err := s.Checkpoint(); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	ranges := []GeoRange{{Start: key, End: key}}
	var group sync.WaitGroup
	stop := make(chan struct{})
	for range 4 {
		group.Add(1)
		go func() {
			defer group.Done()
			for {
				select {
				case <-stop:
					return
				default:
					index.AppendTo(nil)
					index.EstimateGeoRanges(ranges)
					index.GeoRanges(ranges)
				}
			}
		}()
	}
	for n := 0; n < 16; n++ {
		if err := s.Vacuum(); err != nil {
			t.Errorf("Vacuum: %v", err)
			break
		}
	}
	close(stop)
	group.Wait()
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestGeoFieldDefinitionValidation(t *testing.T) {
	field := geoFieldFor(IndexTypeLocation)
	if err := validateField(field); err != nil {
		t.Fatalf("valid geo field rejected: %v", err)
	}
	field.Geo.Level = 10
	if err := validateField(field); err != ErrInvalidField {
		t.Fatalf("wrong level error = %v, want ErrInvalidField", err)
	}
	field = geoFieldFor(IndexTypeLocation)
	registry := NewRegistry()
	if err := registry.Register(field); err != nil {
		t.Fatalf("Register: %v", err)
	}
	data, err := registry.MarshalBinary()
	if err != nil {
		t.Fatalf("MarshalBinary: %v", err)
	}
	restored := NewRegistry()
	if err := restored.UnmarshalBinary(data); err != nil {
		t.Fatalf("UnmarshalBinary: %v", err)
	}
	restoredField, ok := restored.Field(IndexTypeLocation)
	if !ok || restoredField.Geo != field.Geo || restoredField.Kind != KindGeo {
		t.Fatalf("restored = %+v, ok=%v", restoredField, ok)
	}
}
