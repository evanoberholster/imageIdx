package storage

import (
	"errors"

	"github.com/evanoberholster/imageIdx/internal/pagealloc"
)

// Extent is an index-owned allocation in a COW arena. Size is the payload
// length; pagealloc rounds it to allocation granularity.
type Extent struct {
	Ref  pagealloc.Ref
	Size uint32
}

// UnionExtents retains all recoverable allocations, deduplicating shared
// extents. Conflicting sizes at the same address are corruption.
func UnionExtents(sets ...[]Extent) ([]Extent, error) {
	seen := make(map[pagealloc.Ref]uint32)
	var out []Extent
	for _, set := range sets {
		for _, e := range set {
			if e.Size == 0 {
				return nil, errors.New("storage: empty extent")
			}
			if size, ok := seen[e.Ref]; ok {
				if size != e.Size {
					return nil, errors.New("storage: conflicting extents")
				}
				continue
			}
			seen[e.Ref] = e.Size
			out = append(out, e)
		}
	}
	return out, nil
}

// ReconstructAllocator reserves both superblocks and the union of recoverable
// extents. Runtime reader retention is a separate obligation of the caller.
func ReconstructAllocator(arenaBytes uint64, live []Extent) (*pagealloc.Allocator, error) {
	// Ref is uint32 and allocation capacities are also uint32.
	if arenaBytes/uint64(pagealloc.MinBlockSize) > 1<<32 {
		return nil, pagealloc.ErrTooLarge
	}
	a, err := pagealloc.New(arenaBytes)
	if err != nil {
		return nil, err
	}
	if err := a.Reserve(1, SuperblockSlotBytes); err != nil {
		return nil, err
	}
	for _, e := range live {
		if e.Size == 0 || e.Size > ^uint32(0)-(pagealloc.MinBlockSize-1) {
			return nil, pagealloc.ErrTooLarge
		}
		if err := a.Reserve(e.Ref, e.Size); err != nil {
			return nil, err
		}
	}
	return a, nil
}

type CommitOutcome uint8

const (
	CommitAborted CommitOutcome = iota
	CommitDurable
	CommitUncertain
)

// CommitSuperblock is the common final COW publication barrier. The caller
// must synchronize new payloads BEFORE calling it. A write or sync failure
// protects the candidate's extents until a successful overwrite or reopen.
// Callbacks permit index-local failure injection without global production hooks.
func CommitSuperblock(header SuperblockHeader, payload []byte, write func([]byte) error, sync func() error) (CommitOutcome, error) {
	var slot [SuperblockSlotBytes]byte
	if err := EncodeSuperblock(slot[:], header, payload); err != nil {
		return CommitAborted, err
	}
	if err := write(slot[:]); err != nil {
		return CommitUncertain, err
	}
	if err := sync(); err != nil {
		return CommitUncertain, err
	}
	return CommitDurable, nil
}
