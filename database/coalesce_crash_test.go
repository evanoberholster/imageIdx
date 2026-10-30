package database

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestCoalescingCrashChild(t *testing.T) {
	phase := os.Getenv("IMAGEIDX_COALESCE_CRASH_PHASE")
	if phase == "" {
		return
	}
	root := os.Getenv("IMAGEIDX_COALESCE_CRASH_ROOT")
	db, err := OpenDirs(filepath.Join(root, "data"), filepath.Join(root, "index"), decodePair, Options{
		BatchSize: 64,
		boundary: func(at string) {
			if at == phase {
				os.Exit(75)
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.WaitIndexed(t.Context(), 3); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	t.Fatal("coalescing crash boundary was not reached")
}

func TestCoalescingCrashBoundaries(t *testing.T) {
	for _, phase := range []string{
		"before index apply", "after durable index", "after acknowledgment", "during cleanup",
	} {
		t.Run(phase, func(t *testing.T) {
			data, idxPath := paths(t)
			db, err := OpenDirs(data, idxPath, decodePair, Options{BatchSize: 64})
			if err != nil {
				t.Fatal(err)
			}
			writeCoalescingFixture(t, db)
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			root := filepath.Dir(data)
			cmd := exec.Command(os.Args[0], "-test.run=^TestCoalescingCrashChild$")
			cmd.Env = append(os.Environ(), "IMAGEIDX_COALESCE_CRASH_PHASE="+phase, "IMAGEIDX_COALESCE_CRASH_ROOT="+root)
			runErr := cmd.Run()
			var exit *exec.ExitError
			if !errors.As(runErr, &exit) || exit.ExitCode() != 75 {
				t.Fatalf("child exit = %v", runErr)
			}
			if phase == "before index apply" {
				if _, err := OpenDirs(data, idxPath, decodePair, Options{}); !errors.Is(err, ErrRecoveryRequired) {
					t.Fatalf("interrupted coalesced batch: %v", err)
				}
				return
			}
			db, err = OpenDirs(data, idxPath, decodePair, Options{BatchSize: 64})
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if err := db.WaitIndexed(t.Context(), 3); err != nil {
				t.Fatal(err)
			}
			assertPairState(t, db)
		})
	}
}
