//go:build unix

package hashidx

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
)

// lockStoreDir exclusively locks the store directory with flock. The lock
// file is never unlinked: every opener must lock the same inode. When
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
	if err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
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
	return errors.Join(syscall.Flock(int(f.Fd()), syscall.LOCK_UN), f.Close())
}
