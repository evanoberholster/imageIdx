package index_test

import (
	"context"
	"slices"
	"sync"
	"sync/atomic"
	"testing"

	schematypes "github.com/evanoberholster/imageIdx/schema"
	"github.com/evanoberholster/imageIdx/index"
	"github.com/evanoberholster/imageIdx/index/geoidx"
	"github.com/evanoberholster/imageIdx/search"
)

func geoTestSchema(t testing.TB) *schematypes.Definitions {
	t.Helper()
	schema := schematypes.NewDefinitions()
	if err := schema.RegisterAll(
		schematypes.NamespaceDef{Namespace: 2, Name: "geo2", Kind: schematypes.KindGeo, Entity: schematypes.EntityImage},
		schematypes.NamespaceDef{Namespace: 3, Name: "geo3", Kind: schematypes.KindGeo, Entity: schematypes.EntityImage},
	); err != nil {
		t.Fatal(err)
	}
	return schema
}

func geoKeys(t testing.TB) (a, b, c geoidx.S2Key) {
	t.Helper()
	var err error
	if a, err = geoidx.KeyFromLatLng(10, 10); err != nil {
		t.Fatal(err)
	}
	if b, err = geoidx.KeyFromLatLng(20, 20); err != nil {
		t.Fatal(err)
	}
	if c, err = geoidx.KeyFromLatLng(30, 30); err != nil {
		t.Fatal(err)
	}
	if a == b || b == c || a == c {
		t.Fatal("test keys collide")
	}
	return a, b, c
}

func geoDB(t testing.TB) (*index.Store, *geoidx.Index, geoidx.S2Key, geoidx.S2Key, geoidx.S2Key) {
	t.Helper()
	db, err := index.Create(t.TempDir(), index.Spec{Schema: geoTestSchema(t)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	idx, ok := db.Geo(2)
	if !ok {
		t.Fatal("geo namespace missing")
	}
	a, b, c := geoKeys(t)
	return db, idx, a, b, c
}

func TestGeoLocationReplacement(t *testing.T) {
	db, idx, a, b, _ := geoDB(t)
	if err := db.Add(index.Entry{ValueID: 100, Geos: []index.GeoValue{{Namespace: 2, Key: a}}}); err != nil {
		t.Fatal(err)
	}
	if err := db.Update(index.Change{
		Before: index.Entry{ValueID: 100, Geos: []index.GeoValue{{Namespace: 2, Key: a}}},
		After:  index.Entry{ValueID: 100, Geos: []index.GeoValue{{Namespace: 2, Key: b}}},
	}); err != nil {
		t.Fatal(err)
	}
	if idx.Contains(a, 100) {
		t.Fatal("stale S2Key A still associated after A->B")
	}
	if !idx.Contains(b, 100) {
		t.Fatal("S2Key B missing after A->B")
	}
}

func TestGeoUnchangedLocationNoWrite(t *testing.T) {
	db, idx, a, _, _ := geoDB(t)
	if err := db.Add(index.Entry{ValueID: 100, Geos: []index.GeoValue{{Namespace: 2, Key: a}}}); err != nil {
		t.Fatal(err)
	}
	gen := idx.Generation()
	if err := db.Update(index.Change{
		Before: index.Entry{ValueID: 100, Geos: []index.GeoValue{{Namespace: 2, Key: a}}},
		After:  index.Entry{ValueID: 100, Geos: []index.GeoValue{{Namespace: 2, Key: a}}},
	}); err != nil {
		t.Fatal(err)
	}
	if idx.Generation() != gen {
		t.Fatalf("unchanged location rewrote the geo engine: gen %d -> %d", gen, idx.Generation())
	}
	if !idx.Contains(a, 100) {
		t.Fatal("unchanged S2Key A lost")
	}
}

// TestGeoMultipleAssociationsSingleLocation documents that geoidx stores exactly
// one location per ImageID per namespace, so the last association wins and no
// earlier key is left behind. (A single ImageID cannot hold [A,B] simultaneously
// in geoidx; multiple geographic associations would require distinct
// namespaces.)
func TestGeoMultipleAssociationsSingleLocation(t *testing.T) {
	db, idx, a, b, c := geoDB(t)
	if err := db.Add(index.Entry{ValueID: 100, Geos: []index.GeoValue{{Namespace: 2, Key: a}, {Namespace: 2, Key: b}}}); err != nil {
		t.Fatal(err)
	}
	if idx.Contains(a, 100) {
		t.Fatal("earlier association A lingered after [A,B]")
	}
	if !idx.Contains(b, 100) {
		t.Fatal("[A,B] did not leave B")
	}
	// A move to C replaces B without leaving it behind.
	if err := db.Update(index.Change{
		Before: index.Entry{ValueID: 100, Geos: []index.GeoValue{{Namespace: 2, Key: b}}},
		After:  index.Entry{ValueID: 100, Geos: []index.GeoValue{{Namespace: 2, Key: c}}},
	}); err != nil {
		t.Fatal(err)
	}
	if idx.Contains(a, 100) || idx.Contains(b, 100) {
		t.Fatal("move to C left a stale association")
	}
	if !idx.Contains(c, 100) {
		t.Fatal("move to C missing C")
	}
}

func TestGeoNamespaceRemoval(t *testing.T) {
	db, idx, a, _, _ := geoDB(t)
	if err := db.Add(index.Entry{ValueID: 100, Geos: []index.GeoValue{{Namespace: 2, Key: a}}}); err != nil {
		t.Fatal(err)
	}
	if err := db.Update(index.Change{
		Before: index.Entry{ValueID: 100, Geos: []index.GeoValue{{Namespace: 2, Key: a}}},
		After:  index.Entry{ValueID: 100},
	}); err != nil {
		t.Fatal(err)
	}
	if idx.Contains(a, 100) {
		t.Fatal("removed namespace still has the S2Key")
	}
}

func TestGeoIndependentNamespaces(t *testing.T) {
	db, idx2, a, b, c := geoDB(t)
	idx3, ok := db.Geo(3)
	if !ok {
		t.Fatal("geo3 missing")
	}
	if err := db.Add(index.Entry{ValueID: 100, Geos: []index.GeoValue{{Namespace: 2, Key: a}, {Namespace: 3, Key: c}}}); err != nil {
		t.Fatal(err)
	}
	gen3 := idx3.Generation()
	if err := db.Update(index.Change{
		Before: index.Entry{ValueID: 100, Geos: []index.GeoValue{{Namespace: 2, Key: a}, {Namespace: 3, Key: c}}},
		After:  index.Entry{ValueID: 100, Geos: []index.GeoValue{{Namespace: 2, Key: b}, {Namespace: 3, Key: c}}},
	}); err != nil {
		t.Fatal(err)
	}
	if idx2.Contains(a, 100) || !idx2.Contains(b, 100) {
		t.Fatal("namespace 2 not replaced correctly")
	}
	if !idx3.Contains(c, 100) || idx3.Generation() != gen3 {
		t.Fatal("namespace 3 was modified by a namespace 2 change")
	}
}

func TestGeoRepeatedUpdates(t *testing.T) {
	db, idx, a, b, c := geoDB(t)
	if err := db.Add(index.Entry{ValueID: 100, Geos: []index.GeoValue{{Namespace: 2, Key: a}}}); err != nil {
		t.Fatal(err)
	}
	for _, mv := range [][2]geoidx.S2Key{{a, b}, {b, c}} {
		if err := db.Update(index.Change{
			Before: index.Entry{ValueID: 100, Geos: []index.GeoValue{{Namespace: 2, Key: mv[0]}}},
			After:  index.Entry{ValueID: 100, Geos: []index.GeoValue{{Namespace: 2, Key: mv[1]}}},
		}); err != nil {
			t.Fatal(err)
		}
	}
	if idx.Contains(a, 100) || idx.Contains(b, 100) || !idx.Contains(c, 100) {
		t.Fatal("A->B->C did not leave only C")
	}
}

func TestGeoUpdatePersistence(t *testing.T) {
	dir := t.TempDir()
	db, err := index.Create(dir, index.Spec{Schema: geoTestSchema(t)})
	if err != nil {
		t.Fatal(err)
	}
	a, b, c := geoKeys(t)
	if err := db.Add(index.Entry{ValueID: 100, Geos: []index.GeoValue{{Namespace: 2, Key: a}}}); err != nil {
		t.Fatal(err)
	}
	if err := db.Update(index.Change{
		Before: index.Entry{ValueID: 100, Geos: []index.GeoValue{{Namespace: 2, Key: a}}},
		After:  index.Entry{ValueID: 100, Geos: []index.GeoValue{{Namespace: 2, Key: b}}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := index.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	idx, ok := reopened.Geo(2)
	if !ok {
		t.Fatal("geo namespace missing after reopen")
	}
	if idx.Contains(a, 100) {
		t.Fatal("stale S2Key A survived reopen")
	}
	if !idx.Contains(b, 100) || idx.Contains(c, 100) {
		t.Fatal("S2Key B not preserved across reopen")
	}
}

func TestGeoUpdateUnifiedSearch(t *testing.T) {
	db, _, a, b, _ := geoDB(t)
	if err := db.Add(index.Entry{ValueID: 100, Geos: []index.GeoValue{{Namespace: 2, Key: a}}}); err != nil {
		t.Fatal(err)
	}
	eng, err := search.New(db)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	ws := search.NewWorkspace(16)
	cellA, err := geoidx.CellIDFromKey(a)
	if err != nil {
		t.Fatal(err)
	}
	cellB, err := geoidx.CellIDFromKey(b)
	if err != nil {
		t.Fatal(err)
	}
	query := func(cell uint64) []schematypes.ValueID {
		t.Helper()
		clause, err := eng.InCells(2, cell)
		if err != nil {
			t.Fatal(err)
		}
		plan, err := eng.Compile(schematypes.EntityImage, clause)
		if err != nil {
			t.Fatal(err)
		}
		got, err := plan.AppendIDs(ctx, nil, ws)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	if got := query(uint64(cellA)); !slices.Contains(got, uint64(100)) {
		t.Fatalf("unified search missed A: %v", got)
	}
	if err := db.Update(index.Change{
		Before: index.Entry{ValueID: 100, Geos: []index.GeoValue{{Namespace: 2, Key: a}}},
		After:  index.Entry{ValueID: 100, Geos: []index.GeoValue{{Namespace: 2, Key: b}}},
	}); err != nil {
		t.Fatal(err)
	}
	if got := query(uint64(cellA)); slices.Contains(got, uint64(100)) {
		t.Fatalf("unified search still returns the stale S2Key A: %v", got)
	}
	if got := query(uint64(cellB)); !slices.Contains(got, uint64(100)) {
		t.Fatalf("unified search missed B after replacement: %v", got)
	}
}

func TestGeoUpdateConcurrentReaders(t *testing.T) {
	db, idx, a, b, _ := geoDB(t)
	if err := db.Add(index.Entry{ValueID: 1, Geos: []index.GeoValue{{Namespace: 2, Key: a}}}); err != nil {
		t.Fatal(err)
	}
	var stop atomic.Bool
	var wg sync.WaitGroup
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for !stop.Load() {
				_ = idx.Contains(a, 1)
				_ = idx.Contains(b, 1)
			}
		}()
	}
	for i := 0; i < 200; i++ {
		from, to := a, b
		if i%2 == 1 {
			from, to = b, a
		}
		if err := db.Update(index.Change{
			Before: index.Entry{ValueID: 1, Geos: []index.GeoValue{{Namespace: 2, Key: from}}},
			After:  index.Entry{ValueID: 1, Geos: []index.GeoValue{{Namespace: 2, Key: to}}},
		}); err != nil {
			t.Fatal(err)
		}
	}
	stop.Store(true)
	wg.Wait()
	if !idx.Contains(a, 1) || idx.Contains(b, 1) {
		t.Fatal("final geo state inconsistent after concurrent updates")
	}
}

// BenchmarkStoreGeoReplacement measures a same-namespace location move through
// the unified Update path.
func BenchmarkStoreGeoReplacement(b *testing.B) {
	db, _, a, c, _ := geoDB(b)
	if err := db.Add(index.Entry{ValueID: 1, Geos: []index.GeoValue{{Namespace: 2, Key: a}}}); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		from, to := a, c
		if i%2 == 1 {
			from, to = c, a
		}
		if err := db.Update(index.Change{
			Before: index.Entry{ValueID: 1, Geos: []index.GeoValue{{Namespace: 2, Key: from}}},
			After:  index.Entry{ValueID: 1, Geos: []index.GeoValue{{Namespace: 2, Key: to}}},
		}); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkStoreGeoUnchanged measures an Update that re-supplies the same
// location, which geoidx treats as an idempotent insert.
func BenchmarkStoreGeoUnchanged(b *testing.B) {
	db, _, a, _, _ := geoDB(b)
	if err := db.Add(index.Entry{ValueID: 1, Geos: []index.GeoValue{{Namespace: 2, Key: a}}}); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := db.Update(index.Change{
			Before: index.Entry{ValueID: 1, Geos: []index.GeoValue{{Namespace: 2, Key: a}}},
			After:  index.Entry{ValueID: 1, Geos: []index.GeoValue{{Namespace: 2, Key: a}}},
		}); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkGeoEngineDirectReplacement isolates the geoidx write cost of
// alternating a location on one ImageID.
func BenchmarkGeoEngineDirectReplacement(b *testing.B) {
	_, idx, a, c, _ := geoDB(b)
	if err := idx.Insert(a, schematypes.ValueID(1)); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		key := a
		if i%2 == 1 {
			key = c
		}
		if err := idx.Insert(key, schematypes.ValueID(1)); err != nil {
			b.Fatal(err)
		}
	}
}
