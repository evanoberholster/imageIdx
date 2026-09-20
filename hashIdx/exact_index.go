package hashidx

import (
	"bytes"
	"context"
	"encoding/binary"
	"sync"
	"sync/atomic"

	"github.com/evanoberholster/imageIdx/internal/indexutil"
)

// blockHandle is one immutable sorted block. records holds Count*RecordSize
// bytes sorted by (Hash, ImageID); first is the first record's hash.
type blockHandle struct {
	first   Hash
	records []byte
}

func (b blockHandle) count() int { return len(b.records) / RecordSize }

// exactIndexView is one immutable, atomically published snapshot. Everything
// reachable from it is immutable for its lifetime, so readers take no mutex.
type exactIndexView struct {
	generation uint64
	liveCount  uint64
	blocks     []blockHandle
}

// ExactIndex maps exact 256-bit digests to image IDs with lock-free readers
// and a single-owner writer loop. There is no in-memory mode: every
// ExactIndex owns its directory and WAL.
//
// Writes serialize through the writer loop: each Apply hands a pooled batch
// to the loop and waits for its result, so a call returns only after its
// mutation is WAL-durable and published. Batches queued while the loop is
// busy are coalesced into one WAL append and one publication. Maintenance
// (Checkpoint, Vacuum, Close) coordinates through lifecycle: data calls take
// the read lock, maintenance takes the exclusive lock.
type ExactIndex struct {
	view    atomic.Pointer[exactIndexView]
	readers *indexutil.ReaderTracker

	writes    chan *exactBatch
	barriers  chan chan barrierResult
	lifecycle sync.RWMutex
	workers   sync.WaitGroup
	stop      chan struct{}
	closeOnce sync.Once

	config    ExactConfig
	indexID   uint64
	flags     uint32
	algorithm Algorithm

	generation atomic.Uint64
	closed     atomic.Bool
	seq        atomic.Uint64
	publishes  atomic.Uint64
	coalesced  atomic.Uint64

	persistence persistedState
}

// exactWriteQueue bounds queued write batches. Coalescing drains the queue,
// so depth only reflects momentary burst pressure.
const exactWriteQueue = 64

// barrierResult reports the WAL end offset observed by the writer loop after
// draining and applying every queued batch.
type barrierResult struct {
	walOffset int64
	err       error
}

// exactBatch is one writer-owned mutation batch in flight through the
// single-owner writer loop. Shells are pooled; done carries exactly one
// result per cycle. The loop never retains batch memory past completion.
type exactBatch struct {
	adds, removes []ExactEntry
	done          chan error
}

var exactBatchPool = sync.Pool{New: func() any {
	return &exactBatch{done: make(chan error, 1)}
}}

func acquireExactBatch(adds, removes []ExactEntry) *exactBatch {
	batch := exactBatchPool.Get().(*exactBatch)
	batch.adds = append(batch.adds[:0], adds...)
	batch.removes = append(batch.removes[:0], removes...)
	return batch
}

func releaseExactBatch(batch *exactBatch) {
	batch.adds = batch.adds[:0]
	batch.removes = batch.removes[:0]
	exactBatchPool.Put(batch)
}

// newExactIndex builds the initial immutable index from stable metadata and
// initial entries. Duplicate (Hash, ImageID) pairs are rejected; ImageID 0 is
// reserved. An optional reader tracker lets Store share one tracker across its
// indexes.
func newExactIndex(spec ExactSpec, cfg ExactConfig, readers ...*indexutil.ReaderTracker) (*ExactIndex, error) {
	if !spec.Algorithm.Valid() {
		return nil, ErrInvalidAlgorithm
	}
	cfg, err := cfg.withDefaults()
	if err != nil {
		return nil, err
	}
	entries := make([]ExactEntry, len(spec.Entries))
	copy(entries, spec.Entries)
	for _, e := range entries {
		if e.ImageID == 0 {
			return nil, ErrInvalidEntry
		}
	}
	sortEntries(entries)
	for i := 1; i < len(entries); i++ {
		if entries[i] == entries[i-1] {
			return nil, ErrDuplicateEntry
		}
	}
	maxRecords := maxRecordsPerBlock(cfg.BlockSize)
	target := targetRecordsPerBlock(cfg.BlockSize, cfg.TargetFillPct)
	parts := partitionEntries(entries, maxRecords, target)
	blocks := make([]blockHandle, len(parts))
	for i, part := range parts {
		rec := make([]byte, len(part)*RecordSize)
		for j, e := range part {
			encodeRecord(rec[j*RecordSize:(j+1)*RecordSize], e)
		}
		blocks[i] = blockHandle{first: part[0].Hash, records: rec}
	}
	idx := &ExactIndex{
		config: cfg, indexID: spec.IndexID, flags: spec.Flags, algorithm: spec.Algorithm,
		readers: chooseReaders(readers), writes: make(chan *exactBatch, exactWriteQueue),
		barriers: make(chan chan barrierResult), stop: make(chan struct{}),
	}
	idx.generation.Store(1)
	idx.view.Store(&exactIndexView{generation: 1, liveCount: uint64(len(entries)), blocks: blocks})
	idx.workers.Add(1)
	go idx.writerLoop()
	return idx, nil
}

// startWriterLoop starts the single-owner writer loop. openExact builds its
// index literally instead of through newExactIndex, so it calls this once
// its WAL handle is installed.
func (i *ExactIndex) startWriterLoop() {
	i.workers.Add(1)
	go i.writerLoop()
}

// Metadata returns a copied summary of the current generation.
func (i *ExactIndex) Metadata() ExactMetadata {
	v := i.view.Load()
	return ExactMetadata{
		IndexID: i.indexID, Flags: i.flags, Algorithm: i.algorithm,
		Generation: v.generation, BaseCount: v.liveCount, BlockCount: uint32(len(v.blocks)),
	}
}

// LiveBytes reports live logical bytes (40 per entry).
func (i *ExactIndex) LiveBytes() uint64 { return i.view.Load().liveCount * RecordSize }

// PhysicalBytes reports in-memory physical bytes including block payload,
// directory descriptors, and headers. Persistent indexes should consult the
// on-disk footprint via OnDiskBytes instead for amplification analysis.
func (i *ExactIndex) PhysicalBytes() uint64 {
	v := i.view.Load()
	total := uint64(len(v.blocks)) * (blockHeaderSize + dirEntrySize)
	total += v.liveCount * RecordSize
	return total
}

// findStartBlock returns the index of the block that may contain query: the
// greatest block with FirstHash <= query, or -1 when query sorts before the
// first block.
func findStartBlock(blocks []blockHandle, query Hash) int {
	lo, hi := 0, len(blocks)
	for lo < hi {
		mid := (lo + hi) >> 1
		if compareHash(blocks[mid].first, query) <= 0 {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	return lo - 1
}

// findRunStart returns the first block that may hold query, backing up across
// a duplicate-hash run that spans adjacent blocks. Without spanning, this is
// findStartBlock; with spanning, earlier blocks whose last record equals the
// query are included.
func findRunStart(blocks []blockHandle, query Hash) int {
	start := findStartBlock(blocks, query)
	for start > 0 {
		prev := blocks[start-1].records
		if len(prev) == 0 {
			break
		}
		if !bytes.Equal(prev[len(prev)-RecordSize:len(prev)-RecordSize+32], query[:]) {
			break
		}
		start--
	}
	return start
}

// runIterator walks the equal-hash run starting at hash across adjacent block
// boundaries. It yields records in (Hash, ImageID) order and allocates
// nothing, so Lookup, Count, and locatePair share one traversal.
type runIterator struct {
	blocks []blockHandle
	hash   Hash
	block  int
	pos    int
	done   bool
}

// newRunIterator positions an iterator on the first record of hash's run.
func newRunIterator(blocks []blockHandle, hash Hash) runIterator {
	it := runIterator{blocks: blocks, hash: hash, pos: -1}
	if len(blocks) == 0 {
		it.done = true
		return it
	}
	start := findRunStart(blocks, hash)
	if start < 0 {
		it.done = true
		return it
	}
	it.block = start
	return it
}

// next returns the next record in the run and the index of the block holding
// it. A false result means the run is exhausted or was never present.
func (it *runIterator) next() (int, []byte, bool) {
	for !it.done && it.block < len(it.blocks) {
		records := it.blocks[it.block].records
		n := len(records) / RecordSize
		if it.pos < 0 {
			// The run's first block is located by binary search.
			it.pos = blockLowerBound(records, it.hash)
		} else if it.pos == 0 {
			// Run continuation blocks always open with the query hash.
			if n == 0 || !bytes.Equal(records[:32], it.hash[:]) {
				break
			}
		}
		if it.pos >= n {
			// This block is exhausted. The run may continue when the next
			// block opens with the same hash; otherwise it ends here.
			if it.block+1 < len(it.blocks) && compareHash(it.blocks[it.block+1].first, it.hash) == 0 {
				it.block++
				it.pos = 0
				continue
			}
			break
		}
		rec := records[it.pos*RecordSize : (it.pos+1)*RecordSize]
		if !bytes.Equal(rec[:32], it.hash[:]) {
			break
		}
		blockIndex := it.block
		it.pos++
		return blockIndex, rec, true
	}
	it.done = true
	return 0, nil, false
}

// Contains reports whether any ImageID is associated with hash.
func (i *ExactIndex) Contains(hash Hash) bool {
	token := i.readers.Acquire()
	view := i.view.Load()
	found := false
	if !i.closed.Load() && view != nil {
		found = viewContains(view, hash)
	}
	i.readers.Release(token)
	return found
}

func viewContains(view *exactIndexView, hash Hash) bool {
	if len(view.blocks) == 0 {
		return false
	}
	start := findStartBlock(view.blocks, hash)
	if start < 0 {
		return false
	}
	pos := blockLowerBound(view.blocks[start].records, hash)
	if pos < view.blocks[start].count() &&
		bytes.Equal(view.blocks[start].records[pos*RecordSize:pos*RecordSize+32], hash[:]) {
		return true
	}
	return false
}

// Lookup appends the sorted unique ImageIDs for hash to dst.
func (i *ExactIndex) Lookup(dst []ImageID, hash Hash) []ImageID {
	token := i.readers.Acquire()
	if !i.closed.Load() {
		if view := i.view.Load(); view != nil {
			dst = viewLookup(dst, view, hash)
		}
	}
	i.readers.Release(token)
	return dst
}

func viewLookup(dst []ImageID, view *exactIndexView, hash Hash) []ImageID {
	it := newRunIterator(view.blocks, hash)
	for {
		_, rec, ok := it.next()
		if !ok {
			return dst
		}
		dst = append(dst, ImageID(binary.LittleEndian.Uint64(rec[32:40])))
	}
}

// Count returns the number of ImageIDs associated with hash.
func (i *ExactIndex) Count(hash Hash) uint64 {
	token := i.readers.Acquire()
	var n uint64
	if !i.closed.Load() {
		if view := i.view.Load(); view != nil {
			n = viewCount(view, hash)
		}
	}
	i.readers.Release(token)
	return n
}

func viewCount(view *exactIndexView, hash Hash) uint64 {
	var n uint64
	it := newRunIterator(view.blocks, hash)
	for {
		if _, _, ok := it.next(); !ok {
			return n
		}
		n++
	}
}

// Mutation is one atomically published batch. Entries appearing in both Add
// and Remove resolve with Remove winning; duplicate pairs within one side are
// canonicalized. The effective index holds at most one copy of each pair.
type Mutation struct {
	Add    []ExactEntry
	Remove []ExactEntry
}

// Add inserts one pair through the shared mutation path.
func (i *ExactIndex) Add(hash Hash, imageID ImageID) error {
	return i.Apply(Mutation{Add: []ExactEntry{{Hash: hash, ImageID: imageID}}})
}

// Remove deletes one pair through the shared mutation path.
func (i *ExactIndex) Remove(hash Hash, imageID ImageID) error {
	return i.Apply(Mutation{Remove: []ExactEntry{{Hash: hash, ImageID: imageID}}})
}

// Apply atomically publishes one mutation batch. A batch touching many
// records in one block rewrites that block once; the whole batch publishes
// once, so 1,000 images publish once, not 1,000 times.
//
// Apply hands its mutation to the single-owner writer loop and waits: it
// returns only after the mutation is WAL-durable and published (or with the
// publication error). Batches queued while the loop is busy are coalesced
// into one WAL append and one publication; each waiter still observes its
// own mutation on return, so calls stay linearizable. Slices are copied into
// pooled buffers and never retained. A concurrent Close may fail the call
// with ErrClosed instead of applying it.
func (i *ExactIndex) Apply(m Mutation) error {
	if len(m.Add) == 0 && len(m.Remove) == 0 {
		return nil
	}
	if uint64(len(m.Add)+len(m.Remove)) > uint64(i.config.MaxBatchEntries) {
		return ErrBatchTooLarge
	}
	for _, e := range m.Add {
		if e.ImageID == 0 {
			return ErrInvalidEntry
		}
	}
	for _, e := range m.Remove {
		if e.ImageID == 0 {
			return ErrInvalidEntry
		}
	}
	i.lifecycle.RLock()
	defer i.lifecycle.RUnlock()
	if i.closed.Load() {
		return ErrClosed
	}
	batch := acquireExactBatch(m.Add, m.Remove)
	select {
	case i.writes <- batch:
	case <-i.stop:
		releaseExactBatch(batch)
		return ErrClosed
	}
	err := <-batch.done
	releaseExactBatch(batch)
	return err
}

// writerLoop is the sole publisher: it owns WAL appends and view
// publication, so data batches never need a mutex between each other. It
// exits only on stop, rejecting anything still queued with ErrClosed.
func (i *ExactIndex) writerLoop() {
	defer i.workers.Done()
	for {
		select {
		case batch := <-i.writes:
			i.applyCoalesced(batch)
		case response := <-i.barriers:
			// Drain first: the barrier then observes a quiesced loop, which
			// Vacuum relies on. Callers must hold lifecycle.Lock so no new
			// batch can arrive mid-drain.
			pending := i.drainQueued()
			if len(pending) != 0 {
				i.publishMerged(pending)
			}
			var result barrierResult
			result.walOffset, result.err = i.persistence.wal.Offset()
			response <- result
		case <-i.stop:
			for {
				select {
				case batch := <-i.writes:
					batch.done <- ErrClosed
				default:
					return
				}
			}
		}
	}
}

// drainQueued collects every batch waiting behind the one being handled.
// The caller must guarantee no new batch can arrive (Vacuum's exclusive
// lock), so the drain terminates.
func (i *ExactIndex) drainQueued() []*exactBatch {
	var pending []*exactBatch
	for {
		select {
		case batch := <-i.writes:
			pending = append(pending, batch)
		default:
			return pending
		}
	}
}

// applyCoalesced merges the received batch with whatever queued behind it
// (bounded by MaxBatchEntries per publication) and publishes once.
func (i *ExactIndex) applyCoalesced(first *exactBatch) {
	pending := []*exactBatch{first}
	total := len(first.adds) + len(first.removes)
	for total < int(i.config.MaxBatchEntries) {
		select {
		case batch := <-i.writes:
			pending = append(pending, batch)
			total += len(batch.adds) + len(batch.removes)
		default:
			i.publishMerged(pending)
			return
		}
	}
	i.publishMerged(pending)
}

// publishMerged canonicalizes the flattened batches once, appends one WAL
// record, publishes one view, and completes every waiter with the result.
func (i *ExactIndex) publishMerged(pending []*exactBatch) {
	adds, removes := flattenBatches(pending)
	if len(adds)+len(removes) != 0 {
		current := i.view.Load()
		adds, removes = canonicalizeMutation(current, Mutation{Add: adds, Remove: removes})
	}
	var err error
	if len(adds)+len(removes) != 0 {
		err = i.publishBatch(adds, removes)
	}
	i.publishes.Add(1)
	if len(pending) > 1 {
		i.coalesced.Add(uint64(len(pending) - 1))
	}
	// Ownership returns to the waiters with the result: each Apply releases
	// its own shell exactly once, so the loop must not release here.
	for _, batch := range pending {
		batch.done <- err
	}
}

// flattenBatches concatenates pending batches preserving arrival order.
func flattenBatches(pending []*exactBatch) (adds, removes []ExactEntry) {
	for _, batch := range pending {
		adds = append(adds, batch.adds...)
		removes = append(removes, batch.removes...)
	}
	return adds, removes
}

// publishBatch appends one WAL record and publishes the resulting view. It
// runs only on the writer loop, which is the sole WAL appender, so no lock
// is needed here; maintenance paths coordinate through lifecycle and never
// touch the WAL handle while the loop can append.
func (i *ExactIndex) publishBatch(adds, removes []ExactEntry) error {
	i.persistence.mu.Lock()
	err := appendExactWAL(i.persistence.wal, adds, removes, i.persistence.durability, i.seq.Add(1))
	i.persistence.mu.Unlock()
	if err != nil {
		return err
	}
	current := i.view.Load()
	next := applyEffective(current, adds, removes, i.config)
	next.generation = current.generation + 1
	i.view.Store(next)
	i.generation.Store(next.generation)
	// No reader drain here: old blocks are Go-allocated and reclaimed by GC,
	// and mmap unmapping only happens in Vacuum/Close after an explicit
	// WaitForPriorReaders. Waiting on every publish would stall writers under
	// continuous read load and can deadlock a reader that holds a token
	// across a write.
	return nil
}

// applyWithoutWAL folds one canonicalized batch without touching the WAL.
// It is used by WAL replay during open.
func (i *ExactIndex) applyWithoutWAL(adds, removes []ExactEntry) {
	current := i.view.Load()
	if len(adds) == 0 && len(removes) == 0 {
		return
	}
	next := applyEffective(current, adds, removes, i.config)
	next.generation = current.generation + 1
	i.view.Store(next)
	i.generation.Store(next.generation)
}

// canonicalizeMutation deduplicates the batch (remove wins on conflict) and
// drops pairs whose desired state already matches the current view.
func canonicalizeMutation(view *exactIndexView, m Mutation) (adds, removes []ExactEntry) {
	desired := make(map[ExactEntry]int8, len(m.Add)+len(m.Remove))
	for _, e := range m.Add {
		desired[e] = 1
	}
	for _, e := range m.Remove {
		desired[e] = -1 // remove wins
	}
	adds = make([]ExactEntry, 0, len(m.Add))
	removes = make([]ExactEntry, 0, len(m.Remove))
	for e, op := range desired {
		present := viewContainsPair(view, e)
		if op == 1 && !present {
			adds = append(adds, e)
		} else if op == -1 && present {
			removes = append(removes, e)
		}
	}
	sortEntries(adds)
	sortEntries(removes)
	return adds, removes
}

func viewContainsPair(view *exactIndexView, e ExactEntry) bool {
	if view == nil || len(view.blocks) == 0 {
		return false
	}
	return locatePair(view.blocks, e) >= 0
}

// applyEffective merges canonicalized adds/removes into the block set,
// rewriting each affected block once and splitting overflows. Caller must
// hold the writer mutex.
func applyEffective(view *exactIndexView, adds, removes []ExactEntry, cfg ExactConfig) *exactIndexView {
	maxRecords := maxRecordsPerBlock(cfg.BlockSize)
	if maxRecords < 2 {
		maxRecords = 2
	}
	target := targetRecordsPerBlock(cfg.BlockSize, cfg.TargetFillPct)
	// Group adds/removes by affected block. Removals name an exact pair, so
	// they map to the block holding that pair; adds map by hash directory.
	affected := make(map[int]struct{}, len(view.blocks)+len(adds)+len(removes))
	blockAdds := make(map[int][]ExactEntry)
	blockRemoves := make(map[int][]ExactEntry)
	for _, e := range removes {
		if idx := locatePair(view.blocks, e); idx >= 0 {
			affected[idx] = struct{}{}
			blockRemoves[idx] = append(blockRemoves[idx], e)
		}
	}
	for _, e := range adds {
		idx := findStartBlock(view.blocks, e.Hash)
		if idx < 0 {
			idx = 0
		}
		if len(view.blocks) == 0 {
			continue
		}
		affected[idx] = struct{}{}
		blockAdds[idx] = append(blockAdds[idx], e)
	}
	if len(view.blocks) == 0 {
		merged := append([]ExactEntry(nil), adds...)
		sortEntries(merged)
		parts := partitionEntries(merged, maxRecords, target)
		blocks := make([]blockHandle, len(parts))
		for k, part := range parts {
			rec := make([]byte, len(part)*RecordSize)
			for j, e := range part {
				encodeRecord(rec[j*RecordSize:(j+1)*RecordSize], e)
			}
			blocks[k] = blockHandle{first: part[0].Hash, records: rec}
		}
		return &exactIndexView{liveCount: uint64(len(merged)), blocks: blocks}
	}
	// Decode affected blocks once, merge, re-split.
	replacements := make(map[int][]blockHandle, len(affected))
	var liveDelta int64
	for idx := range affected {
		old := view.blocks[idx]
		oldEntries := decodeBlockEntries(old.records)
		merged := mergeBlockEntries(oldEntries, blockAdds[idx], blockRemoves[idx])
		liveDelta += int64(len(merged) - len(oldEntries))
		if len(merged) == 0 {
			replacements[idx] = nil
			continue
		}
		// Adds routed to block 0 may sort before its previous first hash;
		// the merge and re-partition absorb them here. Replacements expand
		// in place, so the directory stays ordered without a re-sort.
		parts := partitionEntries(merged, maxRecords, target)
		out := make([]blockHandle, len(parts))
		for k, part := range parts {
			rec := make([]byte, len(part)*RecordSize)
			for j, e := range part {
				encodeRecord(rec[j*RecordSize:(j+1)*RecordSize], e)
			}
			out[k] = blockHandle{first: part[0].Hash, records: rec}
		}
		replacements[idx] = out
	}
	// Handle adds for hashes beyond all blocks (routed to last block above,
	// which covers them via split). Adds before the first block are also
	// covered because block 0's merge includes them. Block order is already
	// correct: replacements expand in place and stay within the original
	// block's range, so no re-sort is needed (and re-sorting would reorder
	// equal-FirstHash spanning blocks).
	newBlocks := make([]blockHandle, 0, len(view.blocks)+len(replacements))
	for idx, old := range view.blocks {
		if rep, ok := replacements[idx]; ok {
			newBlocks = append(newBlocks, rep...)
		} else {
			newBlocks = append(newBlocks, old)
		}
	}
	liveCount := uint64(int64(view.liveCount) + liveDelta)
	return &exactIndexView{blocks: newBlocks, liveCount: liveCount}
}

// locatePair finds the block holding an exact pair, or -1.
func locatePair(blocks []blockHandle, e ExactEntry) int {
	it := newRunIterator(blocks, e.Hash)
	for {
		blockIndex, rec, ok := it.next()
		if !ok {
			return -1
		}
		if ImageID(binary.LittleEndian.Uint64(rec[32:40])) == e.ImageID {
			return blockIndex
		}
	}
}

func decodeBlockEntries(records []byte) []ExactEntry {
	n := len(records) / RecordSize
	out := make([]ExactEntry, n)
	for i := 0; i < n; i++ {
		out[i] = decodeRecord(records[i*RecordSize : (i+1)*RecordSize])
	}
	return out
}

func mergeBlockEntries(old, adds, removes []ExactEntry) []ExactEntry {
	if len(adds) == 0 && len(removes) == 0 {
		return old
	}
	removeSet := make(map[ExactEntry]struct{}, len(removes))
	for _, e := range removes {
		removeSet[e] = struct{}{}
	}
	merged := make([]ExactEntry, 0, len(old)+len(adds))
	for _, e := range old {
		if _, ok := removeSet[e]; !ok {
			merged = append(merged, e)
		}
	}
	merged = append(merged, adds...)
	sortEntries(merged)
	// Defensive de-duplication (canonicalization should already guarantee
	// uniqueness, but merges across split boundaries must never emit dups).
	out := merged[:0]
	for i, e := range merged {
		if i == 0 || e != merged[i-1] {
			out = append(out, e)
		}
	}
	return out
}

// PendingCompactions reports queued but unpublished write batches. Block
// rewrites happen synchronously on the writer loop, so this is only nonzero
// while Apply calls are in flight.
func (i *ExactIndex) PendingCompactions() int64 { return int64(len(i.writes)) }

// WaitIdle returns immediately; every queued batch has a waiting owner, so
// there is no detached background work to drain.
func (i *ExactIndex) WaitIdle(_ context.Context) error { return nil }

// WriteStats returns the total publications and the number of batches
// coalesced into shared publications since construction. Consecutive
// concurrent Applies sharing one WAL append and one view publish show up as
// coalesced batches.
func (i *ExactIndex) WriteStats() (publishes, coalesced uint64) {
	return i.publishes.Load(), i.coalesced.Load()
}

// Close marks the index closed, stops the writer loop (queued batches fail
// with ErrClosed), drains readers, and releases the WAL and mmap regions.
// Further Apply calls return ErrClosed; reads return zero values.
func (i *ExactIndex) Close() error {
	var closeErr error
	i.closeOnce.Do(func() {
		i.lifecycle.Lock()
		defer i.lifecycle.Unlock()
		i.closed.Store(true)
		close(i.stop)
		i.workers.Wait()
		i.readers.WaitForPriorReaders()
		closeErr = i.persistence.close()
	})
	return closeErr
}
