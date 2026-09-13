package bitmapidx

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"

	"github.com/evanoberholster/imageIdx/internal/persist"
)

var (
	ErrStoreExists      = errors.New("bitmapidx: store directory is not empty")
	ErrStoreBusy        = errors.New("bitmapidx: store directory already has a writer")
	ErrOwnedIndex       = errors.New("bitmapidx: close the owning store instead")
	ErrRecoveryRequired = persist.ErrWALFailed
)

// The lock file is never unlinked: every opener must lock the same inode.
func lockStore(dir string, create bool) (*os.File, error) {
	if create {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
	}
	f, err := os.OpenFile(filepath.Join(dir, ".bitmapidx.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, errors.Join(ErrStoreBusy, err)
	}
	if create {
		entries, err := os.ReadDir(dir)
		if err != nil {
			unlockStore(f)
			return nil, err
		}
		for _, entry := range entries {
			if entry.Name() != ".bitmapidx.lock" {
				unlockStore(f)
				return nil, ErrStoreExists
			}
		}
	}
	return f, nil
}

func unlockStore(f *os.File) error {
	if f == nil {
		return nil
	}
	return errors.Join(syscall.Flock(int(f.Fd()), syscall.LOCK_UN), f.Close())
}
