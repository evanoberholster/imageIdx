package bitmapidx

import (
	"runtime"
	"sync"
	"sync/atomic"
)

// Epoch provides query-wide RCU-style reader tracking. A collection shares one
// Epoch across all of its indexes, so an AND/OR query enters and exits once.
type Epoch struct {
	current atomic.Uint32
	readers [2]atomic.Int64
	syncMu  sync.Mutex
}

type ReadGuard struct {
	epoch *Epoch
	slot  uint32
}

func NewEpoch() *Epoch { return &Epoch{} }

func (e *Epoch) Enter() ReadGuard {
	if e == nil {
		return ReadGuard{}
	}
	for {
		slot := e.current.Load() & 1
		e.readers[slot].Add(1)
		if e.current.Load()&1 == slot {
			return ReadGuard{epoch: e, slot: slot}
		}
		e.readers[slot].Add(-1)
	}
}

func (g ReadGuard) Exit() {
	if g.epoch != nil {
		g.epoch.readers[g.slot&1].Add(-1)
	}
}

// Synchronize advances the epoch and waits for readers which could still hold
// retired views or mmap slices. Concurrent reclaimers are serialized.
func (e *Epoch) Synchronize() {
	if e == nil {
		return
	}
	e.syncMu.Lock()
	old := e.current.Load() & 1
	e.current.Store(old ^ 1)
	for e.readers[old].Load() != 0 {
		runtime.Gosched()
	}
	e.syncMu.Unlock()
}
