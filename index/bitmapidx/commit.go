package bitmapidx

import (
	"errors"
	"slices"

	common "github.com/evanoberholster/imageIdx/common"
)

// commitQueueCapacity bounds queued commit requests. Apply blocks when the
// queue is full, so bursty writers exert backpressure instead of growing
// memory without bound.
const commitQueueCapacity = 64

// commitReq is one unit of commit work. A request with a non-nil barrier
// is a durability marker: when it reaches the front of the queue, every
// batch queued before it is WAL-appended and published.
type commitReq struct {
	changes []stagedChange
	barrier chan error
}

// Barrier blocks until every batch queued before it is WAL-appended and
// published, then reports the first commit error since the previous
// barrier, if any. Under DurabilitySync a nil return means the batches
// are fsynced and durable. Barrier on an index that never served a write
// reports only the health state.
//
// Call Barrier before Close when queued batches must survive: Close
// commits what is queued, but Apply concurrent with Close may report
// ErrClosed.
func (i *NamespaceIndex) Barrier() error {
	if !i.commits.Started() {
		if i.closed.Load() {
			return ErrClosed
		}
		if i.owner != nil {
			i.owner.writeMu.Lock()
			err := i.owner.writable()
			i.owner.writeMu.Unlock()
			return err
		}
		return nil
	}
	res := make(chan error, 1)
	if !i.commits.Submit(commitReq{barrier: res}) {
		return ErrClosed
	}
	// A successful Submit is always committed, even across Close, so the
	// marker's answer is guaranteed.
	return <-res
}

// commitGroup is the commit.Queue callback: it is the single owner of the
// dictionary writer, the WAL append position, and view publication for
// live writes — the Pebble memtable-writer role. Readers never interact
// with it: publication stays one atomic view Store, so reads keep their
// zero-lock guarantee. Maintenance (Checkpoint, Compact, Vacuum)
// serializes through the same writerMu the group holds, exactly as with
// synchronous writes.
//
// Each group commits in FIFO order under a single lock hold: per batch it
// resolves the dictionary, composes the delta against the running view,
// and encodes the WAL frame; then it appends every frame, syncs once, and
// publishes the final view once. Barrier markers observe exactly the
// batches queued before them. A failed batch records its error for the
// next barrier and the group continues with the latest good view, so one
// bad batch never poisons its neighbors.
func (i *NamespaceIndex) commitGroup(group []commitReq) {
	if i.owner != nil {
		i.owner.writeMu.Lock()
		defer i.owner.writeMu.Unlock()
	}
	i.writerMu.Lock()
	defer i.writerMu.Unlock()
	if i.closed.Load() {
		i.failGroup(group, ErrClosed)
		return
	}
	if i.owner != nil {
		if err := i.owner.writable(); err != nil {
			i.failGroup(group, err)
			return
		}
	}
	current := i.view.Load()
	groupStart := i.dict
	groupStart.chunks = slices.Clone(groupStart.chunks)
	type frame struct {
		payload []byte
		groups  uint32
		updates uint32
	}
	var frames []frame
	rotated := false
	var rotatedFrozen *namespaceDelta
	dirty := false
	// Markers are answered only after the group's outcome is known: an
	// early answer could report success for batches that later fail.
	// Each marker observes the batches queued before it; reported errors
	// are consumed, so only errors after the last marker latch for the
	// next barrier. A group failure reports to every marker directly.
	type barrierResp struct {
		waiter chan error
		snap   error
	}
	var pending []barrierResp
	firstErr := i.barrierErr
	i.barrierErr = nil
	fail := func(req commitReq, err error) {
		if firstErr == nil {
			firstErr = err
		}
	}
	for _, req := range group {
		if req.barrier != nil {
			pending = append(pending, barrierResp{waiter: req.barrier, snap: firstErr})
			firstErr = nil
			continue
		}
		saved := i.dict
		saved.chunks = slices.Clone(saved.chunks)
		resolved, payload, updates, err := i.resolveBatch(current, req.changes, false)
		if err != nil {
			i.dict = saved
			i.dict.abort()
			fail(req, err)
			continue
		}
		active := composeNamespaceDelta(current, resolved)
		if active == nil && current.active == nil {
			continue // every change already matched published state
		}
		base, frozen, activeNext, rotate := i.planRotation(current, active)
		i.dict.seal()
		current = i.nextView(current, base, frozen, activeNext)
		if rotate {
			rotated = true
			rotatedFrozen = frozen
		}
		if i.owner != nil && updates > 0 {
			frames = append(frames, frame{payload: payload, groups: uint32(len(resolved)), updates: updates})
		}
		dirty = true
	}
	if i.owner != nil && len(frames) != 0 {
		for _, f := range frames {
			if err := i.owner.appendFrame(i, f.payload, f.groups, f.updates, false); err != nil {
				i.dict = groupStart
				i.dict.abort()
				i.failGroup(group, err)
				return
			}
		}
		if i.owner.durability == common.DurabilitySync {
			if err := i.owner.wal.Sync(); err != nil {
				i.owner.failed = errors.Join(ErrRecoveryRequired, err)
				i.dict = groupStart
				i.dict.abort()
				i.failGroup(group, i.owner.failed)
				return
			}
		}
	}
	if dirty {
		i.view.Store(current)
		if rotated && current.frozen == rotatedFrozen {
			i.scheduleCompaction(rotatedFrozen)
		}
	}
	for _, p := range pending {
		p.waiter <- p.snap
	}
	i.barrierErr = firstErr
}

// failGroup reports err to every request in group: barriers observe it
// directly, batches latch it for the next barrier.
func (i *NamespaceIndex) failGroup(group []commitReq, err error) {
	for _, req := range group {
		if req.barrier != nil {
			req.barrier <- err
			continue
		}
		if i.barrierErr == nil {
			i.barrierErr = err
		}
	}
}
