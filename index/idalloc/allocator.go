// Package idalloc issues independent, monotonically increasing entity IDs.
// Reservations, not surviving records, are authoritative after a restart.
package idalloc

import (
	"errors"
	"fmt"
	"math"
	"runtime"
	"sync"
	"sync/atomic"

	"github.com/evanoberholster/imageIdx/common"
)

const BlockSize uint64 = 1000

var (
	ErrKind      = errors.New("idalloc: invalid entity kind")
	ErrClosed    = errors.New("idalloc: allocator closed")
	ErrExhausted = errors.New("idalloc: entity IDs exhausted")
)

// HighWaterStore must durably and atomically persist each watermark before
// ReserveThrough returns nil. It must reject decreases and support retries of
// the same value. The caller owns the store and must close it only after Close.
type HighWaterStore interface {
	LoadReservedThrough(common.EntityKind) (uint64, error)
	ReserveThrough(common.EntityKind, uint64) error
}

type slot struct {
	mu     sync.Mutex
	issued atomic.Uint64
	limit  atomic.Uint64
}

// Allocator is owned by one open database. Never open two allocators on the
// same backing store simultaneously; the database's exclusive lock enforces
// single-process ownership.
type Allocator struct {
	store HighWaterStore
	slots [common.EntityKindCount + 1]slot
	// The top bit rejects new admissions; the remaining bits count active Next
	// calls. Close waits for all admitted calls before the store may be closed.
	state atomic.Uint64
}

const closing = uint64(1) << 63

// Open loads durable reservations. Even if the previous process issued no
// IDs, every existing reservation is discarded on reopen.
func Open(store HighWaterStore) (*Allocator, error) {
	if store == nil {
		return nil, errors.New("idalloc: nil high-water store")
	}
	a := &Allocator{store: store}
	for n := 1; n <= common.EntityKindCount; n++ {
		high, err := store.LoadReservedThrough(common.EntityKind(n))
		if err != nil {
			return nil, fmt.Errorf("idalloc: load %s: %w", common.EntityKind(n), err)
		}
		a.slots[n].issued.Store(high)
		a.slots[n].limit.Store(high)
	}
	return a, nil
}

// Next returns one ID. A failed call consumes no ID; a reservation whose
// durable write succeeded but reported an error may instead leave a gap.
// The steady-state path uses only atomic operations, without locks or I/O.
func (a *Allocator) Next(kind common.EntityKind) (common.ValueID, error) {
	if !kind.Valid() {
		return 0, ErrKind
	}
	for {
		state := a.state.Load()
		if state&closing != 0 {
			return 0, ErrClosed
		}
		if a.state.CompareAndSwap(state, state+1) {
			break
		}
	}
	defer a.state.Add(^uint64(0))
	s := &a.slots[kind]
	for {
		issued := s.issued.Load()
		if issued < s.limit.Load() {
			if s.issued.CompareAndSwap(issued, issued+1) {
				return common.ValueID(issued + 1), nil
			}
			continue
		}
		if issued == math.MaxUint64 {
			return 0, ErrExhausted
		}
		s.mu.Lock()
		// Only the goroutine finding an exhausted block reserves the next.
		if s.issued.Load() == s.limit.Load() {
			high := s.limit.Load()
			newHigh := high + BlockSize
			if newHigh < high {
				newHigh = math.MaxUint64
			}
			if err := a.store.ReserveThrough(kind, newHigh); err != nil {
				s.mu.Unlock()
				return 0, fmt.Errorf("idalloc: reserve %s: %w", kind, err)
			}
			s.limit.Store(newHigh)
		}
		s.mu.Unlock()
	}
}

// SkipThrough reconciles a caller-supplied canonical ID with this allocator.
// IDs <= id that have not yet been issued become gaps. If id lies beyond the
// current reservation, a synced high-water advance precedes publication.
// This is for legacy/external commits, not a substitute for Next: a caller
// supplying an ID already handed to another goroutine cannot claim it safely.
func (a *Allocator) SkipThrough(kind common.EntityKind, id common.ValueID) error {
	if !kind.Valid() || id == 0 {
		return ErrKind
	}
	for {
		state := a.state.Load()
		if state&closing != 0 {
			return ErrClosed
		}
		if a.state.CompareAndSwap(state, state+1) {
			break
		}
	}
	defer a.state.Add(^uint64(0))
	s := &a.slots[kind]
	s.mu.Lock()
	defer s.mu.Unlock()
	if uint64(id) > s.limit.Load() {
		if err := a.store.ReserveThrough(kind, uint64(id)); err != nil {
			return fmt.Errorf("idalloc: reconcile %s: %w", kind, err)
		}
		s.limit.Store(uint64(id))
	}
	for issued := s.issued.Load(); uint64(id) > issued; issued = s.issued.Load() {
		if s.issued.CompareAndSwap(issued, uint64(id)) {
			break
		}
	}
	return nil
}

// Close rejects new calls and waits for admitted allocations (including
// reservations) to finish. It does not close the caller-owned store.
func (a *Allocator) Close() error {
	for {
		state := a.state.Load()
		if state&closing != 0 || a.state.CompareAndSwap(state, state|closing) {
			break
		}
	}
	for a.state.Load() != closing {
		runtime.Gosched()
	}
	return nil
}
