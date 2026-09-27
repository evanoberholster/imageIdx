package storage

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestAcquireReleaseLock(t *testing.T) {
	dir := t.TempDir()
	lock, err := AcquireLock(dir, ".test.lock", true)
	if err != nil {
		t.Fatal(err)
	}
	// A second opener fails fast instead of stalling.
	if _, err := AcquireLock(dir, ".test.lock", false); !errors.Is(err, ErrBusy) {
		t.Fatalf("second acquire err = %v, want ErrBusy", err)
	}
	if err := ReleaseLock(lock); err != nil {
		t.Fatal(err)
	}
	// After release the directory opens again.
	reopened, err := AcquireLock(dir, ".test.lock", false)
	if err != nil {
		t.Fatal(err)
	}
	if err := ReleaseLock(reopened); err != nil {
		t.Fatal(err)
	}
	if err := ReleaseLock(nil); err != nil {
		t.Fatalf("ReleaseLock(nil) err = %v, want nil", err)
	}
}

func TestAcquireCreateRejectsNonEmpty(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "data"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := AcquireLock(dir, ".test.lock", true); !errors.Is(err, ErrExists) {
		t.Fatalf("create over data err = %v, want ErrExists", err)
	}
	// The failed create releases the lock it took.
	reopened, err := AcquireLock(dir, ".test.lock", false)
	if err != nil {
		t.Fatalf("reacquire after failed create: %v", err)
	}
	_ = ReleaseLock(reopened)
}
