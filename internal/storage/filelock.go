package storage

import (
	"errors"
	"os"
)

var (
	// ErrBusy reports another process holding the store lock.
	ErrBusy = errors.New("storage: store directory already has a writer")
	// ErrExists reports creating a store in a non-empty directory.
	ErrExists = errors.New("storage: store directory is not empty")
)

// checkEmpty verifies dir holds nothing but the lock file, releasing f on
// any failure. Callers hold no other state, so a released handle is simply
// discarded.
func checkEmpty(dir, name string, f *os.File) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		_ = ReleaseLock(f)
		return err
	}
	for _, entry := range entries {
		if entry.Name() != name {
			_ = ReleaseLock(f)
			return ErrExists
		}
	}
	return nil
}
