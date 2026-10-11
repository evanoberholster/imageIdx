package geoidx

import (
	"cmp"
	common "github.com/evanoberholster/imageIdx/common"
	"github.com/evanoberholster/imageIdx/roar"
	"github.com/golang/geo/s2"
	"math/rand"
	"slices"
	"sync"
	"testing"
)

const (
	testLat  = 37.7749
	testLng  = -122.4194
	testSpan = 0.4
)

func testEntries(t testing.TB, n int, spread float64) (*Index, []Entry) {
	t.Helper()
	rng := rand.New(rand.NewSource(7))
	entries := make([]Entry, n)
	for i := range entries {
		lat := testLat + (rng.Float64()*2-1)*spread
		lng := testLng + (rng.Float64()*2-1)*spread
		key, err := KeyFromLatLng(lat, lng)
		if err != nil {
			t.Fatalf("KeyFromLatLng: %v", err)
		}
		entries[i] = Entry{Key: key, ImageID: uint64(i + 1)}
	}
	index, err := NewMemory(Config{})
	if err != nil {
		t.Fatalf("NewMemory: %v", err)
	}
	t.Cleanup(func() { _ = index.Close() })
	if err := index.InsertBatch(entries); err != nil {
		t.Fatalf("InsertBatch: %v", err)
	}
	if err := index.Compact(); err != nil {
		t.Fatalf("Compact: %v", err)
	}
	return index, entries
}

func testCover(radiusMeters float64) []Range {
	center := s2.LatLngFromDegrees(testLat, testLng)
	return CoverCap(nil, CapFromMeters(center, radiusMeters))
}

func intersectSorted(a, b []uint64) []uint64 {
	out := make([]uint64, 0)
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		switch {
		case a[i] < b[j]:
			i++
		case a[i] > b[j]:
			j++
		default:
			out = append(out, a[i])
			i++
			j++
		}
	}
	return out
}

func TestCollectRangesMatchesSearchRanges(t *testing.T) {
	index, _ := testEntries(t, 50_000, testSpan)
	ranges := testCover(3000)
	m := roar.NewMutableBitmap()
	index.CollectRanges(m, ranges)
	got := m.AppendTo(nil)
	want := index.SearchRanges(nil, ranges)
	if !slices.Equal(got, want) {
		t.Fatalf("CollectRanges = %d ids, SearchRanges = %d ids", len(got), len(want))
	}
	if !slices.IsSorted(got) {
		t.Fatal("CollectRanges output not ascending")
	}
}

func TestFilterRangesIntersectsCandidates(t *testing.T) {
	index, _ := testEntries(t, 50_000, testSpan)
	ranges := testCover(3000)
	geo := index.SearchRanges(nil, ranges)

	for _, size := range []int{10, 500, 50_000} {
		candidates := make([]uint64, 0, size)
		for id := uint64(1); id <= uint64(size); id++ {
			candidates = append(candidates, id)
		}
		bit := roar.MustFromSorted(candidates)
		m := roar.NewMutableBitmap()
		index.FilterRanges(m, bit, ranges)
		got := m.AppendTo(nil)
		want := intersectSorted(geo, candidates)
		if !slices.Equal(got, want) {
			t.Fatalf("FilterRanges size=%d: got %d ids, want %d", size, len(got), len(want))
		}
	}

	// Empty candidates and empty ranges are no-ops.
	m := roar.NewMutableBitmap()
	index.FilterRanges(m, roar.Bitmap{}, ranges)
	if n := m.Cardinality(); n != 0 {
		t.Fatalf("empty candidates produced %d", n)
	}
}

func TestRadiusResultsAreExactAtCellResolution(t *testing.T) {
	index, entries := testEntries(t, 50_000, testSpan)
	center := s2.LatLngFromDegrees(testLat, testLng)
	for _, radius := range []float64{100, 1000, 5000} {
		// Oracle: every entry whose authoritative cell center is within radius.
		wantIDs := make([]uint64, 0)
		wantResults := make([]Result, 0)
		for _, e := range entries {
			d := keyDistanceMeters(e.Key, center)
			if d <= radius {
				wantIDs = append(wantIDs, e.ImageID)
				wantResults = append(wantResults, Result{ImageID: e.ImageID, Distance: metersToUint32(d)})
			}
		}
		slices.Sort(wantIDs)

		gotIDs, err := index.SearchRadius(nil, testLat, testLng, radius)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(gotIDs, wantIDs) {
			t.Fatalf("radius %.0f: SearchRadius got %d, want %d", radius, len(gotIDs), len(wantIDs))
		}
		gotResults, err := index.SearchRadiusResults(nil, testLat, testLng, radius)
		if err != nil {
			t.Fatal(err)
		}
		if len(gotResults) != len(wantResults) {
			t.Fatalf("radius %.0f: results got %d, want %d", radius, len(gotResults), len(wantResults))
		}
		// Results sort by (distance, id); compare as sets then ordering.
		slices.SortFunc(gotResults, compareResults)
		slices.SortFunc(wantResults, compareResults)
		if !slices.Equal(gotResults, wantResults) {
			t.Fatalf("radius %.0f: results differ", radius)
		}
	}
}

// TestNearestDifferential checks Nearest against a brute-force top-k by exact
// cell-center distance, which is the authoritative metric. Public results round
// to meters, but ordering is by the precise distance then ImageID.
func TestNearestDifferential(t *testing.T) {
	index, entries := testEntries(t, 30_000, testSpan)
	center := s2.LatLngFromDegrees(testLat, testLng)
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
	for _, k := range []int{1, 10, 100, 1000} {
		got, err := index.Nearest(nil, testLat, testLng, k)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != k {
			t.Fatalf("k=%d: got %d", k, len(got))
		}
		want := make([]Result, k)
		for n := range want {
			want[n] = all[n].result
		}
		if !slices.Equal(got, want) {
			t.Fatalf("k=%d: Nearest differs from brute force", k)
		}
	}
}

func TestDeltaBounded(t *testing.T) {
	index, err := NewMemory(Config{TargetDelta: 16, CompactionTrigger: 64, MaxDelta: 256})
	if err != nil {
		t.Fatal(err)
	}
	defer index.Close()
	for i := 0; i < 1000; i++ {
		key, err := KeyFromLatLng(testLat+float64(i)*1e-4, testLng)
		if err != nil {
			t.Fatal(err)
		}
		if err := index.Insert(key, uint64(i+1)); err != nil {
			t.Fatal(err)
		}
	}
	if got := index.Len(); got != 1000 {
		t.Fatalf("Len = %d, want 1000", got)
	}
	// Background compaction must keep the pending delta bounded; an explicit
	// Compact drains it fully without losing data.
	if err := index.Compact(); err != nil {
		t.Fatal(err)
	}
	if got := index.Len(); got != 1000 {
		t.Fatalf("Len after Compact = %d, want 1000", got)
	}
	index.mu.Lock()
	pending := index.pendingLocked()
	generation := index.dirGen
	index.mu.Unlock()
	if pending != 0 {
		t.Fatalf("pending delta %d not drained by Compact", pending)
	}
	if generation < 2 {
		t.Fatalf("base generation %d, want at least 2 after compaction", generation)
	}
}

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

// TestCollectorPoolReusePreservesBitmaps checks that a bitmap materialized from
// one Collect/Filter call stays valid after the pooled collector is churned by
// many later calls. The collector stages IDs through MutableBitmap.AddMany,
// which copies, so reuse must never disturb earlier results.
func TestCollectorPoolReusePreservesBitmaps(t *testing.T) {
	index, entries := testEntries(t, 50_000, testSpan)
	ranges := testCover(5000)
	want := index.SearchRanges(nil, ranges)
	if len(want) <= collectChunk {
		t.Fatalf("test needs > %d matches, got %d", collectChunk, len(want))
	}
	candidates := make([]uint64, 0, len(entries))
	for id := uint64(1); id <= uint64(len(entries)); id++ {
		candidates = append(candidates, id)
	}
	bit := roar.MustFromSorted(candidates)

	first := roar.NewMutableBitmap()
	index.CollectRanges(first, ranges)
	if got := first.AppendTo(nil); !slices.Equal(got, want) {
		t.Fatalf("initial collect mismatch: got %d ids, want %d", len(got), len(want))
	}

	for i := 0; i < 2000; i++ {
		m := roar.NewMutableBitmap()
		if i%2 == 0 {
			index.CollectRanges(m, ranges)
		} else {
			index.FilterRanges(m, bit, ranges)
		}
		if got := m.AppendTo(nil); !slices.Equal(got, want) {
			t.Fatalf("churn %d mismatch: got %d ids, want %d", i, len(got), len(want))
		}
	}

	// The bitmap from before the churn must still be intact.
	if got := first.AppendTo(nil); !slices.Equal(got, want) {
		t.Fatalf("bitmap corrupted after pool reuse: got %d ids, want %d", len(got), len(want))
	}
	if got := first.Cardinality(); got != uint64(len(want)) {
		t.Fatalf("bitmap cardinality after pool reuse = %d, want %d", got, len(want))
	}
}

// TestCollectorPoolConcurrent drives the collector pool from many goroutines and
// requires every materialized bitmap to be correct. Run with -race.
func TestCollectorPoolConcurrent(t *testing.T) {
	index, entries := testEntries(t, 50_000, testSpan)
	ranges := testCover(5000)
	want := index.SearchRanges(nil, ranges)
	candidates := make([]uint64, 0, len(entries))
	for id := uint64(1); id <= uint64(len(entries)); id++ {
		candidates = append(candidates, id)
	}
	bit := roar.MustFromSorted(candidates)

	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		g := g
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 400; i++ {
				m := roar.NewMutableBitmap()
				if (g+i)%2 == 0 {
					index.CollectRanges(m, ranges)
				} else {
					index.FilterRanges(m, bit, ranges)
				}
				if got := m.AppendTo(nil); !slices.Equal(got, want) {
					t.Errorf("goroutine %d iter %d: got %d ids, want %d", g, i, len(got), len(want))
					return
				}
			}
		}()
	}
	wg.Wait()
}
