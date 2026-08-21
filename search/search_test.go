package search

import (
	"errors"
	"slices"
	"testing"

	"github.com/evanoberholster/imageIdx/bitmapidx"
	"github.com/evanoberholster/imageIdx/geoindex"
	"github.com/evanoberholster/imageIdx/timestamp"
)

func TestSchedulerPlansAndExecutesCombinedQuery(t *testing.T) {
	quantum := uint64(1) << timestamp.TimestampShift
	epoch := bitmapidx.NewEpoch()
	bitmap, err := bitmapidx.New(
		bitmapidx.BitmapKey{Type: bitmapidx.IndexTypeTag, ValueID: 7},
		1,
		[]uint64{1, 2, 3, 5},
		bitmapidx.Config{Epoch: epoch},
	)
	if err != nil {
		t.Fatal(err)
	}
	defer bitmap.Close()

	timeIndex, err := timestamp.New([]timestamp.Record{
		{Timestamp: quantum, ImageID: 1},
		{Timestamp: 2 * quantum, ImageID: 2},
		{Timestamp: 3 * quantum, ImageID: 3},
		{Timestamp: 4 * quantum, ImageID: 4},
		{Timestamp: 5 * quantum, ImageID: 5},
	}, 0, timestamp.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer timeIndex.Close()

	key, err := geoindex.KeyFromLatLng(37, -122)
	if err != nil {
		t.Fatal(err)
	}
	geo, err := geoindex.NewFromCells(2, []geoindex.GeoCellEntry{
		{Key: key, ImageID: 1},
		{Key: key, ImageID: 2},
		{Key: key, ImageID: 3},
		{Key: key, ImageID: 4},
	}, geoindex.Config{Epoch: epoch})
	if err != nil {
		t.Fatal(err)
	}
	defer geo.Close()

	query := Query{
		Bitmaps: []*bitmapidx.BitmapIndex{bitmap},
		TimestampRanges: []TimestampRange{{
			Index: timeIndex,
			Start: 2 * quantum,
			End:   5 * quantum,
		}},
		GeoRanges: []GeoRange{{
			Index:  geo,
			Ranges: []geoindex.S2KeyRange{{Start: key, End: key}},
		}},
	}
	plan, err := (Scheduler{}).Plan(query)
	if err != nil {
		t.Fatal(err)
	}
	stages := plan.Stages()
	if len(stages) != 3 || stages[0].Kind != StageTimestamp {
		t.Fatalf("stages = %+v", stages)
	}

	workspace := NewWorkspace(8)
	got := plan.Execute([]ImageID{99}, workspace)
	if !slices.Equal(got, []ImageID{99, 2, 3}) {
		t.Fatalf("Execute = %v", got)
	}
	workspace.Reset()
	got = plan.Execute(got[:1], workspace)
	if !slices.Equal(got, []ImageID{99, 2, 3}) {
		t.Fatalf("reused Execute = %v", got)
	}
}

func TestSearchUsesExactTimestampLookup(t *testing.T) {
	quantum := uint64(1) << timestamp.TimestampShift
	times := map[timestamp.ImageID]uint64{1: quantum + 10, 2: quantum + 20}
	index, err := timestamp.New([]timestamp.Record{
		{Timestamp: times[1], ImageID: 1},
		{Timestamp: times[2], ImageID: 2},
	}, 0, timestamp.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer index.Close()

	got, err := Search(nil, Query{TimestampRanges: []TimestampRange{{
		Index: index,
		Start: quantum + 15,
		End:   quantum + 25,
		Exact: func(id timestamp.ImageID) (uint64, bool) {
			value, ok := times[id]
			return value, ok
		},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, []ImageID{2}) {
		t.Fatalf("Search = %v", got)
	}
}

func TestSearchRejectsEmptyAndNilIndexQueries(t *testing.T) {
	if _, err := Search(nil, Query{}); !errors.Is(err, ErrEmptyQuery) {
		t.Fatalf("empty query error = %v", err)
	}
	if _, err := Search(nil, Query{Bitmaps: []*bitmapidx.BitmapIndex{nil}}); !errors.Is(err, ErrInvalidQuery) {
		t.Fatalf("nil index error = %v", err)
	}
}
