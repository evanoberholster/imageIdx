package bitmapidx

import (
	"sync"

	"github.com/evanoberholster/imageIdx/internal/storage"
)

// snapshotPin holds one reader token on the live index for the lifetime of a
// query snapshot. The token prevents compaction and Close from unmapping a
// namespaceView while a plan is still using an older view.
type snapshotPin struct {
	once    sync.Once
	tracker *storage.ReaderTracker
	token   uint32
}

func (p *snapshotPin) release() {
	if p == nil || p.tracker == nil {
		return
	}
	p.once.Do(func() { p.tracker.Release(p.token) })
}

// Snapshot returns a read-only handle pinned to the currently published
// namespace view. Query execution on the returned handle can run while writers
// publish newer views; the pinned view and its backing storage remain valid
// until ReleaseSnapshot is called. The caller must call ReleaseSnapshot.
func (i *Index) Snapshot() *Index {
	if i == nil || i.readers == nil {
		return i
	}
	token := i.readers.Acquire()
	if i.closed.Load() {
		i.readers.Release(token)
		handle := &Index{readers: i.readers, config: i.config}
		handle.closed.Store(true)
		handle.view.Store(&namespaceView{generation: 1})
		return handle
	}
	view := i.view.Load()
	handle := &Index{
		owner:    i.owner,
		readers:  i.readers,
		config:   i.config,
		analyzer: i.analyzer,
		snapshot: &snapshotPin{tracker: i.readers, token: token},
	}
	handle.view.Store(view)
	return handle
}

// ReleaseSnapshot releases the reader pin acquired by Snapshot. It is a no-op
// for handles that did not come from Snapshot and is safe to call once.
func (i *Index) ReleaseSnapshot() {
	if i != nil {
		i.snapshot.release()
	}
}
