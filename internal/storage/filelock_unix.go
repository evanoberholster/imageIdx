//go:build unix

package storage

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
)

// AcquireLock exclusively locks the lock file name inside dir, creating the
// directory and the file as needed. A held lock reports ErrBusy instead
// of stalling the opener.
func AcquireLock(dir, name string, create bool) (*os.File, error) {
	if create {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
	}
	f, err := os.OpenFile(filepath.Join(dir, name), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, errors.Join(ErrBusy, err)
	}
	if create {
		if err := checkEmpty(dir, name, f); err != nil {
			return nil, err
		}
	}
	return f, nil
}

// ReleaseLock unlocks and closes the handle. A nil handle is a no-op.
func ReleaseLock(f *os.File) error {
	if f == nil {
		return nil
	}
	return errors.Join(syscall.Flock(int(f.Fd()), syscall.LOCK_UN), f.Close())
}
