package timeidx

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func rebuildFixture(t *testing.T) (string, string) {
	t.Helper()
	dir, stage := t.TempDir(), t.TempDir()
	for n, d := range []string{dir, stage} {
		i, err := NewStore(d, Spec{Namespace: 1}, Config{}, DurabilitySync)
		if err != nil {
			t.Fatal(err)
		}
		sourceApply(t, i, uint64(7+n), Insert(uint64(n+1), uint64(n+1)))
		if err := i.Close(); err != nil {
			t.Fatal(err)
		}
	}
	return dir, stage
}

func checkRebuildFile(t *testing.T, dir string, replacement bool) {
	t.Helper()
	i, err := OpenNamespace(dir, 1, Config{}, DurabilitySync)
	if err != nil {
		t.Fatal(err)
	}
	defer i.Close()
	id, generation := uint64(1), uint64(7)
	if replacement {
		id, generation = 2, 8
	}
	got := i.Search(nil, 0, 3)
	if len(got) != 1 || got[0] != id || i.Namespace() != 1 || i.SourceGeneration() != generation {
		t.Fatal("incoherent rebuilt file", got, i.SourceGeneration())
	}
	if err := VerifyNamespace(dir, 1); err != nil {
		t.Fatal(err)
	}
}

func TestRebuildFaultBoundaries(t *testing.T) {
	for _, boundary := range []string{"final-lock", "staging-lock", "verify", "staged-sync", "rename", "renamed", "destination-sync", "source-sync"} {
		t.Run(boundary, func(t *testing.T) {
			dir, stage := rebuildFixture(t)
			ioerr := &os.PathError{Op: boundary, Path: stage, Err: os.ErrPermission}
			err := publishRebuild(dir, stage, 1, func(s string) error {
				if s == boundary {
					return ioerr
				}
				return nil
			})
			if !errors.Is(err, os.ErrPermission) || errors.Is(err, ErrCorrupt) || errors.Is(err, ErrRebuildRequired) {
				t.Fatal(err)
			}
			after := boundary == "renamed" || boundary == "destination-sync" || boundary == "source-sync"
			if errors.Is(err, ErrRecoveryRequired) != after {
				t.Fatal("wrong certainty", err)
			}
			checkRebuildFile(t, dir, after)
			if !after {
				checkRebuildFile(t, stage, true)
				// Prove both locks were released on every pre-rename failure.
				if err := PublishRebuild(dir, stage, 1); err != nil {
					t.Fatal(err)
				}
				checkRebuildFile(t, dir, true)
			}
		})
	}
}

func TestRebuildStagedCreationFailure(t *testing.T) {
	for _, boundary := range []string{"create-file", "data-sync", "create-name"} {
		t.Run(boundary, func(t *testing.T) {
			dir, stage := rebuildFixture(t)
			// Build another replacement, injecting creation/data sync failure.
			failedStage := t.TempDir()
			_, err := createStore(failedStage, Spec{Namespace: 1, Entries: []Entry{{Timestamp: 3, ImageID: 3}}}, Config{}, DurabilitySync, func(i *Index) {
				i.fault = func(s string) error {
					if s == boundary {
						return os.ErrPermission
					}
					return nil
				}
			})
			if !errors.Is(err, os.ErrPermission) || errors.Is(err, ErrCorrupt) {
				t.Fatal(err)
			}
			if err := PublishRebuild(dir, failedStage, 1); err == nil {
				t.Fatal("published incomplete stage")
			}
			checkRebuildFile(t, dir, false)
			if err := PublishRebuild(dir, stage, 1); err != nil {
				t.Fatal(err)
			}
			checkRebuildFile(t, dir, true)
		})
	}
}

func TestRebuildWrongNamespace(t *testing.T) {
	dir, _ := rebuildFixture(t)
	stage := t.TempDir()
	i, err := NewStore(stage, Spec{Namespace: 2}, Config{}, DurabilitySync)
	if err != nil {
		t.Fatal(err)
	}
	i.Close()
	if err := os.Rename(filepath.Join(stage, NamespaceFilename(2)), filepath.Join(stage, NamespaceFilename(1))); err != nil {
		t.Fatal(err)
	}
	if err := PublishRebuild(dir, stage, 1); !errors.Is(err, ErrNamespaceMismatch) {
		t.Fatal(err)
	}
	checkRebuildFile(t, dir, false)
}

func TestRebuildProcessCrash(t *testing.T) {
	if dir := os.Getenv("TIMEIDX_REBUILD_FINAL"); dir != "" {
		boundary := os.Getenv("TIMEIDX_REBUILD_BOUNDARY")
		err := publishRebuild(dir, os.Getenv("TIMEIDX_REBUILD_STAGE"), 1, func(s string) error {
			if s == boundary {
				os.Exit(83)
			}
			return nil
		})
		t.Fatal("did not terminate", err)
	}
	for _, boundary := range []string{"rename", "renamed"} {
		t.Run(boundary, func(t *testing.T) {
			dir, stage := rebuildFixture(t)
			cmd := exec.Command(os.Args[0], "-test.run=^TestRebuildProcessCrash$")
			cmd.Env = append(os.Environ(), "TIMEIDX_REBUILD_FINAL="+dir, "TIMEIDX_REBUILD_STAGE="+stage, "TIMEIDX_REBUILD_BOUNDARY="+boundary)
			out, err := cmd.CombinedOutput()
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != 83 {
				t.Fatalf("not injected termination: %v %s", err, out)
			}
			// Process exit is not a power-cut simulation: after a completed
			// rename the replacement is visible; durability is still uncertain.
			checkRebuildFile(t, dir, boundary == "renamed")
			if boundary == "rename" {
				checkRebuildFile(t, stage, true)
			}
		})
	}
}
