package search

import (
	"errors"
	"slices"
	"testing"

	"github.com/evanoberholster/imageIdx/bitmapidx"

	"github.com/evanoberholster/imageIdx/timeidx"
	"github.com/golang/geo/s2"
)

func TestCompileAndSearchCombinedQuery(t *testing.T) {
	quantum := uint64(1) << timeidx.TimestampShift
	bitmap, err := bitmapidx.New(bitmapidx.Spec{
		Key: bitmapidx.Key{Type: bitmapidx.IndexTypeTag, ValueID: 7}, IndexID: 1, IDs: []uint64{1, 2, 3, 5},
	}, bitmapidx.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer bitmap.Close()
	secondBitmap, err := bitmapidx.New(bitmapidx.Spec{
		Key: bitmapidx.Key{Type: bitmapidx.IndexTypeAlbum, ValueID: 8}, IndexID: 2, IDs: []uint64{2, 3, 4},
	}, bitmapidx.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer secondBitmap.Close()

	timeIndex, err := timeidx.New(timeidx.Spec{IndexID: 3, Entries: []timeidx.Entry{
		{Timestamp: quantum, ImageID: 1},
		{Timestamp: 2 * quantum, ImageID: 2},
		{Timestamp: 3 * quantum, ImageID: 3},
		{Timestamp: 4 * quantum, ImageID: 4},
		{Timestamp: 5 * quantum, ImageID: 5},
	}}, timeidx.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer timeIndex.Close()

	geo, err := bitmapidx.NewGeoField(bitmapidx.IndexType(2), []bitmapidx.GeoEntry{
		{ImageID: 1, Latitude: 37, Longitude: -122},
		{ImageID: 2, Latitude: 37, Longitude: -122},
		{ImageID: 3, Latitude: 37, Longitude: -122},
		{ImageID: 4, Latitude: 37, Longitude: -122},
	}, bitmapidx.FieldConfig{})
	if err != nil {
		t.Fatal(err)
	}
	defer geo.Close()
	key, err := bitmapidx.KeyFromLatLng(37, -122)
	if err != nil {
		t.Fatal(err)
	}
	geoClause, err := WithinCells(geo, bitmapidx.DecodeS2Key(key))
	if err != nil {
		t.Fatal(err)
	}

	plan, err := Compile(
		AllBitmaps(bitmap),
		AllBitmaps(secondBitmap),
		During(timeIndex, 2*quantum, 5*quantum),
		geoClause,
	)
	if err != nil {
		t.Fatal(err)
	}
	stages := plan.Stages()
	if len(stages) != 3 || stages[0].Kind != StageBitmap || stages[0].Estimate != 3 {
		t.Fatalf("stages = %+v", stages)
	}

	workspace := NewWorkspace(8)
	got := plan.Search([]ImageID{99}, workspace)
	if !slices.Equal(got, []ImageID{99, 2, 3}) {
		t.Fatalf("Plan.Search = %v", got)
	}
	workspace.Reset()
	got = plan.Search(got[:1], workspace)
	if !slices.Equal(got, []ImageID{99, 2, 3}) {
		t.Fatalf("reused Plan.Search = %v", got)
	}
	buffer := make([]ImageID, 0, 8)
	if allocations := testing.AllocsPerRun(100, func() {
		buffer = plan.Search(buffer[:0], workspace)
	}); allocations != 0 {
		t.Fatalf("Plan.Search allocations = %v, want 0", allocations)
	}
}

func TestWithinCellsAcceptsCellIDsAndCoarseCells(t *testing.T) {
	geo, err := bitmapidx.NewGeoField(bitmapidx.IndexType(1), []bitmapidx.GeoEntry{
		{ImageID: 1, Latitude: 37.7749, Longitude: -122.4194},
		{ImageID: 2, Latitude: 34.0522, Longitude: -118.2437},
	}, bitmapidx.FieldConfig{})
	if err != nil {
		t.Fatal(err)
	}
	defer geo.Close()

	cell := s2.CellID(bitmapidx.CellIDFromLatLng(37.7749, -122.4194))
	for _, level := range []int{16, 12, 10} {
		clause, err := WithinCells(geo, uint64(cell.Parent(level)))
		if err != nil {
			t.Fatalf("level %d clause: %v", level, err)
		}
		got, err := Search(nil, clause)
		if err != nil {
			t.Fatalf("level %d search: %v", level, err)
		}
		if !slices.Equal(got, []ImageID{1}) {
			t.Fatalf("level %d Search = %v, want [1]", level, got)
		}
	}

	if _, err = WithinCells(geo, 0); err == nil {
		t.Fatal("invalid cell accepted")
	}
	if _, err = WithinCells(geo); !errors.Is(err, ErrInvalidClause) {
		t.Fatalf("empty cells error = %v", err)
	}
}

func TestSearchUsesExactTimestampLookup(t *testing.T) {
	quantum := uint64(1) << timeidx.TimestampShift
	times := map[timeidx.ImageID]uint64{1: quantum + 10, 2: quantum + 20}
	index, err := timeidx.New(timeidx.Spec{Entries: []timeidx.Entry{
		{Timestamp: times[1], ImageID: 1},
		{Timestamp: times[2], ImageID: 2},
	}}, timeidx.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer index.Close()

	got, err := Search(nil, During(
		index,
		quantum+15,
		quantum+25,
		ExactTimestamp(func(id timeidx.ImageID) (uint64, bool) {
			value, ok := times[id]
			return value, ok
		}),
	))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, []ImageID{2}) {
		t.Fatalf("Search = %v", got)
	}
}

func TestSearchRejectsEmptyAndNilIndexQueries(t *testing.T) {
	if _, err := Search(nil); !errors.Is(err, ErrEmptyQuery) {
		t.Fatalf("empty query error = %v", err)
	}
	if _, err := Search(nil, AllBitmaps(nil)); !errors.Is(err, ErrInvalidClause) {
		t.Fatalf("nil index error = %v", err)
	}
	if _, err := WithinCells(nil, uint64(s2.CellIDFromFace(0))); !errors.Is(err, ErrInvalidClause) {
		t.Fatalf("nil geo index error = %v", err)
	}
}

func TestExactTimestampFiltersBitmapCandidates(t *testing.T) {
	quantum := uint64(1) << timeidx.TimestampShift
	times := map[timeidx.ImageID]uint64{1: quantum + 10, 2: quantum + 20}
	index, err := timeidx.New(timeidx.Spec{Entries: []timeidx.Entry{
		{Timestamp: times[1], ImageID: 1},
		{Timestamp: times[2], ImageID: 2},
	}}, timeidx.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer index.Close()
	bitmap, err := bitmapidx.New(bitmapidx.Spec{
		Key: bitmapidx.Key{Type: bitmapidx.IndexTypeTag, ValueID: 4}, IndexID: 4, IDs: []uint64{1, 2},
	}, bitmapidx.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer bitmap.Close()

	got, err := Search(nil,
		AllBitmaps(bitmap),
		During(index, quantum+15, quantum+25, ExactTimestamp(func(id timeidx.ImageID) (uint64, bool) {
			value, ok := times[id]
			return value, ok
		})),
	)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, []ImageID{2}) {
		t.Fatalf("Search = %v", got)
	}
}

func TestBitmapOnlyCountMatchesSearch(t *testing.T) {
	bitmap, err := bitmapidx.New(bitmapidx.Spec{
		Key: bitmapidx.Key{Type: bitmapidx.IndexTypeTag, ValueID: 1}, IndexID: 1, IDs: []uint64{1, 2, 3, 5},
	}, bitmapidx.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer bitmap.Close()
	secondBitmap, err := bitmapidx.New(bitmapidx.Spec{
		Key: bitmapidx.Key{Type: bitmapidx.IndexTypeAlbum, ValueID: 2}, IndexID: 2, IDs: []uint64{2, 3, 4},
	}, bitmapidx.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer secondBitmap.Close()

	plan, err := Compile(AllBitmaps(bitmap), AllBitmaps(secondBitmap))
	if err != nil {
		t.Fatal(err)
	}
	workspace := NewWorkspace(8)
	got := plan.Search(nil, workspace)
	if count := plan.Count(workspace); count != len(got) {
		t.Fatalf("Count = %d, Search len = %d", count, len(got))
	}
	if !slices.Equal(got, []ImageID{2, 3}) {
		t.Fatalf("Search = %v", got)
	}
}

func TestGeoStageFiltersSmallCandidatesAndCollectsLarge(t *testing.T) {
	newBitmap := func(t *testing.T, ids []uint64) *bitmapidx.Index {
		t.Helper()
		index, err := bitmapidx.New(bitmapidx.Spec{
			Key: bitmapidx.Key{Type: bitmapidx.IndexTypeTag, ValueID: 1}, IndexID: 1, IDs: ids,
		}, bitmapidx.Config{})
		if err != nil {
			t.Fatal(err)
		}
		return index
	}
	newGeo := func(t *testing.T, ids []uint64) (*bitmapidx.FieldIndex, bitmapidx.S2Key) {
		t.Helper()
		entries := make([]bitmapidx.GeoEntry, len(ids))
		for i, id := range ids {
			entries[i] = bitmapidx.GeoEntry{ImageID: id, Latitude: 37, Longitude: -122}
		}
		index, err := bitmapidx.NewGeoField(bitmapidx.IndexType(2), entries, bitmapidx.FieldConfig{})
		if err != nil {
			t.Fatal(err)
		}
		key, err := bitmapidx.KeyFromLatLng(37, -122)
		if err != nil {
			t.Fatal(err)
		}
		return index, key
	}

	// Filter path: 3 candidates against a 10-ID cell (1*3 <= 4*10).
	bitmap := newBitmap(t, []uint64{1, 2, 3})
	defer bitmap.Close()
	geo, key := newGeo(t, []uint64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10})
	defer geo.Close()
	geoClause, err := WithinCells(geo, bitmapidx.DecodeS2Key(key))
	if err != nil {
		t.Fatal(err)
	}
	plan, err := Compile(AllBitmaps(bitmap), geoClause)
	if err != nil {
		t.Fatal(err)
	}
	workspace := NewWorkspace(16)
	if got := plan.Search(nil, workspace); !slices.Equal(got, []ImageID{1, 2, 3}) {
		t.Fatalf("filter-path Search = %v", got)
	}

	// Fallback path: 90 candidates across 10 cells of a 100-ID geo
	// population (10*90 > 4*100), so the stage collects and intersects.
	geoIDs := make([]uint64, 0, 100)
	geoEntries := make([]bitmapidx.GeoEntry, 0, 100)
	cellIDs := make([]uint64, 0, 10)
	for cell := range 10 {
		key, err := bitmapidx.KeyFromLatLng(37+float64(cell)/100, -122)
		if err != nil {
			t.Fatal(err)
		}
		if cell == 0 || bitmapidx.DecodeS2Key(key) != cellIDs[len(cellIDs)-1] {
			cellIDs = append(cellIDs, bitmapidx.DecodeS2Key(key))
		}
		for i := range 10 {
			id := uint64(cell*10 + i + 1)
			geoIDs = append(geoIDs, id)
			geoEntries = append(geoEntries, bitmapidx.GeoEntry{ImageID: id, Latitude: 37 + float64(cell)/100, Longitude: -122})
		}
	}
	bigGeo, err := bitmapidx.NewGeoField(bitmapidx.IndexType(4), geoEntries, bitmapidx.FieldConfig{})
	if err != nil {
		t.Fatal(err)
	}
	defer bigGeo.Close()
	bigClause, err := WithinCells(bigGeo, cellIDs...)
	if err != nil {
		t.Fatal(err)
	}
	if len(cellIDs) < 5 {
		t.Fatalf("need distinct cells to force the collect fallback, got %d", len(cellIDs))
	}
	// Bitmap holds 90 of the 100 geo IDs, so it sorts first and leaves 90
	// candidates for the geo stage.
	bitmapIDs := make([]uint64, 0, 90)
	for _, id := range geoIDs {
		if id%10 != 0 {
			bitmapIDs = append(bitmapIDs, id)
		}
	}
	bigBitmap := newBitmap(t, bitmapIDs)
	defer bigBitmap.Close()
	fallback, err := Compile(AllBitmaps(bigBitmap), bigClause)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := fallback.Search(nil, workspace), bitmapIDs; !slices.Equal(got, want) {
		t.Fatalf("fallback-path Search = %v, want %v", got, want)
	}
}
