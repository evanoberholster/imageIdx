package geoidx

import (
	"cmp"
	"slices"
	"sync"
	"testing"

	common "github.com/evanoberholster/imageIdx/common"
	"github.com/golang/geo/s2"
)

// nearestEdgeCoords returns coordinates that exercise ties, the date line,
// polar regions, and cross-face boundaries.
func nearestEdgeCoords() []s2.LatLng {
	raw := [][2]float64{
		{0, 0}, {0, 0}, {0, 0}, // same-cell triple
		{0.0005, 0.0005},
		{0, 179.999}, {0, -179.999}, // date line
		{89.9, 0}, {89.9, 120}, {-89.9, 0}, {-89.9, -120},
		{45, 45}, {-45, -45}, {37.7749, -122.4194},
	}
	coords := make([]s2.LatLng, len(raw))
	for i, c := range raw {
		coords[i] = s2.LatLngFromDegrees(c[0], c[1])
	}
	return coords
}

func buildEdgeIndex(t testing.TB) (*Index, []Entry) {
	t.Helper()
	coords := nearestEdgeCoords()
	entries := make([]Entry, len(coords))
	for i, ll := range coords {
		key, err := KeyFromLatLng(ll.Lat.Degrees(), ll.Lng.Degrees())
		if err != nil {
			t.Fatal(err)
		}
		entries[i] = Entry{Key: key, ImageID: common.ImageID(i + 1)}
	}
	index, err := NewMemory(Config{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = index.Close() })
	if err := index.InsertBatch(entries); err != nil {
		t.Fatal(err)
	}
	if err := index.Compact(); err != nil {
		t.Fatal(err)
	}
	return index, entries
}

func nearestOracle(entries []Entry, center s2.LatLng, k int) []Result {
	type hit struct {
		result Result
		meters float64
	}
	all := make([]hit, len(entries))
	for i, e := range entries {
		meters := keyDistanceMeters(e.Key, center)
		all[i] = hit{result: Result{ImageID: e.ImageID, Distance: metersToUint32(meters)}, meters: meters}
	}
	slices.SortFunc(all, func(a, b hit) int {
		if a.meters != b.meters {
			return cmp.Compare(a.meters, b.meters)
		}
		return cmp.Compare(a.result.ImageID, b.result.ImageID)
	})
	if k > len(all) {
		k = len(all)
	}
	out := make([]Result, k)
	for i := range out {
		out[i] = all[i].result
	}
	return out
}

func TestNearestEdgeCases(t *testing.T) {
	empty, err := NewMemory(Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer empty.Close()
	if got, err := empty.Nearest(nil, 0, 0, 10); err != nil || len(got) != 0 {
		t.Fatalf("empty Nearest = %v, %v; want empty", got, err)
	}

	index, entries := buildEdgeIndex(t)
	queries := []s2.LatLng{
		s2.LatLngFromDegrees(0, 0),
		s2.LatLngFromDegrees(0, 180),
		s2.LatLngFromDegrees(89.9, 0),
		s2.LatLngFromDegrees(-89.9, 0),
		s2.LatLngFromDegrees(37.7749, -122.4194),
	}
	for _, center := range queries {
		for _, k := range []int{1, 3, 10, len(entries), len(entries) + 5} {
			got, err := index.Nearest(nil, center.Lat.Degrees(), center.Lng.Degrees(), k)
			if err != nil {
				t.Fatal(err)
			}
			want := nearestOracle(entries, center, k)
			if !slices.Equal(got, want) {
				t.Fatalf("Nearest(%v, k=%d) = %+v, want %+v", center, k, got, want)
			}
		}
	}
}

// TestNearestSameCellTies verifies equal-distance ties break by ImageID.
func TestNearestSameCellTies(t *testing.T) {
	index, entries := buildEdgeIndex(t)
	center := s2.LatLngFromDegrees(0, 0)
	got, err := index.Nearest(nil, 0, 0, 3)
	if err != nil {
		t.Fatal(err)
	}
	// Three entries share the (0,0) cell and are the nearest; ImageIDs ascend.
	if len(got) != 3 || got[0].ImageID != 1 || got[1].ImageID != 2 || got[2].ImageID != 3 {
		t.Fatalf("same-cell tie = %+v", got)
	}
	if !slices.Equal(got, nearestOracle(entries, center, 3)) {
		t.Fatalf("same-cell tie differs from oracle: %+v", got)
	}
}

func TestRadiusEdgeCases(t *testing.T) {
	index, entries := buildEdgeIndex(t)
	for _, c := range [][3]float64{
		{0, 0, 100}, {0, 0, 100000}, {0, 179.999, 100}, {0, 180, 100000},
		{89.9, 0, 5000}, {-89.9, 0, 5000},
	} {
		center := s2.LatLngFromDegrees(c[0], c[1])
		radius := c[2]
		want := make([]uint64, 0)
		for _, e := range entries {
			if keyDistanceMeters(e.Key, center) <= radius {
				want = append(want, e.ImageID)
			}
		}
		slices.Sort(want)
		got, err := index.SearchRadius(nil, c[0], c[1], radius)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(got, want) {
			t.Fatalf("radius center=%v r=%v: got %v, want %v", center, radius, got, want)
		}
	}
}

// TestNearestDuringMutationPublication races nearest queries with writes under
// the race detector.
func TestNearestDuringMutationPublication(t *testing.T) {
	index, err := NewMemory(Config{TargetDelta: 16, CompactionTrigger: 64, MaxDelta: 512})
	if err != nil {
		t.Fatal(err)
	}
	defer index.Close()
	insertKeys(t, index, testKeys(500), 1)

	stop := make(chan struct{})
	var wg sync.WaitGroup
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				_, _ = index.Nearest(nil, testLat, testLng, 10)
				_, _ = index.SearchRadius(nil, testLat, testLng, 2000)
			}
		}()
	}
	insertKeys(t, index, testKeys(1000), 10_000)
	close(stop)
	wg.Wait()
	if err := index.Compact(); err != nil {
		t.Fatal(err)
	}
	if got := index.Len(); got != 1500 {
		t.Fatalf("Len = %d, want 1500", got)
	}
}
