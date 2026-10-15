package database

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/cockroachdb/pebble/v2"
	"github.com/evanoberholster/imageIdx/schema"
	"github.com/evanoberholster/imageIdx/store"
)

func addTarget(t testing.TB, data string) *schema.Definitions {
	t.Helper()
	s, err := schema.Open(filepath.Join(data, "schema.dat"), schema.Options{})
	if err != nil {
		t.Fatal(err)
	}
	target := s.Definitions()
	if prepared := s.PreparedDefinitions(); prepared != nil {
		target = prepared
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, ok := target.Lookup(501); !ok {
		if err := target.Register(schema.NamespaceDef{Namespace: 501, Name: "extra-kv", Kind: schema.KindKV, Entity: schema.EntityImage}); err != nil {
			t.Fatal(err)
		}
		if err := target.Register(schema.NamespaceDef{Namespace: 502, Name: "extra-time", Kind: schema.KindTimestamp, Entity: schema.EntityImage}); err != nil {
			t.Fatal(err)
		}
	}
	return target
}

func TestSchemaAdditionCrashChild(t *testing.T) {
	phase := os.Getenv("IMAGEIDX_SCHEMA_PHASE")
	if phase == "" {
		return
	}
	root := os.Getenv("IMAGEIDX_SCHEMA_ROOT")
	data, indexDir := filepath.Join(root, "data"), filepath.Join(root, "index")
	var definitions *schema.Definitions
	if os.Getenv("IMAGEIDX_SCHEMA_TARGET") == "1" {
		definitions = addTarget(t, data)
	}
	_, err := Open(data, indexDir, decodeTest, Options{Definitions: definitions, boundary: func(at string) {
		if at == phase {
			os.Exit(74)
		}
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Fatalf("schema crash boundary %q not reached", phase)
}

func crashSchemaOpen(t *testing.T, root, phase string, supplyTarget bool) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestSchemaAdditionCrashChild$")
	target := "0"
	if supplyTarget {
		target = "1"
	}
	cmd.Env = append(os.Environ(), "IMAGEIDX_SCHEMA_PHASE="+phase, "IMAGEIDX_SCHEMA_ROOT="+root, "IMAGEIDX_SCHEMA_TARGET="+target)
	var exit *exec.ExitError
	if err := cmd.Run(); !errors.As(err, &exit) || exit.ExitCode() != 74 {
		t.Fatalf("%s child exit: %v", phase, err)
	}
}

func TestSchemaAdditionCrashBoundaries(t *testing.T) {
	for _, phase := range []string{
		"before schema prepare", "after schema prepare", "before schema index initialization",
		"during rebuild scan", "after rebuild batch", "before rebuild verify",
		"after rebuild verified", "after old index rename", "after replacement rename",
		"after schema index initialization", "before schema activation", "after schema activation",
	} {
		t.Run(phase, func(t *testing.T) {
			data, indexDir := paths(t)
			db := openTest(t, data, indexDir)
			g, err := db.Commit(Mutation{Entity: schema.EntityImage, ValueID: 1, Record: []byte("kept"), Projection: projection(1, 'x')})
			if err != nil {
				t.Fatal(err)
			}
			wait(t, db, g)
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			root := filepath.Dir(data)
			crashSchemaOpen(t, root, phase, true)
			if phase == "before schema index initialization" || phase == "after schema index initialization" {
				crashSchemaOpen(t, root, phase, false) // repeat crash during recovery
			}
			options := Options{}
			if phase == "before schema prepare" {
				options.Definitions = addTarget(t, data)
			}
			db, err = Open(data, indexDir, decodeTest, options)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if db.ids.PreparedDefinitions() != nil {
				t.Fatal("schema still prepared")
			}
			if _, ok := db.ids.Lookup(501); !ok {
				t.Fatal("new KV missing")
			}
			if _, ok := db.ids.Lookup(502); !ok {
				t.Fatal("new index missing")
			}
			if got, err := db.Get(schema.EntityImage, 1); err != nil || string(got) != "kept" {
				t.Fatalf("canonical: %q %v", got, err)
			}
			contains(t, db, 1, 'x', true)
			if id, err := db.NextID(schema.EntityImage); id <= 1 || err != nil {
				t.Fatalf("allocator reused ID: %d %v", id, err)
			}
		})
	}
}

func TestLegacyCleanupCrashChild(t *testing.T) {
	phase := os.Getenv("IMAGEIDX_LEGACY_PHASE")
	if phase == "" {
		return
	}
	root := os.Getenv("IMAGEIDX_LEGACY_ROOT")
	_, err := Open(filepath.Join(root, "data"), filepath.Join(root, "index"), decodeTest, Options{boundary: func(at string) {
		if at == phase {
			os.Exit(74)
		}
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Fatalf("legacy cleanup boundary %q not reached", phase)
}

func TestLegacySchemaCleanupCrashBoundaries(t *testing.T) {
	for _, phase := range []string{"after legacy Pebble schema cleanup", "after legacy index backup cleanup", "after legacy index schema cleanup", "after legacy retirement sync"} {
		t.Run(phase, func(t *testing.T) {
			data, indexDir := paths(t)
			legacy, err := store.OpenLegacy(data, canonicalSpecs()...)
			if err != nil {
				t.Fatal(err)
			}
			if err := legacy.Close(); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(os.Args[0], "-test.run=^TestLegacyCleanupCrashChild$")
			cmd.Env = append(os.Environ(), "IMAGEIDX_LEGACY_PHASE="+phase, "IMAGEIDX_LEGACY_ROOT="+filepath.Dir(data))
			var exit *exec.ExitError
			if err := cmd.Run(); !errors.As(err, &exit) || exit.ExitCode() != 74 {
				t.Fatalf("child: %v", err)
			}
			for range 2 {
				db, err := Open(data, indexDir, decodeTest, Options{})
				if err != nil {
					t.Fatal(err)
				}
				if err := db.Close(); err != nil {
					t.Fatal(err)
				}
			}
			pdb, err := pebble.Open(data, &pebble.Options{ReadOnly: true})
			if err != nil {
				t.Fatal(err)
			}
			key := store.EncodeKey(store.Key{})
			if value, closer, err := pdb.Get(key[:]); !errors.Is(err, pebble.ErrNotFound) {
				if closer != nil {
					_ = closer.Close()
				}
				t.Fatalf("legacy Pebble schema remains: %x %v", value, err)
			}
			if err := pdb.Close(); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(filepath.Join(indexDir, "schema.bin")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("index schema remains: %v", err)
			}
		})
	}
}

func TestAllocatorHighWaterAcrossInterruptedActivation(t *testing.T) {
	entities := []schema.EntityKind{schema.EntityImage, schema.EntityFace, schema.EntityPerson}
	for _, phase := range []string{"before schema index initialization", "after schema activation"} {
		t.Run(phase, func(t *testing.T) {
			data, indexDir := paths(t)
			db := openTest(t, data, indexDir)
			last := map[schema.EntityKind]schema.ValueID{}
			for _, e := range entities {
				for i := 0; i < int(e)+2; i++ {
					id, err := db.NextID(e)
					if err != nil {
						t.Fatal(err)
					}
					last[e] = id
				}
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			crashSchemaOpen(t, filepath.Dir(data), phase, true)
			db, err := Open(data, indexDir, decodeTest, Options{})
			if err != nil {
				t.Fatal(err)
			}
			if db.ids.PreparedDefinitions() != nil {
				t.Fatal("interrupted activation did not complete")
			}
			// An unclean allocator checkpoint skips the unused tail of a
			// reserved block, so only monotonicity and non-reuse are promised.
			for _, e := range entities {
				id, err := db.NextID(e)
				if err != nil {
					t.Fatal(err)
				}
				if id <= last[e] {
					t.Fatalf("%s reused acknowledged ID: got %d, last issued %d", e, id, last[e])
				}
				last[e] = id
			}
			// A further additive restart must not rewind any entity.
			target := db.ids.Definitions()
			if err := target.Register(schema.NamespaceDef{Namespace: 640, Name: "recovery-kv", Kind: schema.KindKV, Entity: schema.EntityImage}); err != nil {
				t.Fatal(err)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			db, err = Open(data, indexDir, decodeTest, Options{Definitions: target})
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			for _, e := range entities {
				id, err := db.NextID(e)
				if err != nil {
					t.Fatal(err)
				}
				if id <= last[e] {
					t.Fatalf("%s rewound across additive restart: got %d, previous %d", e, id, last[e])
				}
			}
		})
	}
}
