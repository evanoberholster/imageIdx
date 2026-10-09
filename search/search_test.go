package search

import (
	"context"
	"errors"
	"math"
	"slices"
	"testing"

	common "github.com/evanoberholster/imageIdx/common"
	"github.com/evanoberholster/imageIdx/index/bitmapidx"
	"github.com/evanoberholster/imageIdx/index/geoidx"
	"github.com/evanoberholster/imageIdx/index/timeidx"
	"github.com/golang/geo/s2"
)

func enumNamespaceForSearch(t *testing.T, typ common.Namespace, value string, ids []uint64) *bitmapidx.Index {
	t.Helper()
	index, err := bitmapidx.NewMemory(bitmapidx.NamespaceSpec{
		Namespace: typ, Kind: common.KindBitmapEnum}, bitmapidx.NamespaceConfig{})
	if err != nil {
		t.Fatal(err)
	}
	entries := make([]bitmapidx.Entry, len(ids))
	for n, id := range ids {
		entries[n] = bitmapidx.Entry{ImageID: id, Value: bitmapidx.EnumStringValue(value)}
	}
	batch := index.NewBatch()
	if err := batch.Add(entries...); err != nil {
		_ = index.Close()
		t.Fatal(err)
	}
	if err := batch.Apply(); err != nil {
		_ = index.Close()
		t.Fatal(err)
	}
	batch.Close()
	if err := index.Barrier(); err != nil {
		_ = index.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = index.Close() })
	return index
}

func TestCompileAndSearchCombinedQuery(t *testing.T) {
	quantum := uint64(1) << timeidx.TimestampShift
	tags := enumNamespaceForSearch(t, common.NamespaceTag, "favorite", []uint64{1, 2, 3, 5})
	albums := enumNamespaceForSearch(t, common.NamespaceAlbum, "holiday", []uint64{2, 3, 4})

	timeIndex, err := timeidx.NewMemory(timeidx.Spec{Entries: []timeidx.Entry{
		{Timestamp: quantum, ImageID: 1}, {Timestamp: 2 * quantum, ImageID: 2},
		{Timestamp: 3 * quantum, ImageID: 3}, {Timestamp: 4 * quantum, ImageID: 4},
		{Timestamp: 5 * quantum, ImageID: 5},
	}}, timeidx.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer timeIndex.Close()

	geo := newTestGeoNamespace(t, common.Namespace(20), []geoEntry{
		{id: 1, lat: 37, lng: -122}, {id: 2, lat: 37, lng: -122},
		{id: 3, lat: 37, lng: -122}, {id: 4, lat: 37, lng: -122},
	})
	key, err := geoidx.KeyFromLatLng(37, -122)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := Compile(
		AllValues(tags, bitmapidx.EnumStringValue("favorite")),
		AllValues(albums, bitmapidx.EnumStringValue("holiday")),
		During(timeIndex, 2*quantum, 5*quantum),
		InCells(geo, geoidx.DecodeKey(key)),
	)
	if err != nil {
		t.Fatal(err)
	}
	stages := plan.Stages()
	if len(stages) != 4 || stages[0].Kind != StageNamespace || stages[0].Estimate != 3 {
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
	geo := newTestGeoNamespace(t, common.Namespace(21), []geoEntry{
		{id: 1, lat: 37.7749, lng: -122.4194},
		{id: 2, lat: 34.0522, lng: -118.2437},
	})
	cellID, err := geoidx.CellIDFromLatLng(37.7749, -122.4194)
	if err != nil {
		t.Fatal(err)
	}
	cell := s2.CellID(cellID)
	for _, level := range []int{geoidx.S2Level, 12, 10} {
		got, err := SearchOnce(context.Background(), nil, InCells(geo, uint64(cell.Parent(level))))
		if err != nil || !slices.Equal(got, []ImageID{1}) {
			t.Fatalf("level %d Search = %v, err=%v", level, got, err)
		}
	}
	if _, err := Compile(InCells(geo, 0)); err == nil {
		t.Fatal("invalid cell accepted")
	}
	if _, err := Compile(InCells(geo)); !errors.Is(err, ErrInvalidClause) {
		t.Fatalf("empty cells error = %v", err)
	}
}

func TestSearchUsesExactTimestampLookup(t *testing.T) {
	quantum := uint64(1) << timeidx.TimestampShift
	times := map[common.ValueID]uint64{1: quantum + 10, 2: quantum + 20}
	index, err := timeidx.NewMemory(timeidx.Spec{Entries: []timeidx.Entry{
		{Timestamp: times[1], ImageID: 1}, {Timestamp: times[2], ImageID: 2},
	}}, timeidx.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer index.Close()
	got, err := SearchOnce(context.Background(), nil, During(index, quantum+15, quantum+25, ExactTimestamp(func(id common.ValueID) (uint64, bool) {
		value, ok := times[id]
		return value, ok
	})))
	if err != nil || !slices.Equal(got, []ImageID{2}) {
		t.Fatalf("Search = %v, err=%v", got, err)
	}
}

func TestSearchRejectsEmptyAndNilNamespaceQueries(t *testing.T) {
	if _, err := SearchOnce(context.Background(), nil); !errors.Is(err, ErrEmptyQuery) {
		t.Fatalf("empty query error = %v", err)
	}
	if _, err := SearchOnce(context.Background(), nil, AllValues(nil, bitmapidx.EnumStringValue("x"))); !errors.Is(err, ErrInvalidClause) {
		t.Fatalf("nil namespace error = %v", err)
	}
	if _, err := Compile(InCells(nil, uint64(s2.CellIDFromFace(0)))); !errors.Is(err, ErrInvalidClause) {
		t.Fatalf("nil geo index error = %v", err)
	}
}

func TestNamespaceOnlyCountMatchesSearch(t *testing.T) {
	first := enumNamespaceForSearch(t, common.NamespaceTag, "favorite", []uint64{1, 2, 3, 5})
	second := enumNamespaceForSearch(t, common.NamespaceAlbum, "holiday", []uint64{2, 3, 4})
	plan, err := Compile(
		AllValues(first, bitmapidx.EnumStringValue("favorite")),
		AllValues(second, bitmapidx.EnumStringValue("holiday")),
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

func TestNamespaceTextAndRangeClauses(t *testing.T) {
	text, err := bitmapidx.NewMemory(bitmapidx.NamespaceSpec{
		Namespace: common.Namespace(24), Kind: common.KindBitmapString, Analyzer: bitmapidx.CodecSpec{Code: bitmapidx.AnalyzerLatin, Version: bitmapidx.AnalyzerVersion},
	}, bitmapidx.NamespaceConfig{})
	if err != nil {
		t.Fatal(err)
	}
	defer text.Close()
	commitBitmap(t, text, func(b *bitmapidx.Batch) error {
		return b.ReplaceText(
			bitmapidx.TextReplacement{ImageID: 1, After: []string{"Golden Gate Bridge"}},
			bitmapidx.TextReplacement{ImageID: 2, After: []string{"Golden Gate Park"}},
		)
	})

	continuous, err := bitmapidx.NewMemory(bitmapidx.NamespaceSpec{
		Namespace: common.Namespace(25), Kind: common.KindBitmapContinuous, Quant: bitmapidx.Quantizer{Code: bitmapidx.QuantIdentity, Version: 1, Scale: 1},
	}, bitmapidx.NamespaceConfig{})
	if err != nil {
		t.Fatal(err)
	}
	defer continuous.Close()
	commitBitmap(t, continuous, func(b *bitmapidx.Batch) error {
		return b.Move(
			bitmapidx.Move{ImageID: 1, To: 10},
			bitmapidx.Move{ImageID: 2, To: 20},
		)
	})
	plan, err := Compile(
		NamespaceText(text, "golden bridge"),
		NamespaceRange(continuous, bitmapidx.Between(0, math.MaxInt64)),
	)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := plan.AppendIDs(context.Background(), nil, NewWorkspace(4)); err != nil || !slices.Equal(got, []ImageID{1}) {
		t.Fatalf("namespace text/range search = %v", got)
	}
}

func TestGeoStageCollectsCandidates(t *testing.T) {
	newGeo := func(t *testing.T, typ common.Namespace, ids []uint64) (*geoidx.Index, uint64) {
		t.Helper()
		entries := make([]geoEntry, len(ids))
		for i, id := range ids {
			entries[i] = geoEntry{id: id, lat: 37, lng: -122}
		}
		index := newTestGeoNamespace(t, typ, entries)
		cellID, err := geoidx.CellIDFromLatLng(37, -122)
		if err != nil {
			t.Fatal(err)
		}
		return index, cellID
	}

	geo, cellID := newGeo(t, common.Namespace(22), []uint64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10})
	tags := enumNamespaceForSearch(t, common.NamespaceTag, "favorite", []uint64{1, 2, 3})
	plan, err := Compile(AllValues(tags, bitmapidx.EnumStringValue("favorite")), InCells(geo, cellID))
	if err != nil {
		t.Fatal(err)
	}
	if got, err := plan.AppendIDs(context.Background(), nil, NewWorkspace(16)); err != nil || !slices.Equal(got, []ImageID{1, 2, 3}) {
		t.Fatalf("geo collect Search = %v", got)
	}

	geoIDs := make([]uint64, 0, 100)
	geoEntries := make([]geoEntry, 0, 100)
	cellIDs := make([]uint64, 0, 10)
	for cell := range 10 {
		cellID, err := geoidx.CellIDFromLatLng(37+float64(cell)/100, -122)
		if err != nil {
			t.Fatal(err)
		}
		cellIDs = append(cellIDs, cellID)
		for i := range 10 {
			id := uint64(cell*10 + i + 1)
			geoIDs = append(geoIDs, id)
			geoEntries = append(geoEntries, geoEntry{id: id, lat: 37 + float64(cell)/100, lng: -122})
		}
	}
	bigGeo := newTestGeoNamespace(t, common.Namespace(23), geoEntries)
	if len(cellIDs) < 5 {
		t.Fatalf("need distinct cells, got %d", len(cellIDs))
	}
	bitmapIDs := make([]uint64, 0, 90)
	for _, id := range geoIDs {
		if id%10 != 0 {
			bitmapIDs = append(bitmapIDs, id)
		}
	}
	largeTags := enumNamespaceForSearch(t, common.NamespaceObject, "favorite", bitmapIDs)
	fallback, err := Compile(AllValues(largeTags, bitmapidx.EnumStringValue("favorite")), InCells(bigGeo, cellIDs...))
	if err != nil {
		t.Fatal(err)
	}
	if got, err := fallback.AppendIDs(context.Background(), nil, NewWorkspace(100)); err != nil || !slices.Equal(got, bitmapIDs) {
		t.Fatalf("large geo collect Search = %v, want %v", got, bitmapIDs)
	}
}

func TestAppendPage(t *testing.T) {
	tags := enumNamespaceForSearch(t, common.NamespaceTag, "favorite", []uint64{1, 2, 3})
	plan, err := Compile(AllValues(tags, bitmapidx.EnumStringValue("favorite")))
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
	tags := enumNamespaceForSearch(t, common.NamespaceTag, "favorite", []uint64{1, 2, 3})
	plan, err := Compile(AllValues(tags, bitmapidx.EnumStringValue("favorite")))
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
