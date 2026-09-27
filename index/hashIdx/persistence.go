package hashidx

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"

	common "github.com/evanoberholster/imageIdx/common"
	"github.com/evanoberholster/imageIdx/internal/storage"
)

// persistedState is the on-disk state both index kinds share: directory,
// durability policy, WAL, mapped generations, and the last checkpointed
// generation. Only the single writer touches it; readers never do.
type persistedState struct {
	mu           sync.Mutex
	dir          string
	durability   common.Durability
	wal          *storage.WAL
	walEpoch     uint64
	mappings     [][]byte
	persistedGen uint64
}

// close releases the WAL and every live mapping. It must run after readers
// have drained. An already-closed WAL handle reports os.ErrClosed, which is
// tolerated so cleanup (unmapping, lock release by the caller) always
// proceeds; any other WAL error is propagated.
func (s *persistedState) close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var err error
	if s.wal != nil {
		if cerr := s.wal.Close(); cerr != nil && !errors.Is(cerr, os.ErrClosed) {
			err = errors.Join(err, cerr)
		}
		s.wal = nil
	}
	for _, mapping := range s.mappings {
		err = errors.Join(err, storage.Unmap(mapping))
	}
	s.mappings = nil
	return err
}

// chooseReaders returns the first non-nil tracker or a fresh private one.
// Store passes its tracker so both indexes share one reader lifetime domain.
func chooseReaders(readers []*storage.ReaderTracker) *storage.ReaderTracker {
	for _, tracker := range readers {
		if tracker != nil {
			return tracker
		}
	}
	return &storage.ReaderTracker{}
}

// writeAtomicFile streams path through write, fsyncs it, and renames it
// over path. Generation files are owner-only (0o600), matching WAL and
// segment files everywhere else in the package.
func writeAtomicFile(path string, write func(io.Writer) error) error {
	dir, base := filepath.Dir(path), filepath.Base(path)
	return storage.WriteFileAtomicFunc(dir, base+".tmp", base, 0o600, write)
}

// storageAmplification returns physical/live, or 1 for an empty live set.
func storageAmplification(live, physical uint64) float64 {
	if live == 0 {
		return 1
	}
	return float64(physical) / float64(live)
}
