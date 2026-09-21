//go:build windows

package hashidx

import (
	"errors"
	"os"
	"path/filepath"

	"golang.org/x/sys/windows"
)

// lockStoreDir exclusively locks the store directory with LockFileEx. The
// lock file is never unlinked: every opener must lock the same file. When
// create is true the directory must otherwise be empty.
func lockStoreDir(dir string, create bool) (*os.File, error) {
	if create {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
	}
	f, err := os.OpenFile(filepath.Join(dir, storeLockName), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	// LOCKFILE_FAIL_IMMEDIATELY mirrors the non-blocking Unix acquisition:
	// a held lock reports ErrStoreBusy instead of stalling the opener.
	var ol windows.Overlapped
	if err = windows.LockFileEx(windows.Handle(f.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
		0, 1, 0, &ol); err != nil {
		_ = f.Close()
		return nil, errors.Join(ErrStoreBusy, err)
	}
	if create {
		entries, err := os.ReadDir(dir)
		if err != nil {
			_ = unlockStoreDir(f)
			return nil, err
		}
		for _, entry := range entries {
			if entry.Name() != storeLockName {
				_ = unlockStoreDir(f)
				return nil, ErrStoreExists
			}
		}
	}
	return f, nil
}

func unlockStoreDir(f *os.File) error {
	if f == nil {
		return nil
	}
	var ol windows.Overlapped
	return errors.Join(windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, &ol), f.Close())
}
