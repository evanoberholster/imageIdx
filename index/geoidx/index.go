package geoidx

import (
	"cmp"
	"os"
	"slices"
	"sync"
	"sync/atomic"

	common "github.com/evanoberholster/imageIdx/common"
	"github.com/evanoberholster/imageIdx/internal/storage"
)

// Config configures an index.
type Config struct {
	// Namespace labels the collection for the higher-level search planner. It
	// has no effect on storage layout and is supplied again on Open.
	Namespace common.Namespace
	// MaxCells is the default cover cell budget for region queries that do not
	// specify one. Values <= 0 select DefaultMaxCells.
	MaxCells int
	// TargetDelta is the expected active-delta batch size; write-path scratch is
	// sized for it. Values <= 0 select optimizedBatchEntries (64).
	TargetDelta int
	// MaxDelta is the hard active-delta limit. When the delta reaches it, apply
	// folds the delta into the base synchronously so a caller that forgets to
	// Compact cannot degrade reads without bound. Values <= 0 select
	// defaultMaxDelta (4096).
	MaxDelta int
}

const (
	optimizedBatchEntries = 64
	defaultMaxDelta       = 4096
)

func (c Config) withDefaults() (Config, error) {
	if c.MaxCells < 0 {
		return Config{}, ErrInvalidConfig
	}
	if c.MaxCells == 0 {
		c.MaxCells = DefaultMaxCells
	}
	if c.TargetDelta <= 0 {
		c.TargetDelta = optimizedBatchEntries
	}
	if c.MaxDelta <= 0 {
		c.MaxDelta = defaultMaxDelta
	}
	if c.MaxDelta < c.TargetDelta {
		return Config{}, ErrInvalidConfig
	}
	return c, nil
}

// mutation is one insert or delete. id is authoritative; key is the asserted
// location. Mutations are kept sorted by id and never mutated after publication.
type mutation struct {
	id  common.ImageID
	key S2Key
	add bool
}

// base is the immutable sorted (key, imageID) representation. keys and ids are
// parallel and share an arena when mf is non-nil. maxID bounds the ImageID
// domain for the optional bitset accelerator.
type base struct {
	keys  []S2Key
	ids   []common.ImageID
	mf    *storage.MappedFile
	gen   uint64
	maxID common.ImageID
}

// view is one immutable reader snapshot. Its slices are never mutated after
// publication; a writer replaces the whole view.
type view struct {
	keys  []S2Key
	ids   []common.ImageID
	muts  []mutation
	maxID common.ImageID
}

// Index is a specialized geographic index: lock-free readers and one serialized
// writer. It is safe for concurrent use by any number of readers and one writer.
type Index struct {
	config Config

	mu      sync.Mutex
	view    atomic.Pointer[view]
	closed  atomic.Bool
	tracker storage.ReaderTracker

	// Writer-owned state, touched only under mu.
	base base
	muts []mutation

	persistent bool
	dir        string
	lock       *os.File
}

// NewMemory returns an empty heap-backed index.
func NewMemory(config Config) (*Index, error) {
	cfg, err := config.withDefaults()
	if err != nil {
		return nil, err
	}
	index := &Index{config: cfg}
	index.publishLocked()
	return index, nil
}

func (i *Index) publishLocked() {
	i.view.Store(&view{keys: i.base.keys, ids: i.base.ids, muts: i.muts, maxID: maxImageID(i.base.maxID, i.muts)})
}

// maxImageID returns the largest ImageID in the base bound or the mutation
// delta, used to size the optional bitset sort.
func maxImageID(baseMax common.ImageID, muts []mutation) common.ImageID {
	best := baseMax
	for n := range muts {
		if muts[n].id > best {
			best = muts[n].id
		}
	}
	return best
}

// withView runs fn while pinning the current view so Compact and Close cannot
// reclaim or unmap an arena the reader may still touch. The order is strict:
// Acquire -> check closed -> use view -> Release. Acquiring before checking
// closed makes Close's grace period wait for this reader.
func (i *Index) withView(fn func(*view)) {
	token := i.tracker.Acquire()
	if !i.closed.Load() {
		fn(i.view.Load())
	}
	i.tracker.Release(token)
}

// Insert records id at key.
func (i *Index) Insert(key S2Key, id common.ImageID) error {
	var ops [1]mutation
	ops[0] = mutation{id: id, key: key, add: true}
	return i.apply(ops[:])
}

// Delete removes id. key is the caller's asserted current location; because an
// ImageID has at most one location, the id is authoritative and a stale key does
// not prevent removal. Moving an image is Delete(oldKey, id) then
// Insert(newKey, id); the caller supplies oldKey from imagemetadata.
func (i *Index) Delete(key S2Key, id common.ImageID) error {
	var ops [1]mutation
	ops[0] = mutation{id: id, key: key, add: false}
	return i.apply(ops[:])
}

// InsertBatch records every entry as an insert.
func (i *Index) InsertBatch(entries []Entry) error {
	return i.applyEntries(entries, true)
}

// DeleteBatch removes every entry.
func (i *Index) DeleteBatch(entries []Entry) error {
	return i.applyEntries(entries, false)
}

// Apply records inserts and then deletes; a later operation for an ImageID wins.
func (i *Index) Apply(inserts, deletes []Entry) error {
	total := len(inserts) + len(deletes)
	var stack [2 * optimizedBatchEntries]mutation
	var ops []mutation
	if total <= len(stack) {
		ops = stack[:0]
	} else {
		ops = make([]mutation, 0, total)
	}
	for _, e := range inserts {
		ops = append(ops, mutation{id: e.ImageID, key: e.Key, add: true})
	}
	for _, e := range deletes {
		ops = append(ops, mutation{id: e.ImageID, key: e.Key, add: false})
	}
	return i.apply(ops)
}

// applyEntries stages an entry batch on the stack for the common <=64 case,
// falling back to a heap slice only for larger batches.
func (i *Index) applyEntries(entries []Entry, add bool) error {
	var stack [optimizedBatchEntries]mutation
	var ops []mutation
	if len(entries) <= len(stack) {
		ops = stack[:len(entries)]
	} else {
		ops = make([]mutation, len(entries))
	}
	for n, e := range entries {
		ops[n] = mutation{id: e.ImageID, key: e.Key, add: add}
	}
	return i.apply(ops)
}

func (i *Index) apply(ops []mutation) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.closed.Load() {
		return ErrClosed
	}
	if len(ops) == 0 {
		return nil
	}
	for _, op := range ops {
		if op.id == 0 {
			return ErrInvalidID
		}
		if _, err := CellIDFromKey(op.key); err != nil {
			return err
		}
	}
	switch {
	case len(ops) <= optimizedBatchEntries:
		next := make([]mutation, len(i.muts), len(i.muts)+len(ops))
		copy(next, i.muts)
		for _, op := range ops {
			next = upsertMutation(next, op)
		}
		i.muts = next
	default:
		// Large batch: sort once and merge, so building the delta is O(n+m)
		// instead of an O(n) insert per operation. ops is a fresh slice here,
		// so it is safe to reorder in place.
		if !slices.IsSortedFunc(ops, compareMutationID) {
			slices.SortStableFunc(ops, compareMutationID)
		}
		i.muts = mergeMutations(i.muts, lastWins(ops))
	}
	i.publishLocked()
	if len(i.muts) >= i.config.MaxDelta {
		return i.compactLocked()
	}
	return nil
}

func compareMutationID(a, b mutation) int { return cmp.Compare(a.id, b.id) }

// lastWins collapses a slice sorted by id to one mutation per id, keeping the
// last occurrence (the write path's later-operation-wins rule).
func lastWins(sorted []mutation) []mutation {
	if len(sorted) < 2 {
		return sorted
	}
	out := sorted[:1]
	for n := 1; n < len(sorted); n++ {
		if sorted[n].id == out[len(out)-1].id {
			out[len(out)-1] = sorted[n]
			continue
		}
		out = append(out, sorted[n])
	}
	return out
}

// mergeMutations merges two id-sorted mutation slices; ops (the new batch) wins
// on equal ids.
func mergeMutations(base, ops []mutation) []mutation {
	out := make([]mutation, 0, len(base)+len(ops))
	b, o := 0, 0
	for b < len(base) || o < len(ops) {
		switch {
		case o >= len(ops) || (b < len(base) && base[b].id < ops[o].id):
			out = append(out, base[b])
			b++
		case b >= len(base) || ops[o].id < base[b].id:
			out = append(out, ops[o])
			o++
		default:
			out = append(out, ops[o]) // ops wins
			b++
			o++
		}
	}
	return out
}

func upsertMutation(muts []mutation, op mutation) []mutation {
	index, found := mutationIndex(muts, op.id)
	if found {
		muts[index].key = op.key
		muts[index].add = op.add
		return muts
	}
	return slices.Insert(muts, index, op)
}

func mutationIndex(muts []mutation, id common.ImageID) (int, bool) {
	return slices.BinarySearchFunc(muts, id, func(m mutation, id common.ImageID) int {
		return cmp.Compare(m.id, id)
	})
}

// Generation returns the current logical generation. It advances on Compact.
func (i *Index) Generation() uint64 {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.base.gen
}

// Namespace returns the namespace configured for this index.
func (i *Index) Namespace() common.Namespace { return i.config.Namespace }

// Len returns the effective number of stored locations (base minus deletes plus
// inserts).
func (i *Index) Len() uint64 {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.closed.Load() {
		return 0
	}
	return i.effectiveCountLocked(i.view.Load())
}

func (i *Index) effectiveCountLocked(v *view) uint64 {
	count := uint64(0)
	for n := range v.keys {
		if _, ok := mutationIndex(v.muts, v.ids[n]); !ok {
			count++
		}
	}
	for n := range v.muts {
		if v.muts[n].add {
			count++
		}
	}
	return count
}
