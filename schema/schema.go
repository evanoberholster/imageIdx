// Package schema owns a standalone, single-file namespace directory and
// crash-safe, per-namespace ValueID allocation. It does not store records.
package schema

import (
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"unicode/utf8"

	"github.com/evanoberholster/imageIdx/common"
	"github.com/evanoberholster/imageIdx/internal/storage"
)

type Backend uint8

const (
	BackendReadWrite Backend = iota
	BackendMmap
)

type NamespaceConfig struct {
	ID          common.Namespace
	Name        string
	ReserveSize uint64
}

type Options struct {
	Backend    Backend
	Namespaces []NamespaceConfig
	fault      func(string) error // deterministic crash/failure injection in package tests
}

type namespaceAllocator struct {
	id     common.Namespace
	name   string
	block  uint64
	offset uint64
	mu     sync.Mutex
	issued atomic.Uint64
	limit  atomic.Uint64
	record allocationRecord // guarded by mu; never read on the allocation fast path
}

// Schema holds a fixed sorted namespace directory. The backing file is locked
// for the entire lifetime; allocate only through one Schema per file.
type Schema struct {
	f         *os.File
	lock      *os.File
	mapped    *storage.MappedFile
	backend   Backend
	entries   []*namespaceAllocator
	state     atomic.Uint64 // closing bit | admitted operations
	poisoned  atomic.Bool
	closeMu   sync.Mutex
	closeCond *sync.Cond
	closeOnce sync.Once
	closeErr  error
	fault     func(string) error
}

const closing uint64 = 1 << 63

func configs(opts Options) ([]NamespaceConfig, error) {
	if opts.Backend != BackendReadWrite && opts.Backend != BackendMmap {
		return nil, fmt.Errorf("schema: invalid backend %d", opts.Backend)
	}
	if len(opts.Namespaces) == 0 || len(opts.Namespaces) > math.MaxUint16 {
		return nil, ErrNamespace
	}
	defs := append([]NamespaceConfig(nil), opts.Namespaces...)
	slices.SortFunc(defs, func(a, b NamespaceConfig) int {
		if a.ID < b.ID {
			return -1
		}
		if a.ID > b.ID {
			return 1
		}
		return 0
	})
	names := make(map[string]struct{}, len(defs)) // initialization only
	for i := range defs {
		d := &defs[i]
		if !d.ID.Valid() || len(d.Name) == 0 || len(d.Name) > maxName || !utf8.ValidString(d.Name) {
			return nil, ErrNamespace
		}
		if i != 0 && defs[i-1].ID == d.ID {
			return nil, ErrNamespaceExists
		}
		if _, exists := names[d.Name]; exists {
			return nil, ErrNamespaceExists
		}
		names[d.Name] = struct{}{}
		if d.ReserveSize == 0 {
			d.ReserveSize = defaultReserve
		}
	}
	return defs, nil
}

// Open opens path as the single allocation file (not a directory). Existing
// IDs/names must match; new namespaces and reservation-size changes publish
// a new checksummed directory while no Schema is exposed to callers.
func Open(path string, opts Options) (_ *Schema, err error) {
	defs, err := configs(opts)
	if err != nil {
		return nil, err
	}
	if path == "" || filepath.Base(path) == "." {
		return nil, ErrCorrupt
	}
	lock, err := storage.AcquireLock(filepath.Dir(path), filepath.Base(path)+".lock", false)
	if err != nil {
		if errors.Is(err, storage.ErrBusy) {
			return nil, errors.Join(ErrLocked, err)
		}
		return nil, err
	}
	s := &Schema{lock: lock, backend: opts.Backend, fault: opts.fault}
	s.closeCond = sync.NewCond(&s.closeMu)
	defer func() {
		if err != nil {
			if s.mapped != nil {
				_ = s.mapped.Close()
			}
			if s.f != nil {
				_ = s.f.Close()
			}
			_ = storage.ReleaseLock(lock)
		}
	}()
	f, openErr := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	created := openErr == nil
	if errors.Is(openErr, os.ErrExist) {
		f, openErr = os.OpenFile(path, os.O_RDWR, 0)
	}
	if openErr != nil {
		return nil, openErr
	}
	s.f = f
	if created {
		err = s.create(defs)
	} else {
		err = s.recover(defs)
	}
	if err != nil {
		return nil, err
	}
	if opts.Backend == BackendMmap {
		s.mapped, err = storage.OpenMapped(path)
		if err != nil {
			return nil, err
		}
		if err = s.f.Close(); err != nil {
			return nil, err
		}
		s.f = nil
	}
	return s, nil
}

func (s *Schema) entry(ns common.Namespace) *namespaceAllocator {
	lo, hi := 0, len(s.entries)
	for lo < hi {
		mid := int(uint(lo+hi) >> 1)
		if s.entries[mid].id < ns {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	if lo < len(s.entries) && s.entries[lo].id == ns {
		return s.entries[lo]
	}
	return nil
}

func (s *Schema) admit() error {
	for {
		state := s.state.Load()
		if state&closing != 0 {
			return ErrClosed
		}
		if s.state.CompareAndSwap(state, state+1) {
			break
		}
	}
	if s.poisoned.Load() {
		s.release()
		return ErrCorrupt
	}
	return nil
}

func (s *Schema) release() {
	if s.state.Add(^uint64(0)) == closing {
		s.closeMu.Lock()
		s.closeCond.Broadcast()
		s.closeMu.Unlock()
	}
}

func (s *Schema) read(dst []byte, off uint64) error {
	if s.mapped != nil {
		data := s.mapped.Bytes()
		if off > uint64(len(data)) || uint64(len(dst)) > uint64(len(data))-off {
			return ErrCorrupt
		}
		copy(dst, data[off:off+uint64(len(dst))])
		return nil
	}
	n, err := s.f.ReadAt(dst, int64(off))
	if err != nil {
		return err
	}
	if n != len(dst) {
		return io.ErrUnexpectedEOF
	}
	return nil
}

func (s *Schema) write(src []byte, off uint64) error {
	if s.mapped != nil {
		data := s.mapped.Bytes()
		if off > uint64(len(data)) || uint64(len(src)) > uint64(len(data))-off {
			return ErrCorrupt
		}
		copy(data[off:off+uint64(len(src))], src)
		return nil
	}
	n, err := s.f.WriteAt(src, int64(off))
	if err != nil {
		return err
	}
	if n != len(src) {
		return io.ErrShortWrite
	}
	return nil
}

func (s *Schema) sync() error {
	if s.mapped != nil {
		return s.mapped.Sync()
	}
	return s.f.Sync()
}

func (s *Schema) Close() error {
	s.closeOnce.Do(func() {
		s.closeMu.Lock()
		for {
			old := s.state.Load()
			if s.state.CompareAndSwap(old, old|closing) {
				break
			}
		}
		for s.state.Load() != closing {
			s.closeCond.Wait()
		}
		s.closeMu.Unlock()
		if !s.poisoned.Load() {
			for _, a := range s.entries {
				a.mu.Lock()
				err := s.persist(a, allocationRecord{a.record.generation + 1, a.limit.Load(), a.issued.Load(), true})
				a.mu.Unlock()
				if err != nil {
					s.closeErr = err
					break
				}
			}
		} else {
			s.closeErr = ErrCorrupt
		}
		if s.mapped != nil {
			s.closeErr = errors.Join(s.closeErr, s.mapped.Close())
		}
		if s.f != nil {
			s.closeErr = errors.Join(s.closeErr, s.f.Close())
		}
		s.closeErr = errors.Join(s.closeErr, storage.ReleaseLock(s.lock))
	})
	return s.closeErr
}
