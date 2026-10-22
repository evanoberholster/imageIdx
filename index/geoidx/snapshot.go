package geoidx

// Snapshot returns a read-only handle pinned to the currently published geo
// view. Writers may publish newer shard/mutation views while the handle is in
// use; the pinned view and its arena mapping remain valid until
// ReleaseSnapshot is called. The caller must call ReleaseSnapshot.
func (i *Index) Snapshot() *Index {
	if i == nil {
		return nil
	}
	tracker := i.readerTracker()
	token := tracker.Acquire()
	if i.closed.Load() {
		tracker.Release(token)
		handle := &Index{config: i.config}
		handle.closed.Store(true)
		handle.view.Store(&view{})
		return handle
	}
	handle := &Index{
		config:          i.config,
		persistent:      i.persistent,
		path:            i.path,
		snapshotTracker: tracker,
		snapshotToken:   token,
	}
	handle.view.Store(i.view.Load())
	return handle
}

// ReleaseSnapshot releases the reader pin acquired by Snapshot. It is a no-op
// for handles that did not come from Snapshot and is safe to call once.
func (i *Index) ReleaseSnapshot() {
	if i == nil || i.snapshotTracker == nil {
		return
	}
	i.snapshotOnce.Do(func() {
		i.snapshotTracker.Release(i.snapshotToken)
	})
}
