package timeidx

import (
	"errors"
	"os"
	"path/filepath"

	"github.com/evanoberholster/imageIdx/common"
	"github.com/evanoberholster/imageIdx/internal/storage"
)

// PublishRebuild atomically installs an independently built and verified file.
// Use ONLY during startup with both namespaces closed. The application must
// coordinate canonical DB snapshot/replay before calling this function. Locks
// exclude active index owners; no database logic or automatic rebuilding occurs.
// Cross-filesystem rename is not supported. A directory-sync failure after
// rename is uncertain: inspect/reopen the final file rather than recreating it.
func PublishRebuild(dir, stagedDir string, ns common.Namespace) error {
	if !ns.Valid() {
		return ErrInvalidNamespace
	}
	final, err := filepath.Abs(filepath.Join(dir, NamespaceFilename(ns)))
	if err != nil {
		return err
	}
	staged, err := filepath.Abs(filepath.Join(stagedDir, NamespaceFilename(ns)))
	if err != nil {
		return err
	}
	if final == staged {
		return ErrInvalidConfig
	}
	lock, err := storage.AcquireLock(dir, NamespaceFilename(ns)+".lock", false)
	if err != nil {
		return err
	}
	defer storage.ReleaseLock(lock)
	stageLock, err := storage.AcquireLock(stagedDir, NamespaceFilename(ns)+".lock", false)
	if err != nil {
		return err
	}
	defer storage.ReleaseLock(stageLock)
	if err = VerifyNamespace(stagedDir, ns); err != nil {
		return err
	}
	mapping, err := storage.OpenMapped(staged)
	if err != nil {
		return err
	}
	err = errors.Join(mapping.Sync(), mapping.Close())
	if err != nil {
		return err
	}
	if err = os.Rename(staged, final); err != nil {
		return err
	}
	if err = storage.SyncDir(dir); err != nil {
		return errors.Join(ErrRecoveryRequired, err)
	}
	if filepath.Dir(final) != filepath.Dir(staged) {
		if err = storage.SyncDir(stagedDir); err != nil {
			return errors.Join(ErrRecoveryRequired, err)
		}
	}
	return nil
}
