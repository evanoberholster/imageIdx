package timeidx

// Snapshot returns a read-only handle pinned to the currently published time
// snapshot. A writer may publish a newer snapshot while the handle is in use;
// the pinned shards and their backing mapping remain valid until
// ReleaseSnapshot is called. The caller must call ReleaseSnapshot.
func (i *Index) Snapshot() *Index {
	if i == nil {
		return nil
	}
	tracker := i.readerTracker()
	token := tracker.Acquire()
	if i.isClosed() {
		tracker.Release(token)
		handle := &Index{namespace: i.namespace, flags: i.flags, offset: i.offset, config: i.config}
		handle.closed.Store(true)
		handle.view.Store(&snapshot{generation: 1})
		return handle
	}
	view := i.view.Load()
	handle := &Index{
		namespace:       i.namespace,
		flags:           i.flags,
		offset:          i.offset,
		config:          i.config,
		path:            i.path,
		snapshotReaders: tracker,
		snapshotToken:   token,
	}
	handle.view.Store(view)
	return handle
}

// ReleaseSnapshot releases the reader pin acquired by Snapshot. It is a no-op
// for handles that did not come from Snapshot and is safe to call once.
func (i *Index) ReleaseSnapshot() {
	if i == nil || i.snapshotReaders == nil {
		return
	}
	i.snapshotOnce.Do(func() {
		i.snapshotReaders.Release(i.snapshotToken)
	})
}
