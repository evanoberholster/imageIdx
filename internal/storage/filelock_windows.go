//go:build windows

package storage

import (
	"errors"
	"os"
	"path/filepath"

	"golang.org/x/sys/windows"
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
	// LOCKFILE_FAIL_IMMEDIATELY mirrors the non-blocking Unix acquisition.
	var ol windows.Overlapped
	if err = windows.LockFileEx(windows.Handle(f.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
		0, 1, 0, &ol); err != nil {
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
	var ol windows.Overlapped
	return errors.Join(windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, &ol), f.Close())
}
