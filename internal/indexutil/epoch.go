// Package indexutil contains low-level mechanisms shared by the index
// implementations. It deliberately contains no domain types or format policy.
package indexutil

import (
	"runtime"
	"sync/atomic"
)

// ReaderEpoch is a two-slot reader counter. Synchronize must be serialized by
// its caller when more than one reclaimer can run concurrently.
type ReaderEpoch struct {
	current atomic.Uint32
	readers [2]atomic.Int64
}

func (e *ReaderEpoch) Enter() uint32 {
	for {
		epoch := e.current.Load() & 1
		e.readers[epoch].Add(1)
		if e.current.Load()&1 == epoch {
			return epoch
		}
		e.readers[epoch].Add(-1)
	}
}

func (e *ReaderEpoch) Exit(epoch uint32) {
	e.readers[epoch&1].Add(-1)
}

func (e *ReaderEpoch) Synchronize() {
	old := e.current.Load() & 1
	e.current.Store(old ^ 1)
	for e.readers[old].Load() != 0 {
		runtime.Gosched()
	}
}
