package hashidx

import (
	"sync"
	"time"
)

// BufferedWriter accumulates mutations and flushes them periodically or when
// an entry threshold is reached. It reduces WAL append + publish cycles when
// many small Apply calls arrive in quick succession.
//
// Mutations enter through a buffered channel (capacity 256). A background
// worker moves them into an internal buffer and flushes when either the
// accumulated entry count reaches batchEntries or the ticker interval
// elapses. Flush forces an immediate publish. Close stops the worker,
// drains everything still queued (including channel leftovers), and flushes.
//
// Concurrency: Apply never blocks the index writer; it fails fast with
// ErrChannelFull when the channel is full so producers can shed load.
// Flush may run concurrently with the worker; buffer takes are serialized
// by mu and each take publishes exactly once. After Close the writer must
// not be used; Close is idempotent.
//
// The zero value is not usable; use NewBufferedWriter.
type BufferedWriter struct {
	idx          *Index
	ch           chan []Entry
	batchEntries int // flush when pending entries reach this count
	tick         *time.Ticker
	stop         chan struct{}
	mu           sync.Mutex
	buf          [][]Entry // protected by mu
	pending      int       // entries in buf, protected by mu
	flushes      uint64    // protected by mu
	flushed      uint64    // entries flushed, protected by mu
	wg           sync.WaitGroup
	closeOnce    sync.Once
}

// bufferedWriterChannelCap bounds queued mutation slices. Apply fails fast
// with ErrChannelFull past it instead of stalling the producer.
const bufferedWriterChannelCap = 256

// NewBufferedWriter returns a writer that flushes when either batchEntries
// have been accumulated or interval has elapsed, whichever comes first. A
// non-positive batchEntries flushes every slice; a non-positive interval
// selects a one-second default. The caller must Close the writer.
func NewBufferedWriter(idx *Index, batchEntries int, interval time.Duration) *BufferedWriter {
	if batchEntries < 1 {
		batchEntries = 1
	}
	if interval <= 0 {
		interval = time.Second
	}
	bw := &BufferedWriter{
		idx:          idx,
		ch:           make(chan []Entry, bufferedWriterChannelCap),
		batchEntries: batchEntries,
		tick:         time.NewTicker(interval),
		stop:         make(chan struct{}),
	}
	bw.wg.Add(1)
	go bw.worker()
	return bw
}

// Apply queues an entry slice for a later flush. It never blocks: a full
// channel reports ErrChannelFull so the caller can retry, drop, or flush.
// The slice is retained until flushed; callers must not mutate it after
// Apply returns nil.
func (bw *BufferedWriter) Apply(entries []Entry) error {
	select {
	case bw.ch <- entries:
		return nil
	default:
		return ErrChannelFull
	}
}

// Flush publishes everything buffered so far in one Apply. It swaps the
// buffer under the lock and publishes outside it, so concurrent Flush
// calls and the worker never publish the same entries twice. On publish
// failure the entries are prepended back under the lock ahead of anything
// queued meanwhile, so nothing is lost and order is preserved.
func (bw *BufferedWriter) Flush() error {
	bw.mu.Lock()
	entries := bw.buf
	bw.buf = nil
	bw.pending = 0
	bw.mu.Unlock()

	if len(entries) == 0 {
		return nil
	}
	return bw.flushEntries(entries)
}

// flushEntries flattens entry slices with one presized allocation and
// publishes once. Locking: buf/pending/stats only; never held across
// idx.Apply.
func (bw *BufferedWriter) flushEntries(entries [][]Entry) error {
	total := 0
	for _, s := range entries {
		total += len(s)
	}
	flat := make([]Entry, 0, total)
	for _, s := range entries {
		flat = append(flat, s...)
	}
	if len(flat) == 0 {
		return nil
	}
	if err := bw.idx.Apply(flat); err != nil {
		bw.mu.Lock()
		bw.buf = append(entries, bw.buf...)
		bw.pending += total
		bw.mu.Unlock()
		return err
	}
	bw.mu.Lock()
	bw.flushes++
	bw.flushed += uint64(len(flat))
	bw.mu.Unlock()
	return nil
}

// Stats returns the total flushes and entries flushed since construction.
func (bw *BufferedWriter) Stats() (flushes uint64, entries uint64) {
	bw.mu.Lock()
	defer bw.mu.Unlock()
	return bw.flushes, bw.flushed
}

// Close stops the worker, drains everything still queued (buffered slices
// and channel leftovers), and flushes once. It stops the ticker, is
// idempotent, and must not race a concurrent Close only in that both
// return the final flush result.
func (bw *BufferedWriter) Close() error {
	bw.closeOnce.Do(func() { close(bw.stop) })
	bw.tick.Stop()
	bw.wg.Wait()
	return bw.Flush()
}

// worker moves channel slices into the buffer and flushes outside the
// lock. The threshold take happens under the lock; publication never does,
// so Flush can never self-deadlock here.
func (bw *BufferedWriter) worker() {
	defer bw.wg.Done()
	for {
		select {
		case entries := <-bw.ch:
			var toFlush [][]Entry
			bw.mu.Lock()
			bw.buf = append(bw.buf, entries)
			bw.pending += len(entries)
			if bw.pending >= bw.batchEntries {
				toFlush = bw.buf
				bw.buf = nil
				bw.pending = 0
			}
			bw.mu.Unlock()
			if toFlush != nil {
				_ = bw.flushEntries(toFlush)
			}
		case <-bw.tick.C:
			_ = bw.Flush()
		case <-bw.stop:
			bw.drain()
			return
		}
	}
}

// drain moves every channel leftover into the buffer, then flushes once.
// It runs only on the stopping worker, after which no new channel takes
// happen here; a concurrent public Flush may still publish first, in
// which case this flush observes an empty buffer and is a no-op.
func (bw *BufferedWriter) drain() {
	for {
		select {
		case entries := <-bw.ch:
			bw.mu.Lock()
			bw.buf = append(bw.buf, entries)
			bw.pending += len(entries)
			bw.mu.Unlock()
		default:
			_ = bw.Flush()
			return
		}
	}
}

// ---------------------------------------------------------------------------

// ApplyBatch merges entry batches and publishes once, so the whole batch
// costs one WAL append and one view publication instead of one per slice.
// Entries validate up front; an invalid entry fails the whole batch.
func (i *Index) ApplyBatch(batches [][]Entry) error {
	if len(batches) == 0 {
		return nil
	}
	total := 0
	for _, batch := range batches {
		for _, e := range batch {
			if e.ImageID == 0 {
				return ErrInvalidEntry
			}
		}
		total += len(batch)
	}
	if total == 0 {
		return nil
	}
	flat := make([]Entry, 0, total)
	for _, batch := range batches {
		flat = append(flat, batch...)
	}
	return i.Apply(flat)
}

// ApplyBatch is a convenience that delegates to the perceptual index. The
// exact index has its own batch path; callers needing exact indexing
// should use ExactIndex.Apply with a Mutation directly.
func (s *Store) ApplyBatch(batches [][]Entry) error {
	return s.Perceptual().ApplyBatch(batches)
}
