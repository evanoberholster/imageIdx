package database

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	schema "github.com/evanoberholster/imageIdx/schema"
)

func changedProjectionDefinitions(t *testing.T, source *schema.Definitions, namespace schema.Namespace, version uint16) *schema.Definitions {
	t.Helper()
	target := schema.NewDefinitions()
	for _, def := range source.Defs() {
		if def.Namespace == namespace {
			def.ProjectionVersion = version
		}
		if err := target.Register(def); err != nil {
			t.Fatal(err)
		}
	}
	return target
}

func TestProjectionVersionMigrationRebuilds(t *testing.T) {
	data, idxPath := paths(t)
	project := func(record []byte) ([]byte, error) { return record, nil }
	db, err := Open(data, idxPath, decodeTest, Options{
		CanonicalProjection: CanonicalProjectionExternal,
		ProjectFromRecord:   project,
	})
	if err != nil {
		t.Fatal(err)
	}
	projection := projection(1, 'a')
	if _, err := db.Commit(Mutation{
		Entity:     schema.EntityImage,
		ValueID:    1,
		Projection: projection,
		Record:     projection,
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.WaitIndexed(t.Context(), 1); err != nil {
		t.Fatal(err)
	}
	target := changedProjectionDefinitions(t, db.ids.Definitions(), tagNS, 2)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	if _, err := Open(data, idxPath, decodeTest, Options{Definitions: target}); err == nil {
		t.Fatal("projection version change without migration option was accepted")
	}
	db, err = Open(data, idxPath, decodeTest, Options{
		Definitions:         target,
		ProjectionMigration: true,
		ProjectFromRecord:   project,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.WaitIndexed(t.Context(), 1); err != nil {
		db.Close()
		t.Fatal(err)
	}
	contains(t, db, 1, 'a', true)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	db, err = Open(data, idxPath, decodeTest, Options{Definitions: target, ProjectFromRecord: project})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	contains(t, db, 1, 'a', true)
}

func TestProjectionVersionDowngradeRejected(t *testing.T) {
	active := schema.NewDefinitions()
	if err := active.Register(schema.NamespaceDef{Namespace: 9, Name: "kv", Kind: schema.KindKV, Entity: schema.EntityImage, ProjectionVersion: 2}); err != nil {
		t.Fatal(err)
	}
	target := schema.NewDefinitions()
	if err := target.Register(schema.NamespaceDef{Namespace: 9, Name: "kv", Kind: schema.KindKV, Entity: schema.EntityImage, ProjectionVersion: 1}); err != nil {
		t.Fatal(err)
	}
	if err := validateAddition(active, target, true); err == nil {
		t.Fatal("projection downgrade accepted")
	}
}

func TestProjectionMigrationRequiresExternalModeAndValidProjector(t *testing.T) {
	data, idxPath := paths(t)
	db := openTest(t, data, idxPath)
	projection := projection(1, 'a')
	if _, err := db.Commit(Mutation{Entity: schema.EntityImage, ValueID: 1, Projection: projection, Record: projection}); err != nil {
		t.Fatal(err)
	}
	if err := db.WaitIndexed(t.Context(), 1); err != nil {
		t.Fatal(err)
	}
	target := changedProjectionDefinitions(t, db.ids.Definitions(), tagNS, 2)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(data, idxPath, decodeTest, Options{
		Definitions:         target,
		ProjectionMigration: true,
		ProjectFromRecord:   func(record []byte) ([]byte, error) { return record, nil },
	}); err == nil {
		t.Fatal("inline projection migration was accepted")
	}
}

func TestProjectionMigrationCrashChild(t *testing.T) {
	phase := os.Getenv("IMAGEIDX_PROJECTION_PHASE")
	if phase == "" {
		return
	}
	root := os.Getenv("IMAGEIDX_PROJECTION_ROOT")
	data, idxPath := filepath.Join(root, "data"), filepath.Join(root, "index")
	s, err := schema.Open(filepath.Join(data, "schema.dat"), schema.Options{})
	if err != nil {
		t.Fatal(err)
	}
	target := changedProjectionDefinitions(t, s.Definitions(), tagNS, 2)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	_, err = Open(data, idxPath, decodeTest, Options{
		Definitions:         target,
		ProjectionMigration: true,
		ProjectFromRecord:   func(record []byte) ([]byte, error) { return record, nil },
		boundary: func(at string) {
			if at == phase {
				os.Exit(76)
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Fatalf("projection crash boundary %q not reached", phase)
}

func TestProjectionMigrationCrashBoundaries(t *testing.T) {
	for _, phase := range []string{
		"before schema prepare", "after schema prepare",
		"before schema activation", "after schema activation",
	} {
		t.Run(phase, func(t *testing.T) {
			data, idxPath := paths(t)
			project := func(record []byte) ([]byte, error) { return record, nil }
			db, err := Open(data, idxPath, decodeTest, Options{
				CanonicalProjection: CanonicalProjectionExternal,
				ProjectFromRecord:   project,
			})
			if err != nil {
				t.Fatal(err)
			}
			firstProjection := projection(1, 'a')
			if _, err := db.Commit(Mutation{Entity: schema.EntityImage, ValueID: 1, Projection: firstProjection, Record: firstProjection}); err != nil {
				t.Fatal(err)
			}
			if err := db.WaitIndexed(t.Context(), 1); err != nil {
				t.Fatal(err)
			}
			target := changedProjectionDefinitions(t, db.ids.Definitions(), tagNS, 2)
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			root := filepath.Dir(data)
			cmd := exec.Command(os.Args[0], "-test.run=^TestProjectionMigrationCrashChild$")
			cmd.Env = append(os.Environ(), "IMAGEIDX_PROJECTION_PHASE="+phase, "IMAGEIDX_PROJECTION_ROOT="+root)
			var exit *exec.ExitError
			if err := cmd.Run(); !errors.As(err, &exit) || exit.ExitCode() != 76 {
				t.Fatalf("%s child exit: %v", phase, err)
			}
			db, err = Open(data, idxPath, decodeTest, Options{ProjectFromRecord: project})
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			contains(t, db, 1, 'a', true)
			// Complete or observe the target version explicitly.
			if _, err := db.Commit(Mutation{Entity: schema.EntityImage, ValueID: 1, Projection: projection(1, 'b'), Record: projection(1, 'b')}); err != nil {
				t.Fatal(err)
			}
			if err := db.WaitIndexed(t.Context(), 2); err != nil {
				t.Fatal(err)
			}
			_ = target
		})
	}
}

func TestProjectionMigrationRejectsSimultaneousConfigChange(t *testing.T) {
	active := schema.NewDefinitions()
	if err := active.Register(schema.NamespaceDef{
		Namespace: 9, Name: "kv", Kind: schema.KindKV, Entity: schema.EntityImage,
		ProjectionVersion: 1, Config: []byte("old"),
	}); err != nil {
		t.Fatal(err)
	}
	target := schema.NewDefinitions()
	if err := target.Register(schema.NamespaceDef{
		Namespace: 9, Name: "kv", Kind: schema.KindKV, Entity: schema.EntityImage,
		ProjectionVersion: 2, Config: []byte("new"),
	}); err != nil {
		t.Fatal(err)
	}
	if err := validateAddition(active, target, true); err == nil {
		t.Fatal("simultaneous projection bump and config change was accepted")
	}
	// The same bypass must fail when the target also appends a namespace.
	if err := target.Register(schema.NamespaceDef{
		Namespace: 10, Name: "extra", Kind: schema.KindKV, Entity: schema.EntityImage,
	}); err != nil {
		t.Fatal(err)
	}
	if err := validateAddition(active, target, true); err == nil {
		t.Fatal("simultaneous config bypass with namespace addition was accepted")
	}
}
