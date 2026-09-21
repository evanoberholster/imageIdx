package bitmapidx

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"

	"github.com/evanoberholster/imageIdx/internal/persist"
)

var (
	// ErrStoreExists reports creating a store in a non-empty directory.
	ErrStoreExists = errors.New("bitmapidx: store directory is not empty")
	// ErrStoreBusy reports another process holding the store lock.
	ErrStoreBusy = errors.New("bitmapidx: store directory already has a writer")
	// ErrOwnedIndex reports Close on a store-owned FieldIndex. Close the
	// owning Index instead; it owns all child and mmap lifetimes.
	ErrOwnedIndex = errors.New("bitmapidx: close the owning store instead")
	// ErrRecoveryRequired reports a WAL failure that leaves the store
	// failed until recovery. It aliases persist.ErrWALFailed.
	ErrRecoveryRequired = persist.ErrWALFailed
)

// lockStore exclusively locks the store directory. It uses flock, so it is
// Unix-only and has no Windows fallback. The lock file is never unlinked:
// every opener must lock the same inode. When create is true the directory
// must otherwise be empty.
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

// unlockStore releases the directory lock and closes the handle. A nil
// handle is a no-op.
func unlockStore(f *os.File) error {
	if f == nil {
		return nil
	}
	return errors.Join(syscall.Flock(int(f.Fd()), syscall.LOCK_UN), f.Close())
}
