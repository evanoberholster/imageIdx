package geoidx

import (
	"math"
	"math/rand"
	"slices"
	"sync"
	"testing"

	"github.com/golang/geo/s2"
)

func TestKeyConversions(t *testing.T) {
	key, err := KeyFromLatLng(37.7749, -122.4194)
	if err != nil {
		t.Fatalf("KeyFromLatLng: %v", err)
	}
	cell, err := CellIDFromKey(key)
	if err != nil {
		t.Fatalf("CellIDFromKey: %v", err)
	}
	if cell.Level() != S2Level {
		t.Fatalf("level = %d, want %d", cell.Level(), S2Level)
	}
	if EncodeKey(uint64(cell)) != key {
		t.Fatalf("round trip key = %d, want %d", EncodeKey(uint64(cell)), key)
	}
	if got := cell.LatLng().Distance(s2.LatLngFromDegrees(37.7749, -122.4194)).Radians() * earthRadiusMeters; got > 35 {
		t.Fatalf("cell center %.1f m from coordinate, want <= cell size", got)
	}
	for _, c := range [][2]float64{{91, 0}, {0, 181}, {math.NaN(), 0}} {
		if _, err := KeyFromLatLng(c[0], c[1]); err != ErrInvalidCoordinate {
			t.Fatalf("KeyFromLatLng(%v) = %v, want ErrInvalidCoordinate", c, err)
		}
	}
	if _, err := KeyFromCellID(0); err != ErrInvalidS2Cell {
		t.Fatalf("KeyFromCellID(0) = %v, want ErrInvalidS2Cell", err)
	}
	coarse := s2.CellIDFromLatLng(s2.LatLngFromDegrees(37.7749, -122.4194)).Parent(10)
	if _, err := KeyFromCellID(uint64(coarse)); err != ErrInvalidS2Level {
		t.Fatalf("KeyFromCellID(coarse) = %v, want ErrInvalidS2Level", err)
	}
}

func TestRangesFromCellIDs(t *testing.T) {
	cell := s2.CellIDFromLatLng(s2.LatLngFromDegrees(37.7749, -122.4194)).Parent(S2Level)
	ranges, err := RangesFromCellIDs(nil, []uint64{uint64(cell)})
	if err != nil {
		t.Fatalf("RangesFromCellIDs: %v", err)
	}
	want := Range{Start: EncodeKey(uint64(cell)), End: EncodeKey(uint64(cell))}
	if len(ranges) != 1 || ranges[0] != want {
		t.Fatalf("ranges = %+v, want [%+v]", ranges, want)
	}

	// A coarse cell expands to a contiguous descendant interval.
	coarse := cell.Parent(12)
	expanded, err := RangesFromCellIDs(nil, []uint64{uint64(coarse)})
	if err != nil {
		t.Fatalf("coarse: %v", err)
	}
	if len(expanded) != 1 {
		t.Fatalf("coarse ranges = %+v, want one interval", expanded)
	}
	if expanded[0].Start != EncodeKey(uint64(coarse.ChildBeginAtLevel(S2Level))) ||
		expanded[0].End != EncodeKey(uint64(coarse.ChildEndAtLevel(S2Level).Prev())) {
		t.Fatalf("coarse interval = %+v", expanded[0])
	}

	// Overlapping inputs merge into one interval.
	merged, err := RangesFromCellIDs(nil, []uint64{uint64(cell), uint64(cell)})
	if err != nil {
		t.Fatalf("overlap: %v", err)
	}
	if len(merged) != 1 || merged[0] != want {
		t.Fatalf("overlap ranges = %+v, want [%+v]", merged, want)
	}

	// Adjacent level-S2Level cells bridge their even gap key and merge.
	sibling := cell.Next()
	if sibling.Level() == S2Level {
		bridged, err := RangesFromCellIDs(nil, []uint64{uint64(cell), uint64(sibling)})
		if err != nil {
			t.Fatalf("sibling: %v", err)
		}
		if len(bridged) != 1 || bridged[0].Start != want.Start || bridged[0].End != EncodeKey(uint64(sibling)) {
			t.Fatalf("sibling ranges = %+v, want one bridged interval", bridged)
		}
	}
	if _, err := RangesFromCellIDs(nil, []uint64{0}); err != ErrInvalidS2Cell {
		t.Fatalf("invalid cell = %v", err)
	}
}

func TestCoverRegions(t *testing.T) {
	center := s2.LatLngFromDegrees(37.7749, -122.4194)
	key, _ := KeyFromLatLng(37.7749, -122.4194)

	// A tiny cap around the coordinate covers the coordinate's cell.
	ranges := CoverCap(nil, CapFromMeters(center, 25))
	if !keyInRanges(ranges, key) {
		t.Fatalf("25 m cap did not cover the query cell: %+v", ranges)
	}

	// A rectangle around the coordinate covers it too.
	rect := s2.RectFromCenterSize(center, s2.LatLngFromDegrees(0.01, 0.01))
	if rectRanges := CoverRect(nil, rect); !keyInRanges(rectRanges, key) {
		t.Fatalf("rect did not cover the query cell: %+v", rectRanges)
	}

	// A polygon around the coordinate covers it.
	poly := s2.PolygonFromLoops([]*s2.Loop{s2.LoopFromPoints([]s2.Point{
		s2.PointFromLatLng(s2.LatLngFromDegrees(37.78, -122.43)),
		s2.PointFromLatLng(s2.LatLngFromDegrees(37.78, -122.41)),
		s2.PointFromLatLng(s2.LatLngFromDegrees(37.77, -122.41)),
		s2.PointFromLatLng(s2.LatLngFromDegrees(37.77, -122.43)),
	})})
	if polyRanges := CoverPolygon(nil, poly); !keyInRanges(polyRanges, key) {
		t.Fatalf("polygon did not cover the query cell: %+v", polyRanges)
	}
}

func randomEntries(seed int64, n int, centerLat, centerLng, spread float64) []Entry {
	rng := rand.New(rand.NewSource(seed))
	entries := make([]Entry, 0, n)
	for i := 0; i < n; i++ {
		lat := centerLat + (rng.Float64()*2-1)*spread
		lng := centerLng + (rng.Float64()*2-1)*spread
		key, err := KeyFromLatLng(lat, lng)
		if err != nil {
			panic(err)
		}
		entries = append(entries, Entry{Key: key, ImageID: uint64(i + 1)})
	}
	return entries
}

func TestMemoryInsertSearchDelete(t *testing.T) {
	index, err := NewMemory(Config{})
	if err != nil {
		t.Fatalf("NewMemory: %v", err)
	}
	defer index.Close()

	entries := randomEntries(1, 2000, 37.7749, -122.4194, 0.2)
	if err := index.InsertBatch(entries); err != nil {
		t.Fatalf("InsertBatch: %v", err)
	}
	if index.Len() != 2000 {
		t.Fatalf("Len = %d, want 2000", index.Len())
	}

	// Every inserted key must be found and returned in ascending ImageID order.
	query := entries[0]
	got := index.SearchRanges(nil, []Range{{Start: query.Key, End: query.Key}})
	if !slices.IsSorted(got) {
		t.Fatalf("SearchRanges not sorted")
	}
	found := false
	for _, id := range got {
		if id == query.ImageID {
			found = true
		}
	}
	if !found || !index.Contains(query.Key, query.ImageID) {
		t.Fatalf("inserted entry not found at its key")
	}

	// Delete removes it.
	if err := index.Delete(query.Key, query.ImageID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if index.Contains(query.Key, query.ImageID) {
		t.Fatalf("entry still present after Delete")
	}
	if index.Len() != 1999 {
		t.Fatalf("Len after delete = %d, want 1999", index.Len())
	}

	// Re-inserting works.
	if err := index.Insert(query.Key, query.ImageID); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if !index.Contains(query.Key, query.ImageID) {
		t.Fatalf("entry missing after re-insert")
	}
}

func TestMove(t *testing.T) {
	index, _ := NewMemory(Config{})
	defer index.Close()
	const id = 42
	oldKey, _ := KeyFromLatLng(37.7749, -122.4194)
	newKey, _ := KeyFromLatLng(40.7128, -74.0060)
	if err := index.Insert(oldKey, id); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if err := index.Delete(oldKey, id); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := index.Insert(newKey, id); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if got := index.SearchRanges(nil, []Range{{Start: oldKey, End: oldKey}}); len(got) != 0 {
		t.Fatalf("old key still has %v", got)
	}
	if got := index.SearchRanges(nil, []Range{{Start: newKey, End: newKey}}); len(got) != 1 || got[0] != id {
		t.Fatalf("new key = %v, want [%d]", got, id)
	}
}

func TestTiedCell(t *testing.T) {
	index, _ := NewMemory(Config{})
	defer index.Close()
	key, _ := KeyFromLatLng(37.7749, -122.4194)
	if err := index.InsertBatch([]Entry{{Key: key, ImageID: 7}, {Key: key, ImageID: 3}, {Key: key, ImageID: 5}}); err != nil {
		t.Fatalf("InsertBatch: %v", err)
	}
	got := index.SearchRanges(nil, []Range{{Start: key, End: key}})
	if !slices.Equal(got, []uint64{3, 5, 7}) {
		t.Fatalf("tied cell = %v, want [3 5 7]", got)
	}
}

func TestNearest(t *testing.T) {
	index, _ := NewMemory(Config{})
	defer index.Close()
	centerLat, centerLng := 37.7749, -122.4194
	// Place images at increasing eastward offsets.
	entries := make([]Entry, 0, 10)
	for i := 1; i <= 10; i++ {
		key, err := KeyFromLatLng(centerLat, centerLng+float64(i)*0.0005)
		if err != nil {
			t.Fatalf("KeyFromLatLng: %v", err)
		}
		entries = append(entries, Entry{Key: key, ImageID: uint64(i)})
	}
	if err := index.InsertBatch(entries); err != nil {
		t.Fatalf("InsertBatch: %v", err)
	}
	nearest, err := index.Nearest(nil, centerLat, centerLng, 3)
	if err != nil {
		t.Fatalf("Nearest: %v", err)
	}
	if len(nearest) != 3 {
		t.Fatalf("Nearest returned %d, want 3", len(nearest))
	}
	for i := 1; i < len(nearest); i++ {
		if nearest[i-1].Distance > nearest[i].Distance {
			t.Fatalf("nearest not sorted: %+v", nearest)
		}
	}
}

func TestPersistenceRoundTrip(t *testing.T) {
	dir := t.TempDir()
	index, err := New(dir, Config{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	entries := randomEntries(2, 1500, 51.5074, -0.1278, 0.2)
	if err := index.InsertBatch(entries); err != nil {
		t.Fatalf("InsertBatch: %v", err)
	}
	target := entries[100]
	if err := index.Checkpoint(); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	// Extra mutations after the checkpoint must survive a second checkpoint.
	extraKey, _ := KeyFromLatLng(48.8566, 2.3522)
	if err := index.Insert(extraKey, 999999); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if err := index.Checkpoint(); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	if err := index.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	reopened, err := Open(dir, Config{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer reopened.Close()
	if reopened.Len() != 1501 {
		t.Fatalf("reopened Len = %d, want 1501", reopened.Len())
	}
	if !reopened.Contains(target.Key, target.ImageID) {
		t.Fatalf("target missing after reopen")
	}
	if !reopened.Contains(extraKey, 999999) {
		t.Fatalf("extra missing after reopen")
	}
}

func TestUncheckpointedMutationsLost(t *testing.T) {
	dir := t.TempDir()
	index, err := New(dir, Config{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	key, _ := KeyFromLatLng(37.7749, -122.4194)
	if err := index.Insert(key, 5); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if err := index.Close(); err != nil { // no Checkpoint
		t.Fatalf("Close: %v", err)
	}
	reopened, err := Open(dir, Config{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer reopened.Close()
	if reopened.Len() != 0 {
		t.Fatalf("reopened Len = %d, want 0", reopened.Len())
	}
}

func TestCompactionWithDeletes(t *testing.T) {
	index, _ := NewMemory(Config{})
	defer index.Close()
	entries := randomEntries(3, 3000, 35.6762, 139.6503, 0.15)
	if err := index.InsertBatch(entries); err != nil {
		t.Fatalf("InsertBatch: %v", err)
	}
	// Delete every third entry.
	for i := 0; i < len(entries); i += 3 {
		if err := index.Delete(entries[i].Key, entries[i].ImageID); err != nil {
			t.Fatalf("Delete: %v", err)
		}
	}
	wantLen := uint64(3000 - 1000)
	if err := index.Compact(); err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if index.Len() != wantLen {
		t.Fatalf("Len after compact = %d, want %d", index.Len(), wantLen)
	}
	// A deleted entry must stay gone, a kept entry must remain.
	deleted := entries[0]
	if index.Contains(deleted.Key, deleted.ImageID) {
		t.Fatalf("deleted entry present after compact")
	}
	kept := entries[1]
	if !index.Contains(kept.Key, kept.ImageID) {
		t.Fatalf("kept entry missing after compact")
	}
}

func TestConcurrentReadersDuringCompact(t *testing.T) {
	dir := t.TempDir()
	index, err := New(dir, Config{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	entries := randomEntries(4, 5000, 52.52, 13.405, 0.3)
	if err := index.InsertBatch(entries); err != nil {
		t.Fatalf("InsertBatch: %v", err)
	}
	if err := index.Checkpoint(); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}

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
				_ = index.SearchRanges(nil, []Range{{Start: 0, End: S2Key(^uint64(0))}})
				_, _ = index.Nearest(nil, 52.52, 13.405, 5)
			}
		}()
	}
	for round := 0; round < 20; round++ {
		key, _ := KeyFromLatLng(52.52+float64(round)*0.0001, 13.405)
		if err := index.Insert(key, uint64(1_000_000+round)); err != nil {
			t.Fatalf("Insert: %v", err)
		}
		if err := index.Checkpoint(); err != nil {
			t.Fatalf("Checkpoint: %v", err)
		}
	}
	close(stop)
	wg.Wait()
	if err := index.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}
