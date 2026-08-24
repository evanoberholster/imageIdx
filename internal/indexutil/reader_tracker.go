// Package indexutil contains low-level mechanisms shared by the index
// implementations. It deliberately contains no domain types or format policy.
package indexutil

import (
	"runtime"
	"sync"
	"sync/atomic"
)

// ReaderTracker is a two-slot read-side lifetime tracker. Readers acquire a
// token before loading reclaimable state. WaitForPriorReaders advances the
// active slot and waits only for readers that could still hold the old state.
type ReaderTracker struct {
	current atomic.Uint32
	readers [2]atomic.Int64
	waitMu  sync.Mutex
}

func (tracker *ReaderTracker) Acquire() uint32 {
	for {
		slot := tracker.current.Load() & 1
		tracker.readers[slot].Add(1)
		if tracker.current.Load()&1 == slot {
			return slot
		}
		tracker.readers[slot].Add(-1)
	}
}

func (tracker *ReaderTracker) Release(token uint32) {
	tracker.readers[token&1].Add(-1)
}

// WaitForPriorReaders serializes concurrent reclaimers, changes the active
// reader slot, and waits for readers registered in the previous slot.
func (tracker *ReaderTracker) WaitForPriorReaders() {
	tracker.waitMu.Lock()
	old := tracker.current.Load() & 1
	tracker.current.Store(old ^ 1)
	for tracker.readers[old].Load() != 0 {
		runtime.Gosched()
	}
	tracker.waitMu.Unlock()
}
