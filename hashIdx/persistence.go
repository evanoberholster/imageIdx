package hashidx

import (
	"errors"
	"os"
	"path/filepath"
	"sync"

	"github.com/evanoberholster/imageIdx/internal/indexutil"
	"github.com/evanoberholster/imageIdx/internal/persist"
)

// persistedState is the on-disk state both index kinds share: directory,
// durability policy, WAL, mapped generations, and the last checkpointed
// generation. Only the single writer touches it; readers never do.
type persistedState struct {
	mu           sync.Mutex
	dir          string
	durability   Durability
	wal          *persist.WAL
	walEpoch     uint64
	mappings     [][]byte
	persistedGen uint64
}

// close releases the WAL and every live mapping. It must run after readers
// have drained.
func (s *persistedState) close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var err error
	if s.wal != nil {
		err = errors.Join(err, s.wal.Close())
		s.wal = nil
	}
	for _, mapping := range s.mappings {
		err = errors.Join(err, persist.Unmap(mapping))
	}
	s.mappings = nil
	return err
}

// chooseReaders returns the first non-nil tracker or a fresh private one.
// Store passes its tracker so both indexes share one reader lifetime domain.
func chooseReaders(readers []*indexutil.ReaderTracker) *indexutil.ReaderTracker {
	for _, tracker := range readers {
		if tracker != nil {
			return tracker
		}
	}
	return &indexutil.ReaderTracker{}
}

// onDiskBytes sums the sizes of every file matching one of patterns under dir.
// Patterns may be globs or literal file names.
func onDiskBytes(dir string, patterns ...string) uint64 {
	var total uint64
	for _, pattern := range patterns {
		matches, _ := filepath.Glob(filepath.Join(dir, pattern))
		for _, path := range matches {
			if info, err := os.Stat(path); err == nil {
				total += uint64(info.Size())
			}
		}
	}
	return total
}

// writeAtomicFile writes path+".tmp" through write, fsyncs it, and renames it
// over path.
func writeAtomicFile(path string, write func(file *os.File) error) error {
	tmpPath := path + ".tmp"
	file, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if err = write(file); err == nil {
		err = file.Sync()
	}
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}

// storageAmplification returns physical/live, or 1 for an empty live set.
func storageAmplification(live, physical uint64) float64 {
	if live == 0 {
		return 1
	}
	return float64(physical) / float64(live)
}
