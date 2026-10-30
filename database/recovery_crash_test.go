package database

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/cockroachdb/pebble/v2"
	schema "github.com/evanoberholster/imageIdx/schema"
	"github.com/evanoberholster/imageIdx/store"
)

func TestRebuildCrashChild(t *testing.T) {
	phase := os.Getenv("IMAGEIDX_REBUILD_PHASE")
	if phase == "" {
		return
	}
	root := os.Getenv("IMAGEIDX_REBUILD_ROOT")
	_, err := OpenDirs(filepath.Join(root, "data"), filepath.Join(root, "index"), decodeTest, Options{
		Recovery: RecoveryAutomatic,
		boundary: func(at string) {
			if at == phase {
				os.Exit(74)
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Fatal("rebuild crash boundary was not reached")
}

func TestCrashDuringStartupRebuild(t *testing.T) {
	for _, phase := range []string{
		"before rebuild", "during rebuild scan", "after rebuild batch",
		"before rebuild verify", "after rebuild verified", "after old index rename",
		"after replacement rename", "during old index cleanup",
	} {
		t.Run(phase, func(t *testing.T) {
			data, dir := paths(t)
			db := openTest(t, data, dir)
			g, err := db.Commit(Mutation{Entity: schema.EntityImage, ValueID: 1, Record: []byte("kept"), Projection: projection(1, 'x')})
			if err != nil {
				t.Fatal(err)
			}
			wait(t, db, g)
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(filepath.Join(dir, "manifest")); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(os.Args[0], "-test.run=^TestRebuildCrashChild$")
			cmd.Env = append(os.Environ(), "IMAGEIDX_REBUILD_PHASE="+phase, "IMAGEIDX_REBUILD_ROOT="+filepath.Dir(data))
			err = cmd.Run()
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != 74 {
				t.Fatalf("subprocess: %v", err)
			}
			db, err = OpenDirs(data, dir, decodeTest, Options{Recovery: RecoveryAutomatic})
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if got, err := db.Get(schema.EntityImage, 1); err != nil || string(got) != "kept" {
				t.Fatalf("canonical: %q %v", got, err)
			}
			contains(t, db, 1, 'x', true)
			if db.Index().SourceGeneration() != g {
				t.Fatalf("rebuild generation=%d, want %d", db.Index().SourceGeneration(), g)
			}
		})
	}
}

func TestCorruptActivationRecordDoesNotPublish(t *testing.T) {
	data, dir := paths(t)
	db := openTest(t, data, dir)
	g, err := db.Commit(Mutation{Entity: schema.EntityImage, ValueID: 1, Projection: projection(1, 'a')})
	if err != nil {
		t.Fatal(err)
	}
	wait(t, db, g)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "manifest")); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestRebuildCrashChild$")
	cmd.Env = append(os.Environ(), "IMAGEIDX_REBUILD_PHASE=after rebuild verified", "IMAGEIDX_REBUILD_ROOT="+filepath.Dir(data))
	var exit *exec.ExitError
	if err := cmd.Run(); !errors.As(err, &exit) || exit.ExitCode() != 74 {
		t.Fatalf("child: %v", err)
	}
	path := activationPath(dir)
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	corrupt := append([]byte(nil), original...)
	corrupt[len(corrupt)-1] ^= 1
	if err := os.WriteFile(path, corrupt, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenDirs(data, dir, decodeTest, Options{Recovery: RecoveryAutomatic}); !errors.Is(err, ErrRecoveryRequired) {
		t.Fatalf("corrupt activation accepted: %v", err)
	}
	if _, err := os.Stat(stagePath(dir)); err != nil {
		t.Fatalf("verified stage discarded: %v", err)
	}
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	db, err = OpenDirs(data, dir, decodeTest, Options{Recovery: RecoveryAutomatic})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	contains(t, db, 1, 'a', true)
}

func TestResumeIndexActivationUsesAuthoritativeSchema(t *testing.T) {
	data, dir := paths(t)
	db := openTest(t, data, dir)
	g, err := db.Commit(Mutation{Entity: schema.EntityImage, ValueID: 1, Projection: projection(1, 'a')})
	if err != nil {
		t.Fatal(err)
	}
	wait(t, db, g)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "manifest")); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestRebuildCrashChild$")
	cmd.Env = append(os.Environ(), "IMAGEIDX_REBUILD_PHASE=after rebuild verified", "IMAGEIDX_REBUILD_ROOT="+filepath.Dir(data))
	var exit *exec.ExitError
	if err := cmd.Run(); !errors.As(err, &exit) || exit.ExitCode() != 74 {
		t.Fatalf("child: %v", err)
	}
	pdb, err := pebble.Open(data, nil)
	if err != nil {
		t.Fatal(err)
	}
	key := store.EncodeKey(store.Key{Namespace: metadataNS, ID: schemaKeyID})
	if err := pdb.Delete(key[:], pebble.Sync); err != nil {
		_ = pdb.Close()
		t.Fatal(err)
	}
	if err := pdb.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = OpenDirs(data, dir, decodeTest, Options{Recovery: RecoveryAutomatic})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	contains(t, db, 1, 'a', true)
}
