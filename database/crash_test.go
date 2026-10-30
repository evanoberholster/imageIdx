package database

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	schema "github.com/evanoberholster/imageIdx/schema"
)

func TestCrashChild(t *testing.T) {
	phase := os.Getenv("IMAGEIDX_CRASH_PHASE")
	if phase == "" {
		return
	}
	root := os.Getenv("IMAGEIDX_CRASH_ROOT")
	db, err := OpenDirs(filepath.Join(root, "data"), filepath.Join(root, "index"), decodeTest, Options{
		boundary: func(at string) {
			if at == phase {
				os.Exit(73)
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	gen, err := db.Commit(Mutation{Entity: schema.EntityImage, ValueID: 1, Record: []byte("canonical"), Projection: projection(1, 'x')})
	if err != nil {
		t.Fatal(err)
	}
	wait(t, db, gen)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	t.Fatal("fault boundary was not reached")
}

func TestCrashBoundaries(t *testing.T) {
	for _, phase := range []string{
		"before canonical commit", "after canonical commit", "before index apply",
		"after durable index", "after acknowledgment", "during cleanup",
	} {
		t.Run(phase, func(t *testing.T) {
			data, idxPath := paths(t)
			root := filepath.Dir(data)
			cmd := exec.Command(os.Args[0], "-test.run=^TestCrashChild$")
			cmd.Env = append(os.Environ(), "IMAGEIDX_CRASH_PHASE="+phase, "IMAGEIDX_CRASH_ROOT="+root)
			err := cmd.Run()
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != 73 {
				t.Fatalf("child exit = %v", err)
			}
			if phase == "before index apply" {
				_, err = OpenDirs(data, idxPath, decodeTest, Options{})
				if !errors.Is(err, ErrRecoveryRequired) {
					t.Fatalf("interrupted batch: %v", err)
				}
				return // unknown engine state must never be acknowledged
			}
			db := openTest(t, data, idxPath)
			defer db.Close()
			if phase == "before canonical commit" {
				if db.DatabaseGeneration() != 0 {
					t.Fatal("uncommitted mutation visible")
				}
				return
			}
			if got, err := db.Get(schema.EntityImage, 1); err != nil || string(got) != "canonical" {
				t.Fatalf("canonical record = %q: %v", got, err)
			}
			wait(t, db, 1)
			contains(t, db, 1, 'x', true)
		})
	}
}
