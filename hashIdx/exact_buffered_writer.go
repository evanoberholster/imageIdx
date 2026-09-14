package hashidx

import (
	"sync"
	"time"
)

// ExactBufferedWriter accumulates ExactIndex mutations on a channel and
// flushes them periodically or when a size threshold is reached.  It is
// intended to reduce the number of WAL append + publish cycles when many
// small Apply calls arrive in quick succession.
type ExactBufferedWriter struct {
	idx   *ExactIndex
	ch    chan Mutation
	bat   int // flush when len(buf) >= bat
	tick  *time.Ticker
	stop  chan struct{}
	mu    sync.Mutex
	buf   []Mutation // protected by mu
	stats struct {
		flushes uint64
		entries uint64
	}
	wg sync.WaitGroup
}

// NewExactBufferedWriter returns a writer that flushes when either `bat`
// mutations have been accumulated or `interval` has elapsed, whichever comes
// first.
func NewExactBufferedWriter(idx *ExactIndex, bat int, interval time.Duration) *ExactBufferedWriter {
	bw := &ExactBufferedWriter{
		idx:  idx,
		ch:   make(chan Mutation, 256),
		bat:  bat,
		tick: time.NewTicker(interval),
		stop: make(chan struct{}),
	}
	bw.wg.Add(1)
	go bw.worker()
	return bw
}

// Apply sends a mutation into the buffer.  It blocks until the channel has
// space (channel size is 256, so callers should not send massive bursts
// without draining).
func (bw *ExactBufferedWriter) Apply(m Mutation) error {
	select {
	case bw.ch <- m:
		return nil
	default:
		return ErrChannelFull
	}
}

// Flush forces an immediate flush of the buffered mutations.
func (bw *ExactBufferedWriter) Flush() error {
	bw.mu.Lock()
	mups := bw.buf
	bw.buf = nil
	bw.mu.Unlock()

	if len(mups) == 0 {
		return nil
	}

	// Merge all batched mutations into one.
	var allAdds []ExactEntry
	var allRemoves []ExactEntry
	for _, m := range mups {
		allAdds = append(allAdds, m.Add...)
		allRemoves = append(allRemoves, m.Remove...)
	}

	if len(allAdds) == 0 && len(allRemoves) == 0 {
		return nil
	}

	// Canonicalize the merged batch.
	adds, removes := canonicalizeMutation(bw.idx.view.Load(), Mutation{Add: allAdds, Remove: allRemoves})

	if err := bw.idx.AppendWAL(adds, removes); err != nil {
		// On error we re‑queue the slices so they are not lost.
		bw.mu.Lock()
		bw.buf = mups
		bw.mu.Unlock()
		return err
	}
	bw.stats.flushes++
	bw.stats.entries += uint64(len(allAdds) + len(allRemoves))
	return nil
}

// Stats returns the total number of flushes and entries flushed since the
// writer was created.
func (bw *ExactBufferedWriter) Stats() (flushes uint64, entries uint64) {
	bw.mu.Lock()
	defer bw.mu.Unlock()
	return bw.stats.flushes, bw.stats.entries
}

// Close stops the writer goroutine and drains any remaining entries.
func (bw *ExactBufferedWriter) Close() error {
	close(bw.stop)
	bw.wg.Wait()
	return bw.Flush()
}

func (bw *ExactBufferedWriter) worker() {
	defer bw.wg.Done()
	for {
		select {
		case m := <-bw.ch:
			bw.mu.Lock()
			bw.buf = append(bw.buf, m)
			if len(bw.buf) >= bw.bat {
				if err := bw.Flush(); err != nil {
					// error already handled inside Flush; just reset.
				}
			}
			bw.mu.Unlock()
		case <-bw.tick.C:
			bw.mu.Lock()
			if len(bw.buf) > 0 {
				if err := bw.Flush(); err != nil {
					// silently ignore; next tick will try again.
				}
			}
			bw.mu.Unlock()
		case <-bw.stop:
			// drain remaining
			bw.mu.Lock()
			if len(bw.buf) > 0 {
				bw.Flush()
			}
			bw.mu.Unlock()
			return
		}
	}
}

// ---------------------------------------------------------------------------

// ApplyBatch applies a slice of mutations as a single batch to the exact
// index.  It merges all entries, canonicalizes once, and calls the index's
// internal WAL append + publish path once, so the whole batch publishes
// once rather than once per slice.
// ApplyBatch applies a slice of mutations as a single batch to the exact index.
// It merges all entries, canonicalizes once, and calls the internal WAL append
// + publish path once, so the whole batch publishes once rather than once per slice.
//
// This is useful for bulk operations where the caller already has the
// mutations collected and wants to minimise WAL/append overhead.
//
// The mutations slice is validated: each entry's ImageID must be non-zero.
// Canonicalization drops pairs whose desired state already matches the current
// view, and remove wins on conflict.
func (i *ExactIndex) ApplyBatch(mutations []Mutation) error {
	if len(mutations) == 0 {
		return nil
	}
	// Merge all slices, validating each entry.
	var allAdds []ExactEntry
	var allRemoves []ExactEntry
	for _, m := range mutations {
		for _, e := range m.Add {
			if e.ImageID == 0 {
				return ErrInvalidEntry
			}
			allAdds = append(allAdds, e)
		}
		for _, e := range m.Remove {
			if e.ImageID == 0 {
				return ErrInvalidEntry
			}
			allRemoves = append(allRemoves, e)
		}
	}
	if len(allAdds) == 0 && len(allRemoves) == 0 {
		return nil
	}

	adds, removes := canonicalizeMutation(i.view.Load(), Mutation{Add: allAdds, Remove: allRemoves})

	if err := i.AppendWAL(adds, removes); err != nil {
		return err
	}
	next := applyEffective(i.view.Load(), adds, removes, i.config)
	next.generation = i.view.Load().generation + 1
	i.view.Store(next)
	i.generation.Store(next.generation)
	return nil
}
