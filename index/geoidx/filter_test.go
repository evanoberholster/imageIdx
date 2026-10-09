package geoidx

import (
	"cmp"
	"math/rand"
	"slices"
	"testing"

	"github.com/evanoberholster/imageIdx/roar"
	"github.com/golang/geo/s2"
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
	generation := index.base.gen
	index.mu.Unlock()
	if pending != 0 {
		t.Fatalf("pending delta %d not drained by Compact", pending)
	}
	if generation < 2 {
		t.Fatalf("base generation %d, want at least 2 after compaction", generation)
	}
}
