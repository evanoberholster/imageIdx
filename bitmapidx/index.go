package bitmapidx

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/evanoberholster/imageIdx/idset"
	"github.com/evanoberholster/imageIdx/internal/indexutil"
)

// Index is one logical bitmap predicate: an immutable set of ImageIDs with a
// stable Key and IndexID.
//
// Concurrency is split along the read/write boundary. Readers load the single
// atomically published indexView and operate on immutable data only, taking no
// locks. Writers serialize through writerMu and publish replacement views;
// background compaction owns the Frozen->Base fold.
type Index struct {
	view    atomic.Pointer[indexView]
	readers *indexutil.ReaderTracker

	writerMu sync.Mutex // serializes Apply, Compact, and compaction publication
	config   Config
	closed   atomic.Bool // set once in Close; readers and writers check it
	workers  sync.WaitGroup
	pending  atomic.Int64 // in-flight background compactions
}

// New constructs an index from its stable metadata and initial IDs.
func New(spec Spec, config Config) (*Index, error) {
	config, err := config.withDefaults()
	if err != nil {
		return nil, err
	}
	return newIndex(spec, config, &indexutil.ReaderTracker{})
}

func newIndex(spec Spec, config Config, readers *indexutil.ReaderTracker) (*Index, error) {
	if spec.Key.Type == 0 || readers == nil {
		return nil, ErrInvalidIndex
	}
	base := idset.New(spec.IDs)
	idx := &Index{config: config, readers: readers}
	idx.view.Store(&indexView{
		version:     bitmapFormatVersion,
		indexType:   spec.Key.Type,
		flags:       spec.Flags,
		indexID:     spec.IndexID,
		valueID:     spec.Key.ValueID,
		cardinality: base.Cardinality(),
		generation:  1,
		base:        base,
	})
	return idx, nil
}

func newIndexFromView(view *indexView, config Config, readers *indexutil.ReaderTracker) (*Index, error) {
	if view == nil || view.version != bitmapFormatVersion || view.indexType == 0 || !view.base.Valid() {
		return nil, ErrInvalidIndex
	}
	idx := &Index{config: config, readers: readers}
	idx.view.Store(view)
	return idx, nil
}

// Metadata returns the immutable public portion of the current snapshot.
func (i *Index) Metadata() Metadata {
	v := i.view.Load()
	return Metadata{
		Key:             Key{Type: v.indexType, ValueID: v.valueID},
		IndexID:         v.indexID,
		Flags:           v.flags,
		BaseCardinality: v.cardinality,
		Generation:      v.generation,
	}
}

// PendingCompactions reports the number of in-flight background compactions.
func (i *Index) PendingCompactions() int64 { return i.pending.Load() }

// WaitIdle blocks until all background compactions have drained or ctx is
// cancelled.
func (i *Index) WaitIdle(ctx context.Context) error {
	ticker := time.NewTicker(100 * time.Microsecond)
	defer ticker.Stop()
	for i.pending.Load() != 0 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
	return nil
}

// Contains reports whether id is a member of the effective set. It registers
// the read internally, loads one snapshot, and allocates nothing.
func (i *Index) Contains(id ImageID) bool {
	token := i.readers.Acquire()
	view := i.view.Load()
	present := !i.closed.Load() && view.contains(id)
	i.readers.Release(token)
	return present
}

// AppendTo appends the index's effective IDs to dst in ascending order. The
// caller owns dst; delta-free indexes stream the Base with no allocation.
func (i *Index) AppendTo(dst []ImageID) []ImageID {
	token := i.readers.Acquire()
	if !i.closed.Load() {
		dst = i.view.Load().appendTo(dst)
	}
	i.readers.Release(token)
	return dst
}

// Apply atomically publishes one mutation. Readers observe either the complete
// previous state or the complete new state, never a partial batch.
//
// The write is routed through four cases that bound the view to Base + Frozen
// + Active without ever growing an arbitrary delta chain:
//
//  1. The combined Active delta would exceed HardDeltaLimit: the batch is
//     large enough that folding everything into a replacement Base is cheaper,
//     so the effective bitmap is rewritten and published with no deltas.
//  2. A Frozen delta already exists and Active would reach the hard limit:
//     the Frozen delta is first folded into the Base, then the batch is
//     re-evaluated against the freshly published view.
//  3. Active reaches ActiveTarget: it is rotated to Frozen and a background
//     compaction is scheduled to fold it into the Base.
//  4. Otherwise the batch simply extends the Active delta.
func (i *Index) Apply(mutation Mutation) error {
	if len(mutation.Add) == 0 && len(mutation.Remove) == 0 {
		return nil
	}
	i.writerMu.Lock()
	defer i.writerMu.Unlock()
	if i.closed.Load() {
		return ErrClosed
	}

	current := i.view.Load()
	active := composeActive(current, mutation)
	if active != nil && active.len() > i.config.HardDeltaLimit {
		// Case 1: very large batch. Rewrite the Base; the replacement is still
		// published once and the old Base remains immutable for readers.
		builder := current.effectiveBuilder()
		applyMutation(&builder, mutation)
		i.publishLocked(current, builder.Freeze(), nil, nil)
		return nil
	}

	if current.frozen != nil && active != nil && active.len() >= i.config.HardDeltaLimit {
		// Case 2: a Frozen delta is already pending and Active is at the hard
		// limit. Fold Frozen into the Base, then recompute Active.
		base := compactBase(current.base, current.frozen)
		current = i.publishLocked(current, base, nil, current.active)
		active = composeActive(current, mutation)
	}

	if current.frozen == nil && active != nil && active.len() >= i.config.ActiveTarget {
		// Case 3: rotate Active to Frozen and compact it in the background.
		i.publishLocked(current, current.base, active, nil)
		i.scheduleCompaction(active)
		return nil
	}

	// Case 4: extend the Active delta.
	i.publishLocked(current, current.base, current.frozen, active)
	return nil
}

// publishLocked builds the replacement view and publishes it with one atomic
// store. It must be called with writerMu held. The returned view shares
// metadata with old but links the supplied layers.
func (i *Index) publishLocked(old *indexView, base idset.Ref, frozen, active *bitmapDelta) *indexView {
	next := &indexView{
		version:     old.version,
		indexType:   old.indexType,
		flags:       old.flags,
		indexID:     old.indexID,
		valueID:     old.valueID,
		cardinality: base.Cardinality(),
		generation:  old.generation + 1,
		base:        base,
		frozen:      frozen,
		active:      active,
	}
	i.view.Store(next)
	return next
}

// composeActive canonicalizes the mutation against the current view, producing
// the replacement Active delta. Conflicting operations within the batch are
// resolved (removal wins), and IDs whose desired state already matches the
// underlying Frozen/Base layers are dropped, so the delta stays minimal.
func composeActive(view *indexView, mutation Mutation) *bitmapDelta {
	states := make(map[ImageID]bool, len(mutation.Add)+len(mutation.Remove)+deltaLen(view.active))
	if view.active != nil {
		for _, id := range view.active.additions.ToArray() {
			states[id] = true
		}
		for _, id := range view.active.removals.ToArray() {
			states[id] = false
		}
	}
	for _, id := range mutation.Add {
		states[id] = true
	}
	for _, id := range mutation.Remove {
		states[id] = false
	}

	additions := make([]ImageID, 0, len(states))
	removals := make([]ImageID, 0, len(states))
	for id, desired := range states {
		underlying := containsWithoutActive(view, id)
		if desired == underlying {
			continue
		}
		if desired {
			additions = append(additions, id)
		} else {
			removals = append(removals, id)
		}
	}
	return newBitmapDelta(additions, removals)
}

// containsWithoutActive reports id's membership below the Active layer, i.e.
// in Frozen or Base.
func containsWithoutActive(view *indexView, id ImageID) bool {
	if present, ok := view.frozen.state(id); ok {
		return present
	}
	return view.base.Contains(id)
}

func deltaLen(delta *bitmapDelta) int {
	if delta == nil {
		return 0
	}
	return delta.len()
}

// applyMutation applies raw additions and removals to a Builder in place.
func applyMutation(builder *idset.Builder, mutation Mutation) {
	if len(mutation.Add) != 0 {
		builder.AddMany(mutation.Add)
	}
	for _, id := range mutation.Remove {
		builder.Remove(id)
	}
}

// compactBase folds a delta into a replacement Base without mutating either
// input.
func compactBase(base idset.Ref, delta *bitmapDelta) idset.Ref {
	builder := idset.Clone(base)
	applyDelta(&builder, delta)
	return builder.Freeze()
}

// scheduleCompaction folds the Frozen delta into the Base on a background
// goroutine. It publishes only if the Frozen delta is still current when it
// finishes, then waits for prior readers so retired layers can be reclaimed.
func (i *Index) scheduleCompaction(frozen *bitmapDelta) {
	i.workers.Add(1)
	i.pending.Add(1)
	go func() {
		defer i.workers.Done()
		defer i.pending.Add(-1)
		if i.config.CompactDelay != 0 {
			time.Sleep(i.config.CompactDelay)
		}
		initial := i.view.Load()
		if initial.frozen != frozen {
			return
		}
		base := compactBase(initial.base, frozen)

		i.writerMu.Lock()
		current := i.view.Load()
		if current.frozen == frozen && !i.closed.Load() {
			i.publishLocked(current, base, nil, current.active)
		}
		i.writerMu.Unlock()
		i.readers.WaitForPriorReaders()
	}()
}

// Compact synchronously merges both deltas into a replacement immutable Base,
// publishing a view with no correction layers.
func (i *Index) Compact() error {
	i.writerMu.Lock()
	if i.closed.Load() {
		i.writerMu.Unlock()
		return ErrClosed
	}
	current := i.view.Load()
	base := current.effectiveBitmap()
	if current.frozen != nil || current.active != nil {
		i.publishLocked(current, base, nil, nil)
	}
	i.writerMu.Unlock()
	i.readers.WaitForPriorReaders()
	return nil
}

// Close marks the index closed, waits for background compactions to drain, and
// waits for prior readers. Further Apply calls return ErrClosed.
func (i *Index) Close() error {
	i.writerMu.Lock()
	i.closed.Store(true)
	i.writerMu.Unlock()
	i.workers.Wait()
	i.readers.WaitForPriorReaders()
	return nil
}
