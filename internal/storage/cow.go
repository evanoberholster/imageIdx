package storage

import (
	"cmp"
	"errors"
	"slices"

	"github.com/evanoberholster/imageIdx/internal/pagealloc"
)

// Extent is an index-owned allocation in a COW arena. Size is the payload
// length; pagealloc rounds it to allocation granularity.
type Extent struct {
	Ref  pagealloc.Ref
	Size uint32
}

// UnionExtents retains all recoverable allocations, deduplicating shared
// extents. The result is address-sorted and owns its storage; input sets are
// untouched. Conflicting sizes at the same address are corruption.
func UnionExtents(sets ...[]Extent) ([]Extent, error) {
	count := 0
	for _, set := range sets {
		count += len(set)
	}
	out := make([]Extent, 0, count)
	compare := func(a, b Extent) int { return cmp.Compare(a.Ref, b.Ref) }
	if len(sets) == 2 && slices.IsSortedFunc(sets[0], compare) && slices.IsSortedFunc(sets[1], compare) {
		// COW publishers can retain address-sorted generation lists. Merge
		// those without sorting an already ordered input again. The result
		// still owns its storage; validation/deduplication below is identical.
		a, b := sets[0], sets[1]
		for len(a) != 0 && len(b) != 0 {
			if a[0].Ref <= b[0].Ref {
				out = append(out, a[0])
				a = a[1:]
			} else {
				out = append(out, b[0])
				b = b[1:]
			}
		}
		out = append(out, a...)
		out = append(out, b...)
	} else {
		for _, set := range sets {
			out = append(out, set...)
		}
		slices.SortFunc(out, compare)
	}
	unique := out[:0]
	for _, e := range out {
		if e.Size == 0 {
			return nil, errors.New("storage: empty extent")
		}
		if len(unique) != 0 && unique[len(unique)-1].Ref == e.Ref {
			if unique[len(unique)-1].Size != e.Size {
				return nil, errors.New("storage: conflicting extents")
			}
			continue
		}
		unique = append(unique, e)
	}
	return unique, nil
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
