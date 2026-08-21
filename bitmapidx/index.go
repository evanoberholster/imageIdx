package bitmapidx

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dgraph-io/sroar"
)

type BitmapMutationOp struct {
	ImageID ImageID
	Add     bool
}

// BitmapWriterState is owned exclusively by BitmapIndex.writerMu. Pending is
// useful to queue/shard integrations; public Write calls canonicalize directly
// and publish exactly once before returning.
type BitmapWriterState struct {
	Pending      []BitmapMutationOp
	PendingCount uint32
	LastPublish  int64
}

type BitmapIndex struct {
	view atomic.Pointer[BitmapIndexView]

	writerMu sync.Mutex
	writer   BitmapWriterState
	config   Config
	closed   atomic.Bool
	workers  sync.WaitGroup
	pending  atomic.Int64
}

func NewBitmapIndex(key BitmapKey, indexID uint64, ids []uint64, config Config) (*BitmapIndex, error) {
	config, err := config.withDefaults()
	if err != nil {
		return nil, err
	}
	if key.Type == 0 {
		return nil, ErrInvalidIndex
	}
	base := NewBitmapRef(ids)
	idx := &BitmapIndex{config: config}
	idx.writer.LastPublish = time.Now().UnixNano()
	idx.view.Store(&BitmapIndexView{
		Version:     BitmapFormatVersion,
		IndexType:   key.Type,
		IndexID:     indexID,
		ValueID:     key.ValueID,
		Cardinality: base.Cardinality(),
		Generation:  1,
		Base:        base,
	})
	return idx, nil
}

// New is a compact alias for NewBitmapIndex.
func New(key BitmapKey, indexID uint64, ids []uint64, config Config) (*BitmapIndex, error) {
	return NewBitmapIndex(key, indexID, ids, config)
}

func newBitmapIndexFromView(view *BitmapIndexView, config Config) (*BitmapIndex, error) {
	config, err := config.withDefaults()
	if err != nil {
		return nil, err
	}
	if view == nil || view.Version != BitmapFormatVersion || view.IndexType == 0 || !view.Base.valid() {
		return nil, ErrInvalidIndex
	}
	idx := &BitmapIndex{config: config}
	idx.writer.LastPublish = time.Now().UnixNano()
	idx.view.Store(view)
	return idx, nil
}

func (i *BitmapIndex) LoadView() *BitmapIndexView { return i.view.Load() }

func (i *BitmapIndex) Key() BitmapKey {
	v := i.view.Load()
	return BitmapKey{Type: v.IndexType, ValueID: v.ValueID}
}

func (i *BitmapIndex) IndexID() uint64           { return i.view.Load().IndexID }
func (i *BitmapIndex) Generation() uint64        { return i.view.Load().Generation }
func (i *BitmapIndex) BaseCardinality() uint64   { return i.view.Load().Cardinality }
func (i *BitmapIndex) PendingCompactions() int64 { return i.pending.Load() }

func (i *BitmapIndex) WaitIdle(ctx context.Context) error {
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

func (i *BitmapIndex) Contains(id uint64) bool {
	guard := i.config.Epoch.Enter()
	view := i.view.Load()
	present := !i.closed.Load() && view.Contains(id)
	guard.Exit()
	return present
}

func (i *BitmapIndex) Search(dst []ImageID) []ImageID {
	guard := i.config.Epoch.Enter()
	if !i.closed.Load() {
		for _, id := range i.view.Load().effectiveBitmap().ToArray() {
			dst = append(dst, ImageID(id))
		}
	}
	guard.Exit()
	return dst
}

func (i *BitmapIndex) Add(ids []uint64) error {
	return i.Write(BitmapMutation{Additions: ids})
}

func (i *BitmapIndex) Remove(ids []uint64) error {
	return i.Write(BitmapMutation{Removals: ids})
}

// Write atomically publishes the entire mutation. If an ID occurs in both
// slices, removal wins, which makes retry/cancellation behavior deterministic.
func (i *BitmapIndex) Write(mutation BitmapMutation) error {
	if len(mutation.Additions) == 0 && len(mutation.Removals) == 0 {
		return nil
	}
	i.writerMu.Lock()
	defer i.writerMu.Unlock()
	if i.closed.Load() {
		return ErrClosed
	}

	current := i.view.Load()
	active := composeActive(current, mutation)
	if active != nil && active.Len() > i.config.HardDeltaLimit {
		// Very large batches take the benchmark-selected Base rewrite path. The
		// replacement is still published once and the old Base remains immutable.
		base := current.effectiveBitmap()
		applyMutation(base.bitmap, mutation)
		i.publishLocked(current, base, nil, nil)
		return nil
	}

	if current.Frozen != nil && active != nil && active.Len() >= i.config.HardDeltaLimit {
		base := compactBase(current.Base, current.Frozen)
		current = i.publishLocked(current, base, nil, current.Active)
		active = composeActive(current, mutation)
	}

	if current.Frozen == nil && active != nil && active.Len() >= i.config.ActiveTarget {
		next := i.publishLocked(current, current.Base, active, nil)
		i.scheduleCompaction(active)
		_ = next
		return nil
	}
	i.publishLocked(current, current.Base, current.Frozen, active)
	return nil
}

func (i *BitmapIndex) publishLocked(old *BitmapIndexView, base BitmapRef, frozen, active *BitmapDelta) *BitmapIndexView {
	next := &BitmapIndexView{
		Version:     old.Version,
		IndexType:   old.IndexType,
		Flags:       old.Flags,
		IndexID:     old.IndexID,
		ValueID:     old.ValueID,
		Cardinality: base.Cardinality(),
		Generation:  old.Generation + 1,
		Base:        base,
		Frozen:      frozen,
		Active:      active,
	}
	i.view.Store(next)
	i.writer.LastPublish = time.Now().UnixNano()
	return next
}

func composeActive(view *BitmapIndexView, mutation BitmapMutation) *BitmapDelta {
	states := make(map[uint64]bool, len(mutation.Additions)+len(mutation.Removals)+deltaLen(view.Active))
	if view.Active != nil {
		for _, id := range view.Active.Additions.ToArray() {
			states[id] = true
		}
		for _, id := range view.Active.Removals.ToArray() {
			states[id] = false
		}
	}
	for _, id := range mutation.Additions {
		states[id] = true
	}
	for _, id := range mutation.Removals {
		states[id] = false
	}

	additions := make([]uint64, 0, len(states))
	removals := make([]uint64, 0, len(states))
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
	return NewBitmapDelta(additions, removals)
}

func containsWithoutActive(view *BitmapIndexView, id uint64) bool {
	if present, ok := view.Frozen.state(id); ok {
		return present
	}
	return view.Base.Contains(id)
}

func deltaLen(delta *BitmapDelta) int {
	if delta == nil {
		return 0
	}
	return delta.Len()
}

func applyMutation(bitmap *sroar.Bitmap, mutation BitmapMutation) {
	if len(mutation.Additions) != 0 {
		bitmap.SetMany(mutation.Additions)
	}
	for _, id := range mutation.Removals {
		bitmap.Remove(id)
	}
}

func compactBase(base BitmapRef, delta *BitmapDelta) BitmapRef {
	result := cloneBitmap(base)
	applyDelta(result, delta)
	return BitmapRef{bitmap: result}
}

func (i *BitmapIndex) scheduleCompaction(frozen *BitmapDelta) {
	i.workers.Add(1)
	i.pending.Add(1)
	go func() {
		defer i.workers.Done()
		defer i.pending.Add(-1)
		if i.config.CompactDelay != 0 {
			time.Sleep(i.config.CompactDelay)
		}
		initial := i.view.Load()
		if initial.Frozen != frozen {
			return
		}
		base := compactBase(initial.Base, frozen)

		i.writerMu.Lock()
		current := i.view.Load()
		if current.Frozen == frozen && !i.closed.Load() {
			i.publishLocked(current, base, nil, current.Active)
		}
		i.writerMu.Unlock()
		i.config.Epoch.Synchronize()
	}()
}

// Compact synchronously merges both deltas into a replacement immutable Base.
func (i *BitmapIndex) Compact() error {
	i.writerMu.Lock()
	if i.closed.Load() {
		i.writerMu.Unlock()
		return ErrClosed
	}
	current := i.view.Load()
	base := current.effectiveBitmap()
	if current.Frozen != nil || current.Active != nil {
		i.publishLocked(current, base, nil, nil)
	}
	i.writerMu.Unlock()
	i.config.Epoch.Synchronize()
	return nil
}

func (i *BitmapIndex) Close() error {
	i.writerMu.Lock()
	i.closed.Store(true)
	i.writerMu.Unlock()
	i.workers.Wait()
	i.config.Epoch.Synchronize()
	return nil
}
