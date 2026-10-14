package index

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"

	schematypes "github.com/evanoberholster/imageIdx/schema"
	"github.com/evanoberholster/imageIdx/index/bitmapidx"
	"github.com/evanoberholster/imageIdx/index/geoidx"
	"github.com/evanoberholster/imageIdx/index/radixhash"
	"github.com/evanoberholster/imageIdx/index/simhash"
	"github.com/evanoberholster/imageIdx/index/vectoridx"
)

// boundarySchema has one namespace per engine family, all EntityImage.
func boundarySchema(t *testing.T) *schematypes.Definitions {
	t.Helper()
	schema := schematypes.NewDefinitions()
	defs := []schematypes.NamespaceDef{
		{Namespace: 1, Name: "time", Kind: schematypes.KindTimestamp, Entity: schematypes.EntityImage},
		{Namespace: 2, Name: "geo", Kind: schematypes.KindGeo, Entity: schematypes.EntityImage},
		{Namespace: 3, Name: "sim", Kind: schematypes.KindHashPerceptual, Entity: schematypes.EntityImage},
		{Namespace: 4, Name: "sha", Kind: schematypes.KindHashExact, Entity: schematypes.EntityImage},
	}
	if err := schema.RegisterAll(defs...); err != nil {
		t.Fatal(err)
	}
	tags, err := BitmapDef(bitmapidx.NamespaceSpec{Namespace: 6, Kind: schematypes.KindBitmapEnum}, "tags", schematypes.EntityImage)
	if err != nil {
		t.Fatal(err)
	}
	if err := schema.Register(tags); err != nil {
		t.Fatal(err)
	}
	vec, err := VectorDef(vectoridx.Config{Namespace: 5, Dimensions: vectoridx.Dim512, ElementType: vectoridx.ElementFloat32}, "vec", schematypes.EntityImage)
	if err != nil {
		t.Fatal(err)
	}
	if err := schema.Register(vec); err != nil {
		t.Fatal(err)
	}
	return schema
}

// TestUnifiedProcessCrashRecovery kills the process at each staged-flush
// boundary of an update to an already-live entity and verifies the recovery
// classification on reopen: a single-unit (time-only) update leaves no marker
// and stays consistent and live, while a multi-engine (time+hash) update is
// flagged rebuild-required because radixhash is not crash-durable.
func TestUnifiedProcessCrashRecovery(t *testing.T) {
	if dir := os.Getenv("UNIFIED_CRASH_DIR"); dir != "" {
		stage := os.Getenv("UNIFIED_CRASH_STAGE")
		scenario := os.Getenv("UNIFIED_CRASH_SCENARIO")
		db, err := Open(dir)
		if err != nil {
			t.Fatal(err)
		}
		db.crash = func(s string) {
			if s == stage {
				os.Exit(83)
			}
		}
		batch := db.NewBatch(1)
		switch scenario {
		case "time-update":
			if err := batch.Add(Entry{ValueID: 1, Times: []TimeValue{{Namespace: 1, Timestamp: 9}}}); err != nil {
				t.Fatal(err)
			}
		case "time-hash-update":
			if err := batch.Add(Entry{ValueID: 1, Times: []TimeValue{{Namespace: 1, Timestamp: 9}}, Hashes: []HashValue{{Namespace: 4, Hash: radixhash.Hash{0xB}}}}); err != nil {
				t.Fatal(err)
			}
		case "hash-update":
			if err := batch.Add(Entry{ValueID: 1, Hashes: []HashValue{{Namespace: 4, Hash: radixhash.Hash{0xB}}}}); err != nil {
				t.Fatal(err)
			}
		default:
			t.Fatalf("unknown scenario %q", scenario)
		}
		_ = db.Apply(batch)
		os.Exit(0)
	}

	scenarios := []struct {
		name     string
		scenario string
		seed     func(t *testing.T, db *Store)
	}{
		{
			name:     "time-update",
			scenario: "time-update",
			seed: func(t *testing.T, db *Store) {
				if err := db.Add(Entry{ValueID: 1, Times: []TimeValue{{Namespace: 1, Timestamp: 5}}}); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name:     "time-hash-update",
			scenario: "time-hash-update",
			seed: func(t *testing.T, db *Store) {
				if err := db.Add(Entry{ValueID: 1, Times: []TimeValue{{Namespace: 1, Timestamp: 5}}, Hashes: []HashValue{{Namespace: 4, Hash: radixhash.Hash{0xA}}}}); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name:     "hash-update",
			scenario: "hash-update",
			seed: func(t *testing.T, db *Store) {
				if err := db.Add(Entry{ValueID: 1, Hashes: []HashValue{{Namespace: 4, Hash: radixhash.Hash{0xA}}}}); err != nil {
					t.Fatal(err)
				}
			},
		},
	}
	stages := []string{"start", "liveness-add-pre", "time", "hash", "liveness-add"}
	for _, sc := range scenarios {
		for _, stage := range stages {
			t.Run(sc.name+"/"+stage, func(t *testing.T) {
				dir := t.TempDir()
				db, err := Create(dir, Spec{Schema: boundarySchema(t)})
				if err != nil {
					t.Fatal(err)
				}
				sc.seed(t, db)
				if err := db.Checkpoint(); err != nil {
					t.Fatal(err)
				}
				if err := db.Close(); err != nil {
					t.Fatal(err)
				}

				cmd := exec.Command(os.Args[0], "-test.run=^TestUnifiedProcessCrashRecovery$")
				cmd.Env = append(os.Environ(),
					"UNIFIED_CRASH_DIR="+dir,
					"UNIFIED_CRASH_STAGE="+stage,
					"UNIFIED_CRASH_SCENARIO="+sc.scenario)
				out, err := cmd.CombinedOutput()
				if err == nil {
					t.Fatalf("child did not crash at %s: %s", stage, out)
				}

				reopened, err := Open(dir)
				if err != nil {
					t.Fatalf("reopen: %v: %s", err, out)
				}
				defer reopened.Close()
				_, statErr := os.Stat(filepath.Join(dir, "recovery.bin"))
				marker := statErr == nil

				switch sc.name {
				case "time-update":
					if marker {
						t.Fatal("single-unit update wrote a recovery marker")
					}
					ti, ok := reopened.Time(1)
					if !ok {
						t.Fatal("time namespace unavailable after interrupt")
					}
					if got := ti.Search(nil, 0, 100); !slices.Equal(got, []uint64{1}) {
						t.Fatalf("time search = %v, want [1]", got)
					}
					live, val, ok := reopened.Liveness(schematypes.EntityImage)
					if !ok {
						t.Fatal("liveness unavailable")
					}
					if bm, _ := live.MatchValues(val); !bm.Contains(1) {
						t.Fatal("already-live entity was not preserved")
					}
				case "time-hash-update":
					if !marker {
						t.Fatal("interrupted multi-engine update left no recovery marker")
					}
					rec := reopened.RecoveryNamespaces()
					slices.Sort(rec)
					if !slices.Contains(rec, schematypes.Namespace(1)) || !slices.Contains(rec, schematypes.Namespace(4)) {
						t.Fatalf("recovery namespaces = %v, want [1 4]", rec)
					}
					if _, ok := reopened.Time(1); ok {
						t.Fatal("flagged time namespace is resolvable")
					}
					if _, ok := reopened.Hash(4); ok {
						t.Fatal("flagged hash namespace is resolvable")
					}
					if _, ok := reopened.Geo(2); !ok {
						t.Fatal("unrelated namespace became unavailable")
					}
				case "hash-update":
					// Single checkpoint-dependent unit: no marker, the namespace
					// stays resolvable, and the un-checkpointed update is simply
					// lost, leaving the durable "before" association.
					if marker {
						t.Fatal("single-unit hash update wrote a recovery marker")
					}
					idx, ok := reopened.Hash(4)
					if !ok {
						t.Fatal("hash namespace unavailable after interrupt")
					}
					if got := idx.Lookup(nil, radixhash.Hash{0xA}); !slices.Contains(got, uint64(1)) {
						t.Fatalf("durable hash A lost: %v", got)
					}
					if got := idx.Lookup(nil, radixhash.Hash{0xB}); len(got) != 0 {
						t.Fatalf("non-durable hash B present: %v", got)
					}
				}
			})
		}
	}
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
	live, val, ok := reopened.Liveness(schematypes.EntityImage)
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
