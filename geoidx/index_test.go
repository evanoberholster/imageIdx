package geoidx

import (
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/golang/geo/s1"
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

func testIndexFromCells(t testing.TB, indexID uint64, cells map[S2Key][]ImageID) *Index {
	t.Helper()
	config, err := testConfig().withDefaults()
	if err != nil {
		t.Fatal(err)
	}
	return newIndex(indexID, 0, 1, cells, config)
}

func searchKey(index *Index, key S2Key) []ImageID {
	return index.SearchRanges(nil, []Range{{Start: key, End: key}})
}

func TestInvalidImageAndCoordinate(t *testing.T) {
	_, err := New(Spec{IndexID: 1, Entries: []Entry{{ImageID: 0, Latitude: 1, Longitude: 1}}}, testConfig())
	if !errors.Is(err, ErrInvalidImageID) {
		t.Fatalf("error = %v", err)
	}
	_, err = New(Spec{IndexID: 1, Entries: []Entry{{ImageID: 1, Latitude: 91, Longitude: 1}}}, testConfig())
	if !errors.Is(err, ErrInvalidCoordinate) {
		t.Fatalf("error = %v", err)
	}
}

func TestSparseCellsOneThroughFour(t *testing.T) {
	base := s2.CellID(DecodeS2Key(keyAt(t, 0, 0)))
	cells := make(map[S2Key][]ImageID)
	var id ImageID = 1
	for count := 1; count <= 4; count++ {
		key := EncodeS2Key(uint64(base.Advance(int64(count * 10))))
		for range count {
			cells[key] = append(cells[key], id)
			id++
		}
	}
	index := testIndexFromCells(t, 9, cells)
	defer index.Close()
	metadata := index.Metadata()
	if metadata.SparseCellCount != 4 || metadata.DenseCellCount != 0 {
		t.Fatalf("wrong partitions: %#v", metadata)
	}
	for key, expected := range cells {
		if got := searchKey(index, key); !slices.Equal(got, expected) {
			t.Fatalf("key %v: got %v want %v", key, got, expected)
		}
	}
}

func TestPromotionDemotionAndMove(t *testing.T) {
	keyA := keyAt(t, 37.7749, -122.4194)
	keyB := keyAt(t, 34.0522, -118.2437)
	index := testIndexFromCells(t, 1, map[S2Key][]ImageID{keyA: {1, 2, 3, 4}})
	defer index.Close()

	if err := index.Apply([]Mutation{{ImageID: 5, To: keyA}}); err != nil {
		t.Fatal(err)
	}
	if err := index.Flush(); err != nil {
		t.Fatal(err)
	}
	metadata := index.Metadata()
	if metadata.SparseCellCount != 0 || metadata.DenseCellCount != 1 {
		t.Fatalf("promotion failed: %#v", metadata)
	}

	if err := index.Apply([]Mutation{{ImageID: 5, From: keyA}}); err != nil {
		t.Fatal(err)
	}
	if err := index.Flush(); err != nil {
		t.Fatal(err)
	}
	metadata = index.Metadata()
	if metadata.SparseCellCount != 1 || metadata.DenseCellCount != 0 {
		t.Fatalf("demotion failed: %#v", metadata)
	}

	if err := index.Apply([]Mutation{{ImageID: 1, From: keyA, To: keyB}}); err != nil {
		t.Fatal(err)
	}
	if err := index.Flush(); err != nil {
		t.Fatal(err)
	}
	if got := searchKey(index, keyA); !slices.Equal(got, []ImageID{2, 3, 4}) {
		t.Fatalf("old cell = %v", got)
	}
	if got := searchKey(index, keyB); !slices.Equal(got, []ImageID{1}) {
		t.Fatalf("new cell = %v", got)
	}
}

func TestCoverRegionAndSearchRanges(t *testing.T) {
	index, err := New(Spec{IndexID: 1, Entries: []Entry{
		{ImageID: 1, Latitude: 37.7749, Longitude: -122.4194},
		{ImageID: 2, Latitude: 37.7790, Longitude: -122.4140},
		{ImageID: 3, Latitude: 34.0522, Longitude: -118.2437},
	}}, testConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer index.Close()

	lo := s2.LatLngFromDegrees(37.70, -122.52)
	hi := s2.LatLngFromDegrees(37.82, -122.35)
	ranges, err := index.CoverRegion(nil, s2.RectFromLatLng(lo).AddPoint(hi), CoverOptions{})
	if err != nil {
		t.Fatal(err)
	}
	got := index.SearchRanges(nil, ranges)
	slices.Sort(got)
	if !slices.Equal(got, []ImageID{1, 2}) {
		t.Fatalf("covered candidates = %v", got)
	}
}

func TestCoverRegionReusableWorkspaceAllocations(t *testing.T) {
	index, err := New(Spec{IndexID: 1}, testConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer index.Close()
	center := s2.PointFromLatLng(s2.LatLngFromDegrees(37.76, -122.43))
	var region s2.Region = s2.CapFromCenterAngle(center, s1.Angle(0.002))
	workspace := NewCoverWorkspace(64)
	ranges := make([]Range, 0, 64)
	options := CoverOptions{MaxCells: 32, Workspace: workspace}
	if _, err = index.CoverRegion(ranges, region, options); err != nil {
		t.Fatal(err)
	}
	if allocations := testing.AllocsPerRun(100, func() {
		result, coverErr := index.CoverRegion(ranges[:0], region, options)
		if coverErr != nil || len(result) == 0 {
			t.Fatalf("cover: %v, %d ranges", coverErr, len(result))
		}
	}); allocations != 0 {
		t.Fatalf("CoverRegion allocations = %v, want 0", allocations)
	}
}

func TestAtomicPromotionWithReaders(t *testing.T) {
	key := keyAt(t, 51.5074, -0.1278)
	index := testIndexFromCells(t, 1, map[S2Key][]ImageID{key: {1, 2, 3, 4}})
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
				count := len(searchKey(index, key))
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
	_ = index.Apply([]Mutation{{ImageID: 5, To: key}})
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
	entries := []Entry{{1, 0, 0}, {2, 0, 1}, {3, 10, 10}}
	index, _ := New(Spec{IndexID: 1, Entries: entries}, testConfig())
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
