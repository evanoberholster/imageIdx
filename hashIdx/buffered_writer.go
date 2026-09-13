package hashidx

import (
	"sync"
	"time"
)

// BufferedWriter accumulates mutations on a channel and flushes them
// periodically or when a size threshold is reached.  It is intended to
// reduce the number of WAL append + publish cycles when many small
// Apply calls arrive in quick succession.
type BufferedWriter struct {
	idx   *Index
	ch    chan []Entry
	bat   int // flush when len(buf) >= bat
	tick  *time.Ticker
	stop  chan struct{}
	mu    sync.Mutex
	buf   [][]Entry // protected by mu
	stats struct {
		flushes uint64
		entries uint64
	}
	wg sync.WaitGroup
}

// NewBufferedWriter returns a writer that flushes when either `bat` entries
// have been accumulated or `interval` has elapsed, whichever comes first.
func NewBufferedWriter(idx *Index, bat int, interval time.Duration) *BufferedWriter {
	bw := &BufferedWriter{
		idx:  idx,
		ch:   make(chan []Entry, 256),
		bat:  bat,
		tick: time.NewTicker(interval),
		stop: make(chan struct{}),
	}
	bw.wg.Add(1)
	go bw.worker()
	return bw
}

// Apply sends an entry slice into the buffer.  It blocks until the channel
// has space (channel size is 256, so callers should not send massive bursts
// without draining).
func (bw *BufferedWriter) Apply(entries []Entry) error {
	select {
	case bw.ch <- entries:
		return nil
	default:
		// Channel full; callers may retry or drop.  Returning an error avoids
		// blocking the critical path unexpectedly.
		return ErrChannelFull
	}
}

// Flush forces an immediate flush of the buffered entries.
func (bw *BufferedWriter) Flush() error {
	bw.mu.Lock()
	entries := bw.buf
	bw.buf = nil
	bw.mu.Unlock()

	if len(entries) == 0 {
		return nil
	}

	// Merge all batched slices into one flat slice.
	var flat []Entry
	for _, s := range entries {
		flat = append(flat, s...)
	}
	if len(flat) == 0 {
		return nil
	}

	if err := bw.idx.Apply(flat); err != nil {
		// On error we re‑queue the slices so they are not lost.
		bw.mu.Lock()
		bw.buf = entries
		bw.mu.Unlock()
		return err
	}
	bw.stats.flushes++
	bw.stats.entries += uint64(len(flat))
	return nil
}

// Stats returns the total number of flushes and entries flushed since the
// writer was created.
func (bw *BufferedWriter) Stats() (flushes uint64, entries uint64) {
	bw.mu.Lock()
	defer bw.mu.Unlock()
	return bw.stats.flushes, bw.stats.entries
}

// Close stops the writer goroutine and drains any remaining entries.
func (bw *BufferedWriter) Close() error {
	close(bw.stop)
	bw.wg.Wait()
	return bw.Flush()
}

func (bw *BufferedWriter) worker() {
	defer bw.wg.Done()
	for {
		select {
		case entries := <-bw.ch:
			bw.mu.Lock()
			bw.buf = append(bw.buf, entries)
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

// ApplyBatch applies a slice of entry slices as a single batch to the
// associated Index.  It merges all entries and calls Apply once, so the
// whole batch publishes once rather than once per slice.
func (i *Index) ApplyBatch(slices [][]Entry) error {
	if len(slices) == 0 {
		return nil
	}
	// Merge all slices, validating each entry.
	var flat []Entry
	for _, s := range slices {
		for _, e := range s {
			if e.ImageID == 0 {
				return ErrInvalidEntry
			}
		}
		flat = append(flat, s...)
	}
	if len(flat) == 0 {
		return nil
	}
	return i.Apply(flat)
}

// StoreApplyBatch is a convenience that delegates to the perceptual index.
// The exact index has its own batch path; callers needing exact indexing
// should use ExactIndex.ApplyBatch or the ExactIndex directly.
func (s *Store) ApplyBatch(slices [][]Entry) error {
	return s.Perceptual().ApplyBatch(slices)
}

// ---------------------------------------------------------------------------

// Entry is a single record added to a perceptual index.  It carries the
// plane values (pHash64, PDQ, timestamp, etc.) that the index uses to build
// its on‑disk layout.  Defined in types.go; this is the package‑wide type.
