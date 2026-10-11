package index

import (
	"os"
	"path/filepath"
	"testing"

	common "github.com/evanoberholster/imageIdx/common"
	"github.com/evanoberholster/imageIdx/index/bitmapidx"
	"github.com/evanoberholster/imageIdx/index/geoidx"
	"github.com/evanoberholster/imageIdx/index/radixhash"
	"github.com/evanoberholster/imageIdx/index/simhash"
	"github.com/evanoberholster/imageIdx/index/vectoridx"
)

// boundarySchema has one namespace per engine family, all EntityImage.
func boundarySchema(t *testing.T) *common.Schema {
	t.Helper()
	schema := common.NewSchema()
	defs := []common.NamespaceDef{
		{Namespace: 1, Name: "time", Kind: common.KindTimestamp, Entity: common.EntityImage},
		{Namespace: 2, Name: "geo", Kind: common.KindGeo, Entity: common.EntityImage},
		{Namespace: 3, Name: "sim", Kind: common.KindHashPerceptual, Entity: common.EntityImage},
		{Namespace: 4, Name: "sha", Kind: common.KindHashExact, Entity: common.EntityImage},
	}
	if err := schema.RegisterAll(defs...); err != nil {
		t.Fatal(err)
	}
	tags, err := BitmapDef(bitmapidx.NamespaceSpec{Namespace: 6, Kind: common.KindBitmapEnum}, "tags", common.EntityImage)
	if err != nil {
		t.Fatal(err)
	}
	if err := schema.Register(tags); err != nil {
		t.Fatal(err)
	}
	vec, err := VectorDef(vectoridx.Config{Namespace: 5, Dimensions: vectoridx.Dim512, ElementType: vectoridx.ElementFloat32}, "vec", common.EntityImage)
	if err != nil {
		t.Fatal(err)
	}
	if err := schema.Register(vec); err != nil {
		t.Fatal(err)
	}
	return schema
}

// TestSingleNamespaceLivenessFailureIsBenign verifies that a single-namespace
// mutation whose internal liveness publish is interrupted cannot expose a
// harmful partial state, so no recovery marker is required. The timestamp value
// commits durably, the liveness commit fails, and the entity is therefore not
// live (not searchable).
func TestSingleNamespaceLivenessFailureIsBenign(t *testing.T) {
	dir := t.TempDir()
	db, err := Create(dir, Spec{Schema: boundarySchema(t)})
	if err != nil {
		t.Fatal(err)
	}
	// Force the liveness publish to fail after the value engine commits.
	if err := db.bitmapStore.Close(); err != nil {
		t.Fatal(err)
	}
	if err := db.Add(Entry{ValueID: 1, Times: []TimeValue{{Namespace: 1, Timestamp: 5}}}); err == nil {
		t.Fatal("expected the liveness commit to fail")
	}
	if _, err := os.Stat(filepath.Join(dir, "recovery.bin")); !os.IsNotExist(err) {
		t.Fatalf("recovery marker written for a single-namespace + liveness batch: %v", err)
	}
	_ = db.Close()

	reopened, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	live, val, ok := reopened.Liveness(common.EntityImage)
	if !ok {
		t.Fatal("liveness namespace unavailable after reopen")
	}
	bm, _ := live.MatchValues(val)
	if bm.Contains(1) {
		t.Fatal("interrupted single-namespace batch left the entity live")
	}
}

// TestEnginePersistenceBoundaries documents, deterministically, which engines'
// single-namespace mutations survive a close/reopen without an explicit
// Checkpoint. The recovery-marker classification depends on this.
func TestEnginePersistenceBoundaries(t *testing.T) {
	cases := []struct {
		name  string
		apply func(t *testing.T, db *Store)
		check func(t *testing.T, db *Store) bool
	}{
		{
			name: "time",
			apply: func(t *testing.T, db *Store) {
				if err := db.Add(Entry{ValueID: 1, Times: []TimeValue{{Namespace: 1, Timestamp: 5}}}); err != nil {
					t.Fatal(err)
				}
			},
			check: func(t *testing.T, db *Store) bool {
				idx, ok := db.Time(1)
				return ok && len(idx.Search(nil, 4, 6)) == 1
			},
		},
		{
			name: "bitmap",
			apply: func(t *testing.T, db *Store) {
				if err := db.Add(Entry{ValueID: 1, Bitmaps: []BitmapValue{{Namespace: 6, Value: bitmapidx.EnumStringValue("x")}}}); err != nil {
					t.Fatal(err)
				}
			},
			check: func(t *testing.T, db *Store) bool {
				idx, ok := db.Bitmap(6)
				if !ok {
					return false
				}
				bm, _ := idx.MatchValues(bitmapidx.EnumStringValue("x"))
				return bm.Contains(1)
			},
		},
		{
			name: "geo",
			apply: func(t *testing.T, db *Store) {
				key, err := geoidx.KeyFromLatLng(1, 1)
				if err != nil {
					t.Fatal(err)
				}
				if err := db.Add(Entry{ValueID: 1, Geos: []GeoValue{{Namespace: 2, Key: key}}}); err != nil {
					t.Fatal(err)
				}
			},
			check: func(t *testing.T, db *Store) bool {
				idx, ok := db.Geo(2)
				return ok && idx.Len() == 1
			},
		},
		{
			name: "radixhash",
			apply: func(t *testing.T, db *Store) {
				if err := db.Add(Entry{ValueID: 1, Hashes: []HashValue{{Namespace: 4, Hash: radixhash.Hash{9}}}}); err != nil {
					t.Fatal(err)
				}
			},
			check: func(t *testing.T, db *Store) bool {
				idx, ok := db.Hash(4)
				return ok && len(idx.Lookup(nil, radixhash.Hash{9})) == 1
			},
		},
		{
			name: "simhash-append",
			apply: func(t *testing.T, db *Store) {
				if err := db.Add(Entry{ValueID: 1, Sims: []SimValue{{Namespace: 3, Record: simhash.Record{PHash: 0}}}}); err != nil {
					t.Fatal(err)
				}
			},
			check: func(t *testing.T, db *Store) bool {
				st, ok := db.Sim(3)
				return ok && st.Len() == 1
			},
		},
		{
			name: "vector",
			apply: func(t *testing.T, db *Store) {
				vec := make([]float32, 512)
				vec[0] = 1
				if err := db.Add(Entry{ValueID: 1, Vectors: []VectorValue{{Namespace: 5, Element: vectoridx.ElementFloat32, Float32: vec}}}); err != nil {
					t.Fatal(err)
				}
			},
			check: func(t *testing.T, db *Store) bool {
				v, ok := db.Vector(5)
				if !ok {
					return false
				}
				r, _ := v.SearchFloat32(make([]float32, 512), nil, 1)
				return len(r) == 1 && r[0].ValueID == 1
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name+"/no-checkpoint", func(t *testing.T) {
			dir := t.TempDir()
			db, err := Create(dir, Spec{Schema: boundarySchema(t)})
			if err != nil {
				t.Fatal(err)
			}
			tc.apply(t, db)
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			t.Logf("%s survived close/reopen without Checkpoint: %v", tc.name, tc.check(t, reopened))
		})
		t.Run(tc.name+"/checkpoint", func(t *testing.T) {
			dir := t.TempDir()
			db, err := Create(dir, Spec{Schema: boundarySchema(t)})
			if err != nil {
				t.Fatal(err)
			}
			tc.apply(t, db)
			if err := db.Checkpoint(); err != nil {
				t.Fatal(err)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			if !tc.check(t, reopened) {
				t.Fatalf("%s did not survive after Checkpoint", tc.name)
			}
		})
	}
}
