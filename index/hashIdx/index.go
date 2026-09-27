package hashidx

import (
	"context"
	"errors"
	"slices"
	"sync"
	"sync/atomic"

	common "github.com/evanoberholster/imageIdx/common"
	"github.com/evanoberholster/imageIdx/internal/commit"
	"github.com/evanoberholster/imageIdx/internal/storage"
)

// hashCommitQueueCapacity bounds queued Apply requests. Apply blocks when
// the queue is full, so bursty writers exert backpressure instead of
// growing memory without bound.
const hashCommitQueueCapacity = 64

// applyRequest is one queued Apply call. Requests are pooled so the
// per-call completion channel does not allocate in steady state.
type applyRequest struct {
	entries []Entry
	done    chan error
}

var applyRequestPool = sync.Pool{New: func() any {
	return &applyRequest{done: make(chan error, 1)}
}}

func acquireApplyRequest(entries []Entry) *applyRequest {
	req := applyRequestPool.Get().(*applyRequest)
	req.entries = entries
	return req
}

func releaseApplyRequest(req *applyRequest) {
	req.entries = nil
	applyRequestPool.Put(req)
}

// Index is a persistent, mmap-backed perceptual-hash index. Readers load one
// immutable view and search without locks; writes queue onto one
// group-commit loop that coalesces whatever is waiting into a single WAL
// sync and a single view publication. Apply blocks until its own commit
// completes, so a call returns only after its entries are WAL-durable and
// visible. There is no in-memory mode: every Index owns its directory and
// WAL.
type Index struct {
	view    atomic.Pointer[indexView]
	readers *storage.ReaderTracker

	writerMu  sync.Mutex
	config    Config
	namespace common.Namespace
	closed    atomic.Bool
	// failed latches the first WAL error, guarded by writerMu. While set,
	// writes and maintenance fail fast with it until close and reopen.
	failed error

	// commits is the single-owner group-commit loop. Requests queued while
	// the loop is busy commit as one group sharing one WAL sync, one lock
	// hold, and one view publication.
	commits *commit.Queue[*applyRequest]
	// groupEntries merges one commit group into a single publication. It
	// is commit-loop owned and reused across groups.
	groupEntries []Entry

	persistence persistedState
}

// Metadata returns a copied summary of the current published view. Namespace
// is the perceptual index's schema namespace and is zero for an index
// constructed directly without a Store.
func (i *Index) Metadata() Metadata {
	v := i.view.Load()
	if v == nil {
		return Metadata{Namespace: i.namespace, RecordsPerPage: i.config.RecordsPerPage}
	}
	return Metadata{
		Namespace:      i.namespace,
		Generation:     v.generation,
		RecordCount:    v.count,
		PageCount:      v.logicalPageCount(),
		RecordsPerPage: i.config.RecordsPerPage,
	}
}

// Add appends one entry. It is convenience syntax around Apply.
func (i *Index) Add(entry Entry) error { return i.Apply([]Entry{entry}) }

// Apply appends one batch of entries and blocks until it is committed. The
// whole batch publishes once: all six planes advance together, so a reader
// observes either none or all of the batch. Batches past
// Config.MaxBatchEntries fail with ErrBatchTooLarge before any WAL or view
// work. Batches queued while the commit loop is busy are coalesced into one
// WAL sync and one view publication, so N concurrent writers pay one fsync
// per group under common.DurabilitySync. A WAL failure latches
// ErrRecoveryRequired: the view is untouched, and later writes fail fast
// with it until close and reopen.
func (i *Index) Apply(entries []Entry) error {
	if len(entries) == 0 {
		return nil
	}
	if uint64(len(entries)) > uint64(i.config.MaxBatchEntries) {
		return ErrBatchTooLarge
	}
	for k := range entries {
		if entries[k].ImageID == 0 {
			return ErrInvalidEntry
		}
	}
	if i.closed.Load() {
		return ErrClosed
	}
	req := acquireApplyRequest(entries)
	if !i.commits.Submit(req) {
		releaseApplyRequest(req)
		return ErrClosed
	}
	// A successful Submit is always committed, even across Close, so the
	// completion signal is guaranteed.
	err := <-req.done
	releaseApplyRequest(req)
	return err
}

// commitGroup is the commit.Queue callback. It holds writerMu for the whole
// group, so it serializes with Checkpoint, Vacuum, and Close. It appends
// one WAL frame per request, syncs once for the group under
// common.DurabilitySync, merges the group into a single page append, and then
// completes every waiter. Readers never interact with it: publication
// stays one atomic view store, so reads keep their zero-lock guarantee.
//
// A WAL failure fails the whole group and publishes nothing; complete
// frames written before the failure may still replay after recovery, the
// same redo contract as a single synchronous Apply.
func (i *Index) commitGroup(group []*applyRequest) {
	i.writerMu.Lock()
	defer i.writerMu.Unlock()
	if i.closed.Load() {
		respondApply(group, ErrClosed)
		return
	}
	if i.failed != nil {
		respondApply(group, i.failed)
		return
	}
	i.persistence.mu.Lock()
	var walErr error
	for _, req := range group {
		if err := appendHashWAL(i.persistence.wal, req.entries, false); err != nil {
			walErr = err
			break
		}
	}
	if walErr == nil && i.persistence.durability == common.DurabilitySync {
		walErr = i.persistence.wal.Sync()
	}
	i.persistence.mu.Unlock()
	if walErr != nil {
		i.failed = errors.Join(ErrRecoveryRequired, walErr)
		respondApply(group, i.failed)
		return
	}
	i.groupEntries = i.groupEntries[:0]
	for _, req := range group {
		i.groupEntries = append(i.groupEntries, req.entries...)
	}
	i.appendEntriesLocked(i.groupEntries)
	respondApply(group, nil)
}

// respondApply completes every waiter in group with err. It runs only
// after the group's last read of any request's entries, so a caller may
// release its pooled request as soon as it observes the signal.
func respondApply(group []*applyRequest, err error) {
	for _, req := range group {
		req.done <- err
	}
}

// appendEntriesLocked writes entries into the active page, sealing full pages
// as they complete, and publishes one replacement view. Caller must hold
// writerMu.
func (i *Index) appendEntriesLocked(entries []Entry) {
	if len(entries) == 0 {
		return
	}
	recordsPerPage := int(i.config.RecordsPerPage)
	current := i.view.Load()
	count := current.count + uint64(len(entries))
	pages := current.pages
	active := current.active
	activeCount := current.activeCount
	cloned := false

	remaining := entries
	for len(remaining) > 0 {
		if active == nil {
			active = newActivePage(recordsPerPage)
			activeCount = 0
		}
		n := min(recordsPerPage-activeCount, len(remaining))
		for k := 0; k < n; k++ {
			entry := &remaining[k]
			slot := activeCount + k
			active.imageIDs[slot] = entry.ImageID
			active.phash64[slot] = entry.PHash64
			active.pdq[0][slot] = entry.PDQ[0]
			active.pdq[1][slot] = entry.PDQ[1]
			active.pdq[2][slot] = entry.PDQ[2]
			active.pdq[3][slot] = entry.PDQ[3]
		}
		activeCount += n
		remaining = remaining[n:]
		if activeCount == recordsPerPage {
			if !cloned {
				pages = slices.Clone(current.pages)
				cloned = true
			}
			pages = append(pages, pageView{
				imageIDs: active.imageIDs,
				phash64:  active.phash64,
				pdq:      active.pdq,
			})
			// The full active planes become the sealed page; the writer
			// allocates a fresh active page for any remaining entries and
			// never touches the sealed arrays again.
			active = nil
			activeCount = 0
		}
	}

	i.view.Store(&indexView{
		generation:  current.generation + 1,
		count:       count,
		pages:       pages,
		active:      active,
		activeCount: activeCount,
	})
}

// EntryAt returns the record stored at ordinal. Ordinals are internal and are
// not stable across Vacuum; they are exposed so future secondary indexes can
// store ordinals instead of duplicating hash data.
func (i *Index) EntryAt(ordinal Ordinal) (Entry, bool) {
	token := i.readers.Acquire()
	var entry Entry
	found := false
	if !i.closed.Load() {
		if view := i.view.Load(); view != nil && uint64(ordinal) < view.count {
			recordsPerPage := uint64(i.config.RecordsPerPage)
			sealed := uint64(len(view.pages)) * recordsPerPage
			switch {
			case uint64(ordinal) < sealed:
				page := &view.pages[uint64(ordinal)/recordsPerPage]
				entry = entryAtPage(page, int(uint64(ordinal)%recordsPerPage))
				found = true
			case view.active != nil && int(uint64(ordinal)-sealed) < view.activeCount:
				entry = entryAtActive(view.active, int(uint64(ordinal)-sealed))
				found = true
			}
		}
	}
	i.readers.Release(token)
	return entry, found
}

// PendingCompactions reports in-flight background work. The perceptual
// index compacts synchronously, so this is always zero. It exists for
// symmetry with the other index packages, so a future generic
// maintenance driver can poll every index type uniformly.
func (i *Index) PendingCompactions() int64 { return 0 }

// WaitIdle returns immediately: every Apply blocks until its own commit is
// published, so there is no caller-visible background work to drain. It
// exists for the same cross-package symmetry as PendingCompactions.
func (i *Index) WaitIdle(_ context.Context) error { return nil }

// Close marks the index closed, drains queued commits, drains readers, and
// releases the WAL and mmap regions. Queued Apply requests commit before
// Close returns; an Apply racing Close may observe ErrClosed. Further Apply
// calls return ErrClosed and searches return no results.
func (i *Index) Close() error {
	i.commits.Stop()
	i.commits.Wait()
	i.writerMu.Lock()
	defer i.writerMu.Unlock()
	if i.closed.Swap(true) {
		return nil
	}
	i.readers.WaitForPriorReaders()
	return i.persistence.close()
}

// pageView is one immutable sealed page. Each plane has exactly count()
// elements; the planes are never mutated after publication.
type pageView struct {
	imageIDs []common.ImageID
	phash64  []uint64
	pdq      [4][]uint64
}

// count reports the number of records in the page.
func (p *pageView) count() int { return len(p.imageIDs) }

// activePage is the writer-owned append target. Every plane has
// RecordsPerPage slots; only the prefix published in an indexView is
// reader-visible, and each slot is written exactly once before publication.
// The writer never rewrites a published slot, which is what keeps readers
// lock-free without copying the active page on every publish.
type activePage struct {
	imageIDs []common.ImageID
	phash64  []uint64
	pdq      [4][]uint64
}

// newActivePage allocates one page in two allocations: the ID plane plus
// one slab sliced into the five hash planes, keeping the planes adjacent
// for scan locality. A sealed page retains its slab; nothing is shared
// across pages.
func newActivePage(recordsPerPage int) *activePage {
	slab := make([]uint64, 5*recordsPerPage)
	phash := slab[:recordsPerPage]
	off := recordsPerPage
	var pdq [4][]uint64
	for w := 0; w < 4; w++ {
		pdq[w] = slab[off : off+recordsPerPage]
		off += recordsPerPage
	}
	return &activePage{
		imageIDs: make([]common.ImageID, recordsPerPage),
		phash64:  phash,
		pdq:      pdq,
	}
}

// indexView is one immutable, atomically published snapshot. Everything
// reachable from a view is immutable for the view's lifetime except the
// unpublished tail of active, which no reader can reach because activeCount
// bounds every scan.
type indexView struct {
	generation  uint64
	count       uint64
	pages       []pageView
	active      *activePage
	activeCount int
}

// logicalPageCount counts sealed pages plus an in-progress final page.
func (v *indexView) logicalPageCount() uint64 {
	pages := uint64(len(v.pages))
	if v.activeCount > 0 {
		pages++
	}
	return pages
}

// entryAtPage reads one record from a sealed page.
func entryAtPage(p *pageView, k int) Entry {
	return Entry{
		ImageID: p.imageIDs[k],
		PHash64: p.phash64[k],
		PDQ: PDQHash{
			p.pdq[0][k],
			p.pdq[1][k],
			p.pdq[2][k],
			p.pdq[3][k],
		},
	}
}

// entryAtActive reads one published record from the active page.
func entryAtActive(a *activePage, k int) Entry {
	return Entry{
		ImageID: a.imageIDs[k],
		PHash64: a.phash64[k],
		PDQ: PDQHash{
			a.pdq[0][k],
			a.pdq[1][k],
			a.pdq[2][k],
			a.pdq[3][k],
		},
	}
}
