package search

import (
	"errors"
	"slices"
	"testing"

	"github.com/evanoberholster/imageIdx/bitmapidx"
	"github.com/evanoberholster/imageIdx/geoindex"
	"github.com/evanoberholster/imageIdx/timestamp"
	"github.com/golang/geo/s2"
)

func TestCompileAndSearchCombinedQuery(t *testing.T) {
	quantum := uint64(1) << timestamp.TimestampShift
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

	timeIndex, err := timestamp.New(timestamp.Spec{IndexID: 3, Entries: []timestamp.Entry{
		{Timestamp: quantum, ImageID: 1},
		{Timestamp: 2 * quantum, ImageID: 2},
		{Timestamp: 3 * quantum, ImageID: 3},
		{Timestamp: 4 * quantum, ImageID: 4},
		{Timestamp: 5 * quantum, ImageID: 5},
	}}, timestamp.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer timeIndex.Close()

	geo, err := geoindex.New(geoindex.Spec{IndexID: 2, Entries: []geoindex.Entry{
		{ImageID: 1, Latitude: 37, Longitude: -122},
		{ImageID: 2, Latitude: 37, Longitude: -122},
		{ImageID: 3, Latitude: 37, Longitude: -122},
		{ImageID: 4, Latitude: 37, Longitude: -122},
	}}, geoindex.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer geo.Close()
	key, err := geoindex.KeyFromLatLng(37, -122)
	if err != nil {
		t.Fatal(err)
	}
	geoClause, err := WithinCells(geo, geoindex.DecodeS2Key(key))
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
	geo, err := geoindex.New(geoindex.Spec{IndexID: 1, Entries: []geoindex.Entry{
		{ImageID: 1, Latitude: 37.7749, Longitude: -122.4194},
		{ImageID: 2, Latitude: 34.0522, Longitude: -118.2437},
	}}, geoindex.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer geo.Close()

	cell := s2.CellIDFromLatLng(s2.LatLngFromDegrees(37.7749, -122.4194)).Parent(geoindex.GeoS2Level)
	clause, err := WithinCells(geo, uint64(cell.Parent(12)))
	if err != nil {
		t.Fatal(err)
	}
	got, err := Search(nil, clause)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, []ImageID{1}) {
		t.Fatalf("Search = %v", got)
	}

	if _, err = WithinCells(geo, 0); err == nil {
		t.Fatal("invalid cell accepted")
	}
	if _, err = WithinCells(geo); !errors.Is(err, ErrInvalidClause) {
		t.Fatalf("empty cells error = %v", err)
	}
}

func TestSearchUsesExactTimestampLookup(t *testing.T) {
	quantum := uint64(1) << timestamp.TimestampShift
	times := map[timestamp.ImageID]uint64{1: quantum + 10, 2: quantum + 20}
	index, err := timestamp.New(timestamp.Spec{Entries: []timestamp.Entry{
		{Timestamp: times[1], ImageID: 1},
		{Timestamp: times[2], ImageID: 2},
	}}, timestamp.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer index.Close()

	got, err := Search(nil, During(
		index,
		quantum+15,
		quantum+25,
		ExactTimestamp(func(id timestamp.ImageID) (uint64, bool) {
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
	quantum := uint64(1) << timestamp.TimestampShift
	times := map[timestamp.ImageID]uint64{1: quantum + 10, 2: quantum + 20}
	index, err := timestamp.New(timestamp.Spec{Entries: []timestamp.Entry{
		{Timestamp: times[1], ImageID: 1},
		{Timestamp: times[2], ImageID: 2},
	}}, timestamp.Config{})
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
		During(index, quantum+15, quantum+25, ExactTimestamp(func(id timestamp.ImageID) (uint64, bool) {
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
