package timestamp

import (
	"runtime"
	"sync/atomic"
)

type readerEpoch struct {
	current atomic.Uint32
	readers [2]atomic.Int64
}

func (e *readerEpoch) enter() uint32 {
	for {
		epoch := e.current.Load() & 1
		e.readers[epoch].Add(1)
		if e.current.Load()&1 == epoch {
			return epoch
		}
		e.readers[epoch].Add(-1)
	}
}

func (e *readerEpoch) exit(epoch uint32) {
	e.readers[epoch&1].Add(-1)
}

// synchronize advances the reader epoch and waits only for readers that could
// have observed objects retired before the advance. There is one compactor in
// V1, so epoch advancement is deliberately single-owner.
func (e *readerEpoch) synchronize() {
	old := e.current.Load() & 1
	e.current.Store(old ^ 1)
	for e.readers[old].Load() != 0 {
		runtime.Gosched()
	}
}
