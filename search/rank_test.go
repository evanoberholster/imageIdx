package search

import (
	"context"
	"math"
	"slices"
	"testing"

	"github.com/evanoberholster/imageIdx/index/bitmapidx"
	"github.com/evanoberholster/imageIdx/index/timeidx"
)

// TestPlanRankAndFilter covers the ranked and time/geo filter API.
func TestPlanRankAndFilter(t *testing.T) {
	quantum := uint64(1) << timeidx.TimestampShift
	field := benchmarkEnumField(t, bitmapidx.IndexTypeTag, "favorite", []uint64{1, 2, 3, 4})
	plan, err := Compile(AllValues(field, bitmapidx.StringValue("favorite")))
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}

	timestamps := map[ImageID]uint64{1: quantum, 2: 2 * quantum, 3: 3 * quantum, 4: 4 * quantum}
	lookupTime := func(id ImageID) (uint64, bool) {
		ts, ok := timestamps[id]
		return ts, ok
	}
	ws := NewWorkspace(16)

	// RankTime orders by |timestamp-target| with ImageID tie-breaks.
	// Quantum-scale gaps retain their native precision.
	ranked, err := plan.RankTime(context.Background(), nil, ws, lookupTime, 3*quantum, 4)
	if err != nil {
		t.Fatal(err)
	}
	want := []Result{
		{ImageID: 3, Score: 0},
		{ImageID: 2, Score: quantum},
		{ImageID: 4, Score: quantum},
		{ImageID: 1, Score: 2 * quantum},
	}
	if !slices.Equal(ranked, want) {
		t.Fatalf("RankTime = %+v, want %+v", ranked, want)
	}
	if got, err := plan.RankTime(context.Background(), nil, ws, lookupTime, 3*quantum, 2); err != nil || len(got) != 2 || got[0].ImageID != 3 || got[1].ImageID != 2 {
		t.Fatalf("RankTime k=2 = %+v", got)
	}
	if got, err := plan.RankTime(context.Background(), nil, ws, lookupTime, 3*quantum, -1); err != nil || len(got) != 4 {
		t.Fatalf("RankTime k=-1 = %d results, want 4", len(got))
	}
	if got, err := plan.RankTime(context.Background(), nil, ws, lookupTime, 3*quantum, 0); err != nil || len(got) != 0 {
		t.Fatalf("RankTime k=0 = %+v, want unchanged", got)
	}
	if got, err := plan.RankTime(context.Background(), nil, ws, nil, 3*quantum, 4); err == nil || len(got) != 0 {
		t.Fatalf("RankTime nil lookup = %+v, want unchanged", got)
	}

	// FilterTime keeps the half-open range.
	if got := FilterTime(nil, []ImageID{1, 2, 3, 4}, lookupTime, 2*quantum, 4*quantum); !slices.Equal(got, []ImageID{2, 3}) {
		t.Fatalf("FilterTime = %v, want [2 3]", got)
	}
	if got := FilterTime(nil, []ImageID{1, 2}, nil, 0, quantum); len(got) != 0 {
		t.Fatalf("FilterTime nil lookup = %v, want empty", got)
	}

	// Geo ranking and filtering.
	geo, err := bitmapidx.NewGeoField(bitmapidx.IndexTypeLocation, []bitmapidx.GeoEntry{
		{ImageID: 1, Latitude: 37.7749, Longitude: -122.4194},
		{ImageID: 2, Latitude: 37.8044, Longitude: -122.2712},
		{ImageID: 3, Latitude: 34.0522, Longitude: -118.2437},
		{ImageID: 4, Latitude: 40.7128, Longitude: -74.0060},
	}, bitmapidx.FieldConfig{})
	if err != nil {
		t.Fatalf("NewGeoField: %v", err)
	}
	t.Cleanup(func() { _ = geo.Close() })
	coordinates := map[ImageID][2]float64{
		1: {37.7749, -122.4194},
		2: {37.8044, -122.2712},
		3: {34.0522, -118.2437},
		4: {40.7128, -74.0060},
	}
	lookupGeo := func(id ImageID) (float64, float64, bool) {
		c, ok := coordinates[id]
		if !ok {
			return 0, 0, false
		}
		return c[0], c[1], true
	}
	geoRanked, err := plan.RankGeo(context.Background(), nil, ws, 37.7749, -122.4194, lookupGeo, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(geoRanked) != 2 || geoRanked[0].ImageID != 1 || geoRanked[1].ImageID != 2 {
		t.Fatalf("RankGeo = %+v, want ids [1 2]", geoRanked)
	}
	if geoRanked[0].Score != 0 || geoRanked[1].Score <= 0 {
		t.Fatalf("RankGeo scores = %+v", geoRanked)
	}
	if got, err := plan.RankGeo(context.Background(), nil, ws, 37.7749, -122.4194, lookupGeo, 0); err != nil || len(got) != 0 {
		t.Fatalf("RankGeo k=0 = %+v, want unchanged", got)
	}

	cell := bitmapidx.CellIDFromLatLng(37.7749, -122.4194)
	filtered, err := FilterGeo(nil, []ImageID{1, 2, 3, 4}, geo, cell)
	if err != nil {
		t.Fatalf("FilterGeo: %v", err)
	}
	if !slices.Equal(filtered, []ImageID{1}) {
		t.Fatalf("FilterGeo = %v, want [1]", filtered)
	}
	if _, err := FilterGeo(nil, []ImageID{1}, field, cell); err == nil {
		t.Fatal("FilterGeo on non-geo field succeeded")
	}
	if got, err := FilterGeo(nil, []ImageID{1}, geo); err != nil || len(got) != 0 {
		t.Fatalf("FilterGeo empty cells = %v, %v", got, err)
	}
}

// fakeANN mirrors vectoridx.Result: an internal node id plus a
// smaller-is-better distance score.
type fakeANN struct {
	node uint32
	dist uint32
}

// TestFilterRankedANNResults checks that a scored ANN list maps to scored
// search results in source order, dropping entries outside the plan.
func TestFilterRankedANNResults(t *testing.T) {
	field := benchmarkEnumField(t, bitmapidx.IndexTypeTag, "favorite", []uint64{1, 2, 3, 4})
	plan, err := Compile(AllValues(field, bitmapidx.StringValue("favorite")))
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	nodeToImage := map[uint32]ImageID{10: 4, 11: 9, 12: 2} // 9 is outside the plan
	ann := []fakeANN{{node: 10, dist: 50}, {node: 11, dist: 30}, {node: 12, dist: 10}}

	got, err := plan.FilterRanked(context.Background(), nil, NewWorkspace(16), ann,
		func(r fakeANN) (ImageID, bool) { image, ok := nodeToImage[r.node]; return image, ok },
		func(r fakeANN) uint64 { return uint64(r.dist) })
	if err != nil {
		t.Fatal(err)
	}
	want := []Result{{ImageID: 4, Score: 50}, {ImageID: 2, Score: 10}}
	if !slices.Equal(got, want) {
		t.Fatalf("FilterRanked = %+v, want %+v", got, want)
	}
	if got, err := plan.FilterRanked[fakeANN](context.Background(), nil, nil, nil, nil, nil); err != nil || len(got) != 0 {
		t.Fatalf("FilterRanked empty source = %+v", got)
	}
}

func TestScorePrecision(t *testing.T) {
	field := benchmarkEnumField(t, bitmapidx.IndexTypeTag, "favorite", []uint64{1, 2, 3})
	plan, err := Compile(AllValues(field, bitmapidx.StringValue("favorite")))
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	// Gaps above the old uint16 boundary remain distinguishable.
	timestamps := map[ImageID]uint64{1: 1000, 2: 1100, 3: 1000 + math.MaxUint16 + 5}
	lookup := func(id ImageID) (uint64, bool) {
		ts, ok := timestamps[id]
		return ts, ok
	}
	got, err := plan.RankTime(context.Background(), nil, NewWorkspace(16), lookup, 1000, -1)
	if err != nil {
		t.Fatal(err)
	}
	want := []Result{
		{ImageID: 1, Score: 0},
		{ImageID: 2, Score: 100},
		{ImageID: 3, Score: math.MaxUint16 + 5},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("saturation = %+v, want %+v", got, want)
	}
	exact := map[ImageID]uint64{1: 1000, 2: 1000 + math.MaxUint16}
	lookupExact := func(id ImageID) (uint64, bool) {
		ts, ok := exact[id]
		return ts, ok
	}
	got, err = plan.RankTime(context.Background(), nil, NewWorkspace(16), lookupExact, 1000, -1)
	if err != nil {
		t.Fatal(err)
	}
	want = []Result{
		{ImageID: 1, Score: 0},
		{ImageID: 2, Score: math.MaxUint16},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("boundary = %+v, want %+v", got, want)
	}
	// Invalid coordinates are skipped.
	nan := map[ImageID][2]float64{1: {37.7749, -122.4194}, 2: {math.NaN(), 0}}
	lookupNaN := func(id ImageID) (float64, float64, bool) {
		c, ok := nan[id]
		return c[0], c[1], ok
	}
	geoGot, err := plan.RankGeo(context.Background(), nil, NewWorkspace(16), 37.7749, -122.4194, lookupNaN, -1)
	if err != nil {
		t.Fatal(err)
	}
	if len(geoGot) != 1 || geoGot[0].ImageID != 1 || geoGot[0].Score != 0 {
		t.Fatalf("NaN geo = %+v, want [{1 0}]", geoGot)
	}
}

func TestRankingCancellation(t *testing.T) {
	quantum := uint64(1) << timeidx.TimestampShift
	field := benchmarkEnumField(t, bitmapidx.IndexTypeTag, "favorite", []uint64{1, 2, 3, 4})
	plan, err := Compile(AllValues(field, bitmapidx.StringValue("favorite")))
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	timestamps := map[ImageID]uint64{1: quantum, 2: 2 * quantum, 3: 3 * quantum, 4: 4 * quantum}
	lookupTime := func(id ImageID) (uint64, bool) {
		ts, ok := timestamps[id]
		return ts, ok
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := plan.RankTime(cancelled, nil, NewWorkspace(16), lookupTime, 3*quantum, 4); err == nil {
		t.Fatal("cancelled RankTime succeeded")
	}
	if _, err := plan.RankGeo(cancelled, nil, NewWorkspace(16), 37.7749, -122.4194, func(ImageID) (float64, float64, bool) {
		return 0, 0, false
	}, 4); err == nil {
		t.Fatal("cancelled RankGeo succeeded")
	}
	nodeToImage := map[uint32]ImageID{10: 4, 12: 2}
	ann := []fakeANN{{node: 10, dist: 50}, {node: 12, dist: 10}}
	if _, err := plan.FilterRanked(cancelled, nil, NewWorkspace(16), ann,
		func(r fakeANN) (ImageID, bool) { image, ok := nodeToImage[r.node]; return image, ok },
		func(r fakeANN) uint64 { return uint64(r.dist) }); err == nil {
		t.Fatal("cancelled FilterRanked succeeded")
	}
	// Uncancelled equivalents still agree with the Background forms.
	if got, err := plan.RankTime(context.Background(), nil, NewWorkspace(16), lookupTime, 3*quantum, 4); err != nil || len(got) != 4 {
		t.Fatalf("RankTime = %+v, %v; want 4 results, nil", got, err)
	}
}
