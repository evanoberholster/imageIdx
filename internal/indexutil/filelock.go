// Directory locking serializes single-writer store directories for the
// index packages. AcquireLock takes an exclusive, non-blocking lock on a
// never-unlinked lock file inside the directory; every opener must lock
// the same file. When create is true the directory must otherwise be
// empty. The backend is flock on Unix and LockFileEx on Windows (see
// filelock_unix.go, filelock_windows.go); other platforms fail fast
// instead of proceeding unlocked.
package indexutil

import (
	"errors"
	"os"
)

var (
	// ErrBusy reports another process holding the store lock.
	ErrBusy = errors.New("filelock: store directory already has a writer")
	// ErrExists reports creating a store in a non-empty directory.
	ErrExists = errors.New("filelock: store directory is not empty")
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
