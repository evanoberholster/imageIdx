package persist

import (
	"os"
	"path/filepath"
)

// RemoveObsolete removes files matching pattern except keep, ignoring
// already-removed entries. Callers checkpoint first and drain readers after
// reclaiming the old generations.
func RemoveObsolete(dir, pattern, keep string) error {
	paths, err := filepath.Glob(filepath.Join(dir, pattern))
	if err != nil {
		return err
	}
	for _, path := range paths {
		if path != keep {
			if err = os.Remove(path); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
	}
	return nil
}
