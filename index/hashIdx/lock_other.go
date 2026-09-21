//go:build !(unix || windows)

package hashidx

import (
	"errors"
	"os"
)

var errLockUnsupported = errors.New("hashidx: file locking unsupported on this platform")

// lockStoreDir reports an explicit error where neither flock nor
// LockFileEx exists. Running without single-writer exclusion would risk
// silent corruption, so unsupported platforms fail fast instead of
// proceeding unlocked.
func lockStoreDir(_ string, _ bool) (*os.File, error) {
	return nil, errLockUnsupported
}

func unlockStoreDir(f *os.File) error {
	if f == nil {
		return nil
	}
	return f.Close()
}
