//go:build !(unix || windows)

package storage

import (
	"errors"
	"os"
)

var errLockUnsupported = errors.New("filelock: file locking unsupported on this platform")

// AcquireLock reports an explicit error where neither flock nor LockFileEx
// exists. Running without single-writer exclusion would risk silent
// corruption, so unsupported platforms fail fast instead of proceeding
// unlocked.
func AcquireLock(_ string, _ string, _ bool) (*os.File, error) {
	return nil, errLockUnsupported
}

// ReleaseLock closes the handle. A nil handle is a no-op.
func ReleaseLock(f *os.File) error {
	if f == nil {
		return nil
	}
	return f.Close()
}
