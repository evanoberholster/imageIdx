package hashidx

import (
	"context"
	"slices"
	"sync"
	"sync/atomic"

	"github.com/evanoberholster/imageIdx/internal/indexutil"
)

// Index is a persistent, mmap-backed perceptual-hash index. Readers load one
// immutable view and search without locks; one serialized writer owns appends,
// page sealing, checkpoint, and vacuum publication. There is no in-memory
// mode: every Index owns its directory and WAL.
type Index struct {
	view    atomic.Pointer[indexView]
	readers *indexutil.ReaderTracker

	writerMu sync.Mutex
	config   Config
	closed   atomic.Bool

	persistence persistedState
}

// Metadata returns a copied summary of the current published view.
func (i *Index) Metadata() Metadata {
	v := i.view.Load()
	if v == nil {
		return Metadata{RecordsPerPage: i.config.RecordsPerPage}
	}
	return Metadata{
		Generation:     v.generation,
		RecordCount:    v.count,
		PageCount:      v.logicalPageCount(),
		RecordsPerPage: i.config.RecordsPerPage,
	}
}

// Add appends one entry. It is convenience syntax around Apply.
func (i *Index) Add(entry Entry) error { return i.Apply([]Entry{entry}) }

// Apply appends one batch of entries. The whole batch publishes once: all
// six planes advance together, so a reader observes either none or all of
// the batch. Batches past Config.MaxBatchEntries fail with ErrBatchTooLarge
// before any WAL or view work. Apply validates entries, appends the WAL
// batch first when the index is persistent, then publishes the new
// immutable view.
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
	i.writerMu.Lock()
	defer i.writerMu.Unlock()
	if i.closed.Load() {
		return ErrClosed
	}
	i.persistence.mu.Lock()
	err := appendHashWAL(i.persistence.wal, entries, i.persistence.durability)
	i.persistence.mu.Unlock()
	if err != nil {
		return err
	}
	i.appendEntriesLocked(entries)
	return nil
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

// PendingCompactions reports in-flight background work. hashidx compacts
// synchronously, so this is always zero.
func (i *Index) PendingCompactions() int64 { return 0 }

// WaitIdle returns immediately; there is no background work to drain.
func (i *Index) WaitIdle(_ context.Context) error { return nil }

// Close marks the index closed, drains readers, and releases the WAL and mmap
// regions. Further Apply calls return ErrClosed and searches return no
// results.
func (i *Index) Close() error {
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
	imageIDs []ImageID
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
	imageIDs []ImageID
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
		imageIDs: make([]ImageID, recordsPerPage),
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
