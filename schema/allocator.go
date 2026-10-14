package schema

import (
	"math"

	"github.com/evanoberholster/imageIdx/common"
)

// NextID issues one ID. Uniqueness is ordered by the successful CAS, not the
// completion order of concurrent goroutines.
func (s *Schema) NextID(ns common.Namespace) (common.ValueID, error) {
	return s.NextIDs(ns, 1)
}

// NextIDs atomically issues the contiguous interval [start, start+count).
// On any error it publishes no part of that interval. A failed reservation
// may nevertheless leave an unused durable block (which is safe to skip).
func (s *Schema) NextIDs(ns common.Namespace, count uint64) (common.ValueID, error) {
	if count == 0 {
		return 0, ErrCount
	}
	a := s.entry(ns)
	if a == nil {
		return 0, ErrNamespace
	}
	if err := s.admit(); err != nil {
		return 0, err
	}
	defer s.release()
	for {
		issued := a.issued.Load()
		if count > math.MaxUint64-issued {
			return 0, ErrExhausted
		}
		if count <= a.limit.Load()-issued {
			if a.issued.CompareAndSwap(issued, issued+count) {
				return issued + 1, nil
			}
			continue
		}
		a.mu.Lock()
		if s.poisoned.Load() {
			a.mu.Unlock()
			return 0, ErrCorrupt
		}
		issued = a.issued.Load()
		if count > math.MaxUint64-issued {
			a.mu.Unlock()
			return 0, ErrExhausted
		}
		if issued+count > a.limit.Load() {
			high := reservationEnd(a.limit.Load(), issued+count, a.block)
			if err := s.persist(a, allocationRecord{a.record.generation + 1, high, 0, false}); err != nil {
				a.mu.Unlock()
				return 0, err
			}
			a.limit.Store(high) // publish only after the synced slot
		}
		a.mu.Unlock()
	}
}

func reservationEnd(old, required, block uint64) uint64 {
	if required <= old {
		return old
	}
	blocks := 1 + (required-old-1)/block
	if blocks > (math.MaxUint64-old)/block {
		return math.MaxUint64
	}
	return old + blocks*block
}

// EnsureMinimum advances the allocation floor. It linearizes at the CAS that
// raises issued: a concurrent allocation linearized before that CAS may
// return <= minimum (even if it finishes afterward); later allocations may
// not. This deliberately creates gaps for restoration/migration.
func (s *Schema) EnsureMinimum(ns common.Namespace, minimum common.ValueID) error {
	a := s.entry(ns)
	if a == nil {
		return ErrNamespace
	}
	if err := s.admit(); err != nil {
		return err
	}
	defer s.release()
	a.mu.Lock()
	defer a.mu.Unlock()
	if s.poisoned.Load() {
		return ErrCorrupt
	}
	if minimum > a.limit.Load() {
		high := reservationEnd(a.limit.Load(), minimum, a.block)
		if err := s.persist(a, allocationRecord{a.record.generation + 1, high, 0, false}); err != nil {
			return err
		}
		a.limit.Store(high)
	}
	for issued := a.issued.Load(); issued < minimum; issued = a.issued.Load() {
		if a.issued.CompareAndSwap(issued, minimum) {
			break
		}
	}
	return nil
}
