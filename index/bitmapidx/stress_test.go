package bitmapidx

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	common "github.com/evanoberholster/imageIdx/common"
)

// stressInt reads an environment override for a stress parameter, falling
// back to def. It lets a long soak run scale up without editing the test.
func stressInt(name string, def int) int {
	if raw := os.Getenv(name); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			return n
		}
	}
	return def
}

// idsSortedUnique reports whether ids is ascending with no duplicates.
func idsSortedUnique(ids []ImageID) bool {
	for i := 1; i < len(ids); i++ {
		if ids[i] <= ids[i-1] {
			return false
		}
	}
	return true
}

// TestStressMixedWorkload drives disjoint writers across enum, text,
// continuous, and geo namespaces while readers query and a maintainer
// checkpoints, vacuums, and compacts the persistent store. After the writers
// stop it folds the deltas, reopens the store from disk, and verifies every
// membership against the writers' reference model.
func TestStressMixedWorkload(t *testing.T) {
	if testing.Short() {
		t.Skip("stress test")
	}
	writers := stressInt("BITMAPIDX_STRESS_WRITERS", 4)
	readers := stressInt("BITMAPIDX_STRESS_READERS", 3)
	rounds := stressInt("BITMAPIDX_STRESS_ROUNDS", 24)
	const idsPerWriter = 192
	const (
		enumType       = common.Namespace(100)
		textType       = common.Namespace(101)
		continuousType = common.Namespace(102)
		geoType        = common.Namespace(103)
	)

	keys := make([]uint64, 0, 32)
	for lat := -60.0; lat <= 60; lat += 30 {
		for lng := -120.0; lng <= 120; lng += 60 {
			key, err := KeyFromLatLng(lat, lng)
			if err != nil {
				t.Fatalf("KeyFromLatLng: %v", err)
			}
			keys = append(keys, key)
		}
	}

	dir := t.TempDir()
	config := NamespaceConfig{ActiveTarget: 64, HardDeltaLimit: 256, MaxDeltaValues: 32}
	store, err := NewIndex(dir, []NamespaceSpec{
		enumNamespace(enumType), textNamespace(textType), continuousNamespace(continuousType), geoNamespaceFor(geoType),
	}, config, DurabilityOS)
	if err != nil {
		t.Fatalf("NewIndex: %v", err)
	}
	tag, _ := store.Lookup(enumType)
	title, _ := store.Lookup(textType)
	iso, _ := store.Lookup(continuousType)
	loc, _ := store.Lookup(geoType)

	type expect struct {
		enumValue string
		cont      int64
		geo       uint64
	}
	reference := make([][]expect, writers)

	var mu sync.Mutex
	var firstErr error
	fail := func(err error) {
		if err == nil {
			return
		}
		mu.Lock()
		if firstErr == nil {
			firstErr = err
		}
		mu.Unlock()
	}

	stop := make(chan struct{})
	var writersWG, workersWG sync.WaitGroup

	for w := 0; w < writers; w++ {
		writersWG.Add(1)
		go func(w int) {
			defer writersWG.Done()
			exp := make([]expect, idsPerWriter)
			curGeo := make([]uint64, idsPerWriter)
			prevText := make([][]string, idsPerWriter)
			prevCont := make([]int64, idsPerWriter)
			tagBatch := tag.NewBatch()
			defer tagBatch.Close()
			titleBatch := title.NewBatch()
			defer titleBatch.Close()
			isoBatch := iso.NewBatch()
			defer isoBatch.Close()
			locBatch := loc.NewBatch()
			defer locBatch.Close()
			apply := func(batch *Batch, stage func(*Batch) error) bool {
				if err := batch.Reset(); err != nil {
					fail(err)
					return false
				}
				if err := stage(batch); err != nil {
					fail(err)
					return false
				}
				if err := batch.Apply(); err != nil {
					fail(err)
					return false
				}
				return true
			}
			for round := 0; round < rounds; round++ {
				enumAdds := make([]Entry, 0, idsPerWriter)
				textChanges := make([]TextReplacement, 0, idsPerWriter)
				contMoves := make([]Move, 0, idsPerWriter)
				geoMoves := make([]Move, 0, idsPerWriter)
				for i := 0; i < idsPerWriter; i++ {
					id := ImageID(w*idsPerWriter + i + 1)
					enumValue := fmt.Sprintf("tag-%d-%d", w, id)
					enumAdds = append(enumAdds, Entry{ImageID: id, Value: StringValue(enumValue)})

					nextText := []string{"shared", fmt.Sprintf("w%d-r%d", w, round)}
					textChanges = append(textChanges, TextReplacement{ImageID: id, Before: prevText[i], After: nextText})
					prevText[i] = nextText

					toCont := int64((round*7 + i + w) % 4096)
					contMove := Move{ImageID: id, To: toCont, HasTo: true}
					if round > 0 {
						contMove.From = prevCont[i]
						contMove.HasFrom = true
					}
					contMoves = append(contMoves, contMove)
					prevCont[i] = toCont

					toGeo := keys[(round+i+w)%len(keys)]
					geoMove := Move{ImageID: id, To: int64(toGeo), HasTo: true}
					if round > 0 {
						geoMove.From = int64(curGeo[i])
						geoMove.HasFrom = true
					}
					geoMoves = append(geoMoves, geoMove)
					curGeo[i] = toGeo

					exp[i] = expect{enumValue: enumValue, cont: toCont, geo: toGeo}
				}
				if !apply(tagBatch, func(b *Batch) error { return b.Add(enumAdds...) }) {
					return
				}
				if !apply(titleBatch, func(b *Batch) error { return b.ReplaceText(textChanges...) }) {
					return
				}
				if !apply(isoBatch, func(b *Batch) error { return b.Move(contMoves...) }) {
					return
				}
				if !apply(locBatch, func(b *Batch) error { return b.Move(geoMoves...) }) {
					return
				}
				for _, index := range []*NamespaceIndex{tag, title, iso, loc} {
					if err := index.Barrier(); err != nil {
						fail(err)
						return
					}
				}
			}
			reference[w] = exp
		}(w)
	}

	// Maintenance: alternate checkpoint and vacuum, with an occasional
	// namespace-level compact to exercise rotation and dictionary sealing.
	workersWG.Add(1)
	go func() {
		defer workersWG.Done()
		for n := 0; ; n++ {
			select {
			case <-stop:
				return
			default:
			}
			if err := store.Checkpoint(); err != nil {
				fail(err)
				return
			}
			if n%2 == 0 {
				if err := store.Vacuum(); err != nil {
					fail(err)
					return
				}
			}
			if n%3 == 0 {
				if err := tag.Compact(); err != nil {
					fail(err)
					return
				}
			}
			time.Sleep(time.Millisecond)
		}
	}()

	// Readers: run every query shape against the live store, asserting the
	// sorted-unique invariant and surfacing errors.
	for r := 0; r < readers; r++ {
		workersWG.Add(1)
		go func() {
			defer workersWG.Done()
			compiled, err := title.CompileText("shared")
			if err != nil {
				fail(err)
				return
			}
			for {
				select {
				case <-stop:
					return
				default:
				}
				if ids, err := tag.ValueIDs(nil, StringValue("tag-0-1")); err != nil {
					fail(err)
					return
				} else if !idsSortedUnique(ids) {
					fail(errors.New("ValueIDs not sorted unique"))
					return
				}
				if ids, err := title.MatchText("shared"); err != nil {
					fail(err)
					return
				} else if !idsSortedUnique(ids.AppendTo(nil)) {
					fail(errors.New("MatchText not sorted unique"))
					return
				}
				if _, err := compiled.Match(); err != nil {
					fail(err)
					return
				}
				if _, err := compiled.Count(); err != nil {
					fail(err)
					return
				}
				if _, err := iso.Range(Between(0, 4096)); err != nil {
					fail(err)
					return
				}
				if _, err := loc.GeoRanges([]GeoRange{{Start: keys[0], End: keys[0]}}); err != nil {
					fail(err)
					return
				}
				loc.EstimateGeoRanges([]GeoRange{{Start: 0, End: ^uint64(0)}})
				loc.AppendTo(nil)
				title.Prefix([]byte("shared"), func(NamespaceKey, []byte) bool { return true })
				if ids := tag.AppendTo(nil); !idsSortedUnique(ids) {
					fail(errors.New("AppendTo not sorted unique"))
					return
				}
			}
		}()
	}

	writersWG.Wait()
	close(stop)
	workersWG.Wait()

	if firstErr != nil {
		store.Close()
		t.Fatalf("stress workload error: %v", firstErr)
	}
	for _, index := range store.Namespaces() {
		if err := index.WaitIdle(context.Background()); err != nil {
			t.Fatalf("WaitIdle: %v", err)
		}
	}
	if err := store.Checkpoint(); err != nil {
		store.Close()
		t.Fatalf("final checkpoint: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	reopened, err := OpenIndex(dir, config, DurabilityOS)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	rtag, _ := reopened.Lookup(enumType)
	rtitle, _ := reopened.Lookup(textType)
	riso, _ := reopened.Lookup(continuousType)
	rloc, _ := reopened.Lookup(geoType)

	total := writers * idsPerWriter
	for w := 0; w < writers; w++ {
		for i := 0; i < idsPerWriter; i++ {
			id := ImageID(w*idsPerWriter + i + 1)
			exp := reference[w][i]
			if ok, err := rtag.Contains(id, StringValue(exp.enumValue)); err != nil || !ok {
				t.Fatalf("enum id=%d value=%q ok=%v err=%v", id, exp.enumValue, ok, err)
			}
			if ok, err := riso.Contains(id, IntValue(exp.cont)); err != nil || !ok {
				t.Fatalf("continuous id=%d value=%d ok=%v err=%v", id, exp.cont, ok, err)
			}
			if ok, err := rloc.Contains(id, GeoValue(exp.geo)); err != nil || !ok {
				t.Fatalf("geo id=%d value=%d ok=%v err=%v", id, exp.geo, ok, err)
			}
		}
	}
	shared, err := rtitle.MatchText("shared")
	if err != nil {
		t.Fatalf("MatchText shared: %v", err)
	}
	if got := shared.AppendTo(nil); len(got) != total {
		t.Fatalf("shared text matched %d ids, want %d", len(got), total)
	}
	for w := 0; w < writers; w++ {
		roundToken := fmt.Sprintf("w%d-r%d", w, rounds-1)
		match, err := rtitle.MatchText(roundToken)
		if err != nil {
			t.Fatalf("MatchText %s: %v", roundToken, err)
		}
		if got := match.AppendTo(nil); len(got) != idsPerWriter {
			t.Fatalf("%s matched %d ids, want %d", roundToken, len(got), idsPerWriter)
		}
	}
	if ids := rtag.AppendTo(nil); len(ids) != total || !idsSortedUnique(ids) {
		t.Fatalf("AppendTo returned %d ids (sorted unique=%v), want %d", len(ids), idsSortedUnique(ids), total)
	}
	if ok, err := rtag.Contains(1, StringValue("definitely-absent")); err != nil || ok {
		t.Fatalf("absent value reported ok=%v err=%v", ok, err)
	}
}

// TestStressReaderGuardUnderMaintenance hammers the snapshot readers against
// continuous checkpoint and vacuum generations. It targets the reader-token
// lifetime bug: a reader that releases its guard before its last snapshot
// access can touch an unmapped segment and crash.
func TestStressReaderGuardUnderMaintenance(t *testing.T) {
	if testing.Short() {
		t.Skip("stress test")
	}
	dir := t.TempDir()
	store, err := NewIndex(dir, []NamespaceSpec{geoNamespaceFor(common.NamespaceLocation)}, NamespaceConfig{}, DurabilityOS)
	if err != nil {
		t.Fatalf("NewIndex: %v", err)
	}
	loc, _ := store.Lookup(common.NamespaceLocation)
	key := sfKey(t)
	seed := loc.NewBatch()
	for id := 1; id <= 512; id++ {
		if err := seed.Move(Move{ImageID: ImageID(id), To: int64(key)}); err != nil {
			t.Fatalf("Move: %v", err)
		}
	}
	if err := seed.Apply(); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	seed.Close()
	if err := loc.Barrier(); err != nil {
		t.Fatalf("Barrier: %v", err)
	}
	if err := store.Checkpoint(); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	ranges := []GeoRange{{Start: key, End: key}}
	lookup := func(ImageID) (float64, float64, bool) { return 37.7749, -122.4194, true }

	stop := make(chan struct{})
	var wg sync.WaitGroup
	for r := 0; r < 8; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				loc.AppendTo(nil)
				loc.EstimateGeoRanges(ranges)
				loc.GeoRanges(ranges)
				loc.CountGeoRanges(ranges)
				loc.Nearest(nil, 37.7749, -122.4194, 4, lookup)
				loc.FilterGeoRanges(nil, []ImageID{1, 2, 3}, ranges)
			}
		}()
	}
	for n := 0; n < 30; n++ {
		if err := store.Vacuum(); err != nil {
			t.Errorf("Vacuum: %v", err)
			break
		}
		if err := store.Checkpoint(); err != nil {
			t.Errorf("Checkpoint: %v", err)
			break
		}
	}
	close(stop)
	wg.Wait()
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}
