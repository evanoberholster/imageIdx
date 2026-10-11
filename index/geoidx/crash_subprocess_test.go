package geoidx

import (
	"os"
	"os/exec"
	"testing"
)

// stageName maps a persist stage to the env value used by the subprocess crash
// test.
func stageName(stage persistStage) string {
	switch stage {
	case persistCommit:
		return "commit"
	case persistSuperSync:
		return "super-sync"
	}
	return ""
}

// TestSubprocessCrashRecovery re-executes the test binary and hard-exits at a
// superblock publication boundary, then reopens the directory in the parent and
// requires the committed generation to be readable. It exercises real process
// termination rather than an in-process error.
func TestSubprocessCrashRecovery(t *testing.T) {
	if dir := os.Getenv("GEOIDX_CRASH_DIR"); dir != "" {
		runCrashChild(t, dir, os.Getenv("GEOIDX_CRASH_STAGE"))
		return
	}
	for _, stage := range []string{"commit", "super-sync"} {
		t.Run(stage, func(t *testing.T) {
			dir := t.TempDir()
			cmd := exec.Command(os.Args[0], "-test.run=TestSubprocessCrashRecovery")
			cmd.Env = append(os.Environ(), "GEOIDX_CRASH_DIR="+dir, "GEOIDX_CRASH_STAGE="+stage)
			out, err := cmd.CombinedOutput()
			if err == nil {
				t.Fatalf("child exited cleanly; expected a crash at %s:\n%s", stage, out)
			}

			cfg := tinyShardConfig()
			cfg.MaxFileBytes = 1 << 20
			index, oerr := Open(dir, cfg)
			if oerr != nil {
				t.Fatalf("reopen after crash at %s: %v\n%s", stage, oerr, out)
			}
			defer index.Close()
			// The pre-crash Checkpoint committed 2000 records; the extra pending
			// batch may or may not be durable depending on the boundary.
			if got := index.Len(); got < 2000 {
				t.Fatalf("Len after crash at %s = %d, want >= 2000", stage, got)
			}
			if !validateDirectory(index.view.Load().shards) {
				t.Fatalf("directory invalid after crash at %s", stage)
			}
		})
	}
}

func runCrashChild(t *testing.T, dir, stage string) {
	cfg := tinyShardConfig()
	cfg.MaxFileBytes = 1 << 20
	index, err := New(dir, cfg)
	if err != nil {
		t.Fatalf("child New: %v", err)
	}
	insertChunked(t, index, shardedMemoryEntries(2000))
	if err := index.Checkpoint(); err != nil {
		t.Fatalf("child Checkpoint: %v", err)
	}
	// Pending, uncommitted batch.
	more := withNewIDs(shardedMemoryEntries(2000), 1_000_000)
	if err := index.InsertBatch(more); err != nil {
		t.Fatalf("child InsertBatch: %v", err)
	}
	persistFail = func(s persistStage) error {
		if stageName(s) == stage {
			os.Exit(7) // simulate process termination at the publication boundary
		}
		return nil
	}
	_ = index.Compact()
	os.Exit(0)
}

// TestGrowthCrashFallback builds two valid generations, forces file growth, and
// hard-exits before the commit. The parent reopens and requires the previous
// committed generation to be recoverable, the allocator reconstruction to be
// correct, and a subsequent compaction to succeed.
func TestGrowthCrashFallback(t *testing.T) {
	if dir := os.Getenv("GEOIDX_GROW_CRASH_DIR"); dir != "" {
		runGrowthCrashChild(t, dir)
		return
	}
	dir := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=TestGrowthCrashFallback")
	cmd.Env = append(os.Environ(), "GEOIDX_GROW_CRASH_DIR="+dir)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("child exited cleanly; expected a crash:\n%s", out)
	}
	cfg := growthConfig()
	index, oerr := Open(dir, cfg)
	if oerr != nil {
		t.Fatalf("reopen after growth crash: %v\n%s", oerr, out)
	}
	defer index.Close()
	if !validateDirectory(index.view.Load().shards) {
		t.Fatal("recovered directory invalid")
	}
	if got := index.Len(); got < 50_000 {
		t.Fatalf("committed generation lost: Len = %d, want >= 50000", got)
	}
	// The reconstructed allocator must be usable and the file must not keep
	// growing without bound: a subsequent compaction and write must succeed.
	if err := index.InsertBatch(withNewIDs(shardedMemoryEntries(1000), 5_000_000)); err != nil {
		t.Fatal(err)
	}
	if err := index.Compact(); err != nil {
		t.Fatalf("post-crash compaction: %v", err)
	}
	if index.StorageStats().LiveBytes == 0 {
		t.Fatal("no live bytes after recovery")
	}
}

func runGrowthCrashChild(t *testing.T, dir string) {
	cfg := growthConfig()
	index, err := New(dir, cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	insertChunked(t, index, shardedMemoryEntries(50_000))
	if err := index.Checkpoint(); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	// This batch overflows the small arena, so the next compaction grows the file
	// before it reaches the commit boundary.
	if err := index.InsertBatch(withNewIDs(shardedMemoryEntries(400_000), 1_000_000)); err != nil {
		t.Fatalf("InsertBatch: %v", err)
	}
	persistFail = func(s persistStage) error {
		if s == persistCommit {
			os.Exit(9) // crash after growth, before the superblock commit
		}
		return nil
	}
	_ = index.Compact()
	os.Exit(0)
}
