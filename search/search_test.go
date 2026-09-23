package search

import (
	"context"
	"errors"
	"math"
	"slices"
	"testing"

	"github.com/evanoberholster/imageIdx/index/bitmapidx"
	"github.com/evanoberholster/imageIdx/index/timeidx"
	"github.com/golang/geo/s2"
)

func enumFieldForSearch(t *testing.T, typ bitmapidx.IndexType, value string, ids []uint64) *bitmapidx.FieldIndex {
	t.Helper()
	index, err := bitmapidx.NewFieldIndex(bitmapidx.Field{
		Type: typ, Kind: bitmapidx.KindEnum, Storage: bitmapidx.StoragePerType,
	}, bitmapidx.FieldConfig{})
	if err != nil {
		t.Fatal(err)
	}
	entries := make([]bitmapidx.Entry, len(ids))
	for n, id := range ids {
		entries[n] = bitmapidx.Entry{ImageID: id, Value: bitmapidx.StringValue(value)}
	}
	if err := index.Add(entries...); err != nil {
		_ = index.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = index.Close() })
	return index
}

func TestCompileAndSearchCombinedQuery(t *testing.T) {
	quantum := uint64(1) << timeidx.TimestampShift
	tags := enumFieldForSearch(t, bitmapidx.IndexTypeTag, "favorite", []uint64{1, 2, 3, 5})
	albums := enumFieldForSearch(t, bitmapidx.IndexTypeAlbum, "holiday", []uint64{2, 3, 4})

	timeIndex, err := timeidx.New(timeidx.Spec{Entries: []timeidx.Entry{
		{Timestamp: quantum, ImageID: 1}, {Timestamp: 2 * quantum, ImageID: 2},
		{Timestamp: 3 * quantum, ImageID: 3}, {Timestamp: 4 * quantum, ImageID: 4},
		{Timestamp: 5 * quantum, ImageID: 5},
	}}, timeidx.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer timeIndex.Close()

	geo, err := bitmapidx.NewGeoField(bitmapidx.IndexType(20), []bitmapidx.GeoEntry{
		{ImageID: 1, Latitude: 37, Longitude: -122}, {ImageID: 2, Latitude: 37, Longitude: -122},
		{ImageID: 3, Latitude: 37, Longitude: -122}, {ImageID: 4, Latitude: 37, Longitude: -122},
	}, bitmapidx.FieldConfig{})
	if err != nil {
		t.Fatal(err)
	}
	defer geo.Close()
	key, err := bitmapidx.KeyFromLatLng(37, -122)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := Compile(
		AllValues(tags, bitmapidx.StringValue("favorite")),
		AllValues(albums, bitmapidx.StringValue("holiday")),
		During(timeIndex, 2*quantum, 5*quantum),
		InCells(geo, bitmapidx.DecodeS2Key(key)),
	)
	if err != nil {
		t.Fatal(err)
	}
	stages := plan.Stages()
	if len(stages) != 4 || stages[0].Kind != StageField || stages[0].Estimate != 3 {
		t.Fatalf("stages = %+v", stages)
	}
	workspace := NewWorkspace(8)
	if got, err := plan.AppendIDs(context.Background(), []ImageID{99}, workspace); err != nil || !slices.Equal(got, []ImageID{99, 2, 3}) {
		t.Fatalf("Plan.AppendIDs = %v, %v", got, err)
	}
	workspace.Reset()
	if got, err := plan.AppendIDs(context.Background(), nil, workspace); err != nil || !slices.Equal(got, []ImageID{2, 3}) {
		t.Fatalf("reused Plan.AppendIDs = %v, %v", got, err)
	}
}

func TestInCellsAcceptsCellIDsAndCoarseCells(t *testing.T) {
	geo, err := bitmapidx.NewGeoField(bitmapidx.IndexType(21), []bitmapidx.GeoEntry{
		{ImageID: 1, Latitude: 37.7749, Longitude: -122.4194},
		{ImageID: 2, Latitude: 34.0522, Longitude: -118.2437},
	}, bitmapidx.FieldConfig{})
	if err != nil {
		t.Fatal(err)
	}
	defer geo.Close()
	cell := s2.CellID(bitmapidx.CellIDFromLatLng(37.7749, -122.4194))
	for _, level := range []int{16, 12, 10} {
		got, err := SearchOnce(context.Background(), nil, InCells(geo, uint64(cell.Parent(level))))
		if err != nil || !slices.Equal(got, []ImageID{1}) {
			t.Fatalf("level %d Search = %v, err=%v", level, got, err)
		}
	}
	if _, err = Compile(InCells(geo, 0)); err == nil {
		t.Fatal("invalid cell accepted")
	}
	if _, err = Compile(InCells(geo)); !errors.Is(err, ErrInvalidClause) {
		t.Fatalf("empty cells error = %v", err)
	}
}

func TestSearchUsesExactTimestampLookup(t *testing.T) {
	quantum := uint64(1) << timeidx.TimestampShift
	times := map[timeidx.ImageID]uint64{1: quantum + 10, 2: quantum + 20}
	index, err := timeidx.New(timeidx.Spec{Entries: []timeidx.Entry{
		{Timestamp: times[1], ImageID: 1}, {Timestamp: times[2], ImageID: 2},
	}}, timeidx.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer index.Close()
	got, err := SearchOnce(context.Background(), nil, During(index, quantum+15, quantum+25, ExactTimestamp(func(id timeidx.ImageID) (uint64, bool) {
		value, ok := times[id]
		return value, ok
	})))
	if err != nil || !slices.Equal(got, []ImageID{2}) {
		t.Fatalf("Search = %v, err=%v", got, err)
	}
}

func TestSearchRejectsEmptyAndNilFieldQueries(t *testing.T) {
	if _, err := SearchOnce(context.Background(), nil); !errors.Is(err, ErrEmptyQuery) {
		t.Fatalf("empty query error = %v", err)
	}
	if _, err := SearchOnce(context.Background(), nil, AllValues(nil, bitmapidx.StringValue("x"))); !errors.Is(err, ErrInvalidClause) {
		t.Fatalf("nil field error = %v", err)
	}
	if _, err := Compile(InCells(nil, uint64(s2.CellIDFromFace(0)))); !errors.Is(err, ErrInvalidClause) {
		t.Fatalf("nil geo index error = %v", err)
	}
}

func TestFieldOnlyCountMatchesSearch(t *testing.T) {
	first := enumFieldForSearch(t, bitmapidx.IndexTypeTag, "favorite", []uint64{1, 2, 3, 5})
	second := enumFieldForSearch(t, bitmapidx.IndexTypeAlbum, "holiday", []uint64{2, 3, 4})
	plan, err := Compile(
		AllValues(first, bitmapidx.StringValue("favorite")),
		AllValues(second, bitmapidx.StringValue("holiday")),
	)
	if err != nil {
		t.Fatal(err)
	}
	workspace := NewWorkspace(8)
	got, err := plan.AppendIDs(context.Background(), nil, workspace)
	if err != nil {
		t.Fatal(err)
	}
	if count, err := plan.Count(context.Background(), workspace); err != nil || count != uint64(len(got)) {
		t.Fatalf("Count = %d, Search len = %d", count, len(got))
	}
	if !slices.Equal(got, []ImageID{2, 3}) {
		t.Fatalf("Search = %v", got)
	}
}

func TestFieldTextAndRangeClauses(t *testing.T) {
	text, err := bitmapidx.NewFieldIndex(bitmapidx.Field{
		Type: bitmapidx.IndexType(24), Kind: bitmapidx.KindText, Storage: bitmapidx.StoragePerType,
		Analyzer: bitmapidx.CodecSpec{Code: bitmapidx.AnalyzerLatin, Version: bitmapidx.AnalyzerVersion},
	}, bitmapidx.FieldConfig{})
	if err != nil {
		t.Fatal(err)
	}
	defer text.Close()
	if err := text.ReplaceText(
		bitmapidx.TextReplacement{ImageID: 1, After: []string{"Golden Gate Bridge"}},
		bitmapidx.TextReplacement{ImageID: 2, After: []string{"Golden Gate Park"}},
	); err != nil {
		t.Fatal(err)
	}

	continuous, err := bitmapidx.NewFieldIndex(bitmapidx.Field{
		Type: bitmapidx.IndexType(25), Kind: bitmapidx.KindContinuous, Storage: bitmapidx.StoragePerType,
		Quant: bitmapidx.Quantizer{Code: bitmapidx.QuantIdentity, Version: 1, Scale: 1},
	}, bitmapidx.FieldConfig{})
	if err != nil {
		t.Fatal(err)
	}
	defer continuous.Close()
	if err := continuous.Move(
		bitmapidx.Move{ImageID: 1, To: 10, HasTo: true},
		bitmapidx.Move{ImageID: 2, To: 20, HasTo: true},
	); err != nil {
		t.Fatal(err)
	}
	plan, err := Compile(
		FieldText(text, "golden bridge"),
		FieldRange(continuous, bitmapidx.Between(0, math.MaxInt64)),
	)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := plan.AppendIDs(context.Background(), nil, NewWorkspace(4)); err != nil || !slices.Equal(got, []ImageID{1}) {
		t.Fatalf("field text/range search = %v", got)
	}
}

func TestGeoStageFiltersSmallCandidatesAndCollectsLarge(t *testing.T) {
	newGeo := func(t *testing.T, typ bitmapidx.IndexType, ids []uint64) (*bitmapidx.FieldIndex, bitmapidx.S2Key) {
		t.Helper()
		entries := make([]bitmapidx.GeoEntry, len(ids))
		for i, id := range ids {
			entries[i] = bitmapidx.GeoEntry{ImageID: id, Latitude: 37, Longitude: -122}
		}
		index, err := bitmapidx.NewGeoField(typ, entries, bitmapidx.FieldConfig{})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = index.Close() })
		key, err := bitmapidx.KeyFromLatLng(37, -122)
		if err != nil {
			t.Fatal(err)
		}
		return index, key
	}

	geo, key := newGeo(t, bitmapidx.IndexType(22), []uint64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10})
	tags := enumFieldForSearch(t, bitmapidx.IndexTypeTag, "favorite", []uint64{1, 2, 3})
	plan, err := Compile(AllValues(tags, bitmapidx.StringValue("favorite")), InCells(geo, bitmapidx.DecodeS2Key(key)))
	if err != nil {
		t.Fatal(err)
	}
	if got, err := plan.AppendIDs(context.Background(), nil, NewWorkspace(16)); err != nil || !slices.Equal(got, []ImageID{1, 2, 3}) {
		t.Fatalf("filter-path Search = %v", got)
	}

	geoIDs := make([]uint64, 0, 100)
	geoEntries := make([]bitmapidx.GeoEntry, 0, 100)
	cellIDs := make([]uint64, 0, 10)
	for cell := range 10 {
		key, err := bitmapidx.KeyFromLatLng(37+float64(cell)/100, -122)
		if err != nil {
			t.Fatal(err)
		}
		decoded := bitmapidx.DecodeS2Key(key)
		if cell == 0 || decoded != cellIDs[len(cellIDs)-1] {
			cellIDs = append(cellIDs, decoded)
		}
		for i := range 10 {
			id := uint64(cell*10 + i + 1)
			geoIDs = append(geoIDs, id)
			geoEntries = append(geoEntries, bitmapidx.GeoEntry{ImageID: id, Latitude: 37 + float64(cell)/100, Longitude: -122})
		}
	}
	bigGeo, err := bitmapidx.NewGeoField(bitmapidx.IndexType(23), geoEntries, bitmapidx.FieldConfig{})
	if err != nil {
		t.Fatal(err)
	}
	defer bigGeo.Close()
	if len(cellIDs) < 5 {
		t.Fatalf("need distinct cells, got %d", len(cellIDs))
	}
	bitmapIDs := make([]uint64, 0, 90)
	for _, id := range geoIDs {
		if id%10 != 0 {
			bitmapIDs = append(bitmapIDs, id)
		}
	}
	largeTags := enumFieldForSearch(t, bitmapidx.IndexTypeObject, "favorite", bitmapIDs)
	fallback, err := Compile(AllValues(largeTags, bitmapidx.StringValue("favorite")), InCells(bigGeo, cellIDs...))
	if err != nil {
		t.Fatal(err)
	}
	if got, err := fallback.AppendIDs(context.Background(), nil, NewWorkspace(100)); err != nil || !slices.Equal(got, bitmapIDs) {
		t.Fatalf("fallback-path Search = %v, want %v", got, bitmapIDs)
	}
}

func TestAppendPage(t *testing.T) {
	tags := enumFieldForSearch(t, bitmapidx.IndexTypeTag, "favorite", []uint64{1, 2, 3})
	plan, err := Compile(AllValues(tags, bitmapidx.StringValue("favorite")))
	if err != nil {
		t.Fatal(err)
	}
	workspace := NewWorkspace(16)
	if got, err := plan.AppendPage(context.Background(), nil, workspace, Page{Limit: 2}); err != nil || !slices.Equal(got, []ImageID{1, 2}) {
		t.Fatalf("AppendPage = %v, %v; want [1 2], nil", got, err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := plan.AppendPage(cancelled, nil, workspace, Page{Limit: 2}); err == nil {
		t.Fatal("cancelled AppendPage succeeded")
	}
}

func TestVisit(t *testing.T) {
	tags := enumFieldForSearch(t, bitmapidx.IndexTypeTag, "favorite", []uint64{1, 2, 3})
	plan, err := Compile(AllValues(tags, bitmapidx.StringValue("favorite")))
	if err != nil {
		t.Fatal(err)
	}
	var got []ImageID
	if err := plan.Visit(context.Background(), NewWorkspace(16), func(id ImageID) bool { got = append(got, id); return true }); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, []ImageID{1, 2, 3}) {
		t.Fatalf("Visit = %v, want [1 2 3]", got)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := plan.Visit(cancelled, NewWorkspace(16), func(ImageID) bool { t.Fatal("cancelled Visit yielded"); return false }); err != context.Canceled {
		t.Fatal(err)
	}
}
