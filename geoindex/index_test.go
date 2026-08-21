package geoindex

import (
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/golang/geo/s2"
)

func testConfig() Config {
	return Config{PublishThreshold: 1_000_000, PublishInterval: time.Hour, MaxCoverCells: 32}
}

func keyAt(t testing.TB, lat, lng float64) S2Key {
	t.Helper()
	key, err := KeyFromLatLng(lat, lng)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func TestInvalidImageAndCoordinate(t *testing.T) {
	_, err := New(1, []GeoEntry{{ImageID: 0, Latitude: 1, Longitude: 1}}, testConfig())
	if !errors.Is(err, ErrInvalidImageID) {
		t.Fatalf("error = %v", err)
	}
	_, err = New(1, []GeoEntry{{ImageID: 1, Latitude: 91, Longitude: 1}}, testConfig())
	if !errors.Is(err, ErrInvalidCoordinate) {
		t.Fatalf("error = %v", err)
	}
}

func TestSparseCellsOneThroughFour(t *testing.T) {
	base := s2.CellID(DecodeS2Key(keyAt(t, 0, 0)))
	entries := make([]GeoCellEntry, 0, 10)
	want := make(map[S2Key][]ImageID)
	var id uint64 = 1
	for count := 1; count <= 4; count++ {
		key := EncodeS2Key(uint64(base.Advance(int64(count * 10))))
		for range count {
			entries = append(entries, GeoCellEntry{Key: key, ImageID: id})
			want[key] = append(want[key], ImageID(id))
			id++
		}
	}
	index, err := NewFromCells(9, entries, testConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer index.Close()
	if index.LoadView().Sparse.CellCount() != 4 || index.LoadView().Bitmap.CellCount() != 0 {
		t.Fatalf("wrong partitions: %#v", index.LoadView().Header())
	}
	for key, expected := range want {
		if got := index.SearchCell(nil, DecodeS2Key(key)); !slices.Equal(got, expected) {
			t.Fatalf("key %v: got %v want %v", key, got, expected)
		}
	}
}

func TestPromotionDemotionAndMove(t *testing.T) {
	keyA := keyAt(t, 37.7749, -122.4194)
	keyB := keyAt(t, 34.0522, -118.2437)
	entries := []GeoCellEntry{{keyA, 1}, {keyA, 2}, {keyA, 3}, {keyA, 4}}
	index, err := NewFromCells(1, entries, testConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer index.Close()
	if err = index.AddCellBatch([]GeoCellEntry{{Key: keyA, ImageID: 5}}); err != nil {
		t.Fatal(err)
	}
	if err = index.Flush(); err != nil {
		t.Fatal(err)
	}
	if index.LoadView().Sparse.CellCount() != 0 || index.LoadView().Bitmap.CellCount() != 1 {
		t.Fatalf("promotion failed: %#v", index.LoadView().Header())
	}
	if err = index.RemoveBatch([]uint64{5}); err != nil {
		t.Fatal(err)
	}
	if err = index.Flush(); err != nil {
		t.Fatal(err)
	}
	if index.LoadView().Sparse.CellCount() != 1 || index.LoadView().Bitmap.CellCount() != 0 {
		t.Fatalf("demotion failed: %#v", index.LoadView().Header())
	}
	if err = index.UpdateBatch([]GeoUpdate{{ImageID: 1, Latitude: 34.0522, Longitude: -118.2437}}); err != nil {
		t.Fatal(err)
	}
	if err = index.Flush(); err != nil {
		t.Fatal(err)
	}
	if got := index.SearchCell(nil, DecodeS2Key(keyA)); !slices.Equal(got, []ImageID{2, 3, 4}) {
		t.Fatalf("old cell = %v", got)
	}
	if got := index.SearchCell(nil, DecodeS2Key(keyB)); !slices.Equal(got, []ImageID{1}) {
		t.Fatalf("new cell = %v", got)
	}
}

func TestSearchRangesBoundingBoxPolygonAndExactFilter(t *testing.T) {
	coordinates := map[ImageID][2]float64{
		1: {37.7749, -122.4194},
		2: {37.7790, -122.4140},
		3: {34.0522, -118.2437},
	}
	entries := make([]GeoEntry, 0, len(coordinates))
	for id, coordinate := range coordinates {
		entries = append(entries, GeoEntry{ImageID: uint64(id), Latitude: coordinate[0], Longitude: coordinate[1]})
	}
	index, err := New(1, entries, testConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer index.Close()
	lookup := func(id ImageID) (float64, float64, bool) {
		coordinate, ok := coordinates[id]
		return coordinate[0], coordinate[1], ok
	}
	got := index.SearchBounds(nil, 37.70, -122.52, 37.82, -122.35, lookup)
	slices.Sort(got)
	if !slices.Equal(got, []ImageID{1, 2}) {
		t.Fatalf("bounds = %v", got)
	}

	points := []s2.Point{
		s2.PointFromLatLng(s2.LatLngFromDegrees(37.70, -122.52)),
		s2.PointFromLatLng(s2.LatLngFromDegrees(37.85, -122.52)),
		s2.PointFromLatLng(s2.LatLngFromDegrees(37.85, -122.35)),
		s2.PointFromLatLng(s2.LatLngFromDegrees(37.70, -122.35)),
	}
	loop := s2.LoopFromPoints(points)
	loop.Normalize()
	polygon := s2.PolygonFromLoops([]*s2.Loop{loop})
	got = index.SearchRegion(nil, polygon, lookup)
	slices.Sort(got)
	if !slices.Equal(got, []ImageID{1, 2}) {
		t.Fatalf("polygon = %v", got)
	}
}

func TestAtomicPromotionWithReaders(t *testing.T) {
	key := keyAt(t, 51.5074, -0.1278)
	index, err := NewFromCells(1, []GeoCellEntry{{key, 1}, {key, 2}, {key, 3}, {key, 4}}, testConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer index.Close()
	stop := make(chan struct{})
	errorsCh := make(chan int, 1)
	var readers sync.WaitGroup
	for range 8 {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				count := len(index.SearchCell(nil, DecodeS2Key(key)))
				if count != 4 && count != 5 {
					select {
					case errorsCh <- count:
					default:
					}
					return
				}
			}
		}()
	}
	_ = index.AddCellBatch([]GeoCellEntry{{key, 5}})
	_ = index.Flush()
	close(stop)
	readers.Wait()
	select {
	case count := <-errorsCh:
		t.Fatalf("partial generation with %d IDs", count)
	default:
	}
}

func TestNearest(t *testing.T) {
	coordinates := map[ImageID][2]float64{1: {0, 0}, 2: {0, 1}, 3: {10, 10}}
	entries := []GeoEntry{{1, 0, 0}, {2, 0, 1}, {3, 10, 10}}
	index, _ := New(1, entries, testConfig())
	defer index.Close()
	lookup := func(id ImageID) (float64, float64, bool) {
		coordinate, ok := coordinates[id]
		return coordinate[0], coordinate[1], ok
	}
	got := index.Nearest(nil, 0, 0.1, 2, lookup)
	if len(got) != 2 || got[0].ImageID != 1 || got[1].ImageID != 2 {
		t.Fatalf("nearest = %v", got)
	}
}
