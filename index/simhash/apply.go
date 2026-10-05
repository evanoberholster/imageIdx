package simhash

import (
	"sync/atomic"
	"unsafe"

	common "github.com/evanoberholster/imageIdx/common"
	"github.com/evanoberholster/imageIdx/internal/storage"
)

// Append appends one record and returns its assigned ImageID. It shares the
// append path without constructing a one-element heap slice.
func (st *Store) Append(rec Record) (common.ImageID, error) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.closed.Load() {
		return 0, ErrClosed
	}
	st.singleRec[0] = rec
	return st.appendBatchLocked(st.singleRec[:])
}

// AppendBatch appends records densely and returns the first assigned ImageID.
// The assigned IDs are first..first+len(records)-1. New slots are not yet
// visible, so appends do not use the update WAL. The batch is preflighted before
// any canonical write, so a failure leaves logical state unchanged.
func (st *Store) AppendBatch(recs []Record) (common.ImageID, error) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.closed.Load() {
		return 0, ErrClosed
	}
	return st.appendBatchLocked(recs)
}

func (st *Store) appendBatchLocked(recs []Record) (common.ImageID, error) {
	if len(recs) == 0 {
		return common.ImageID(st.committed + 1), nil
	}
	if st.committed > ^uint64(0)-uint64(len(recs)) || st.generation == ^uint64(0) {
		return 0, ErrOverflow
	}
	first := st.committed + 1
	last := st.committed + uint64(len(recs))
	firstShard := hashPageOf(first) / st.pagesPerShard
	lastShard := hashPageOf(last) / st.pagesPerShard

	// Preflight: ensure every required shard exists before touching canonical
	// state. ImageIDs are dense, so the required shards form a contiguous range.
	// If a later shard creation fails, logical state is unchanged: committed,
	// generation, and the reader-visible high-water are untouched, and any
	// already-created shard is retained as physical capacity.
	if err := st.ensureShardsLocked(lastShard); err != nil {
		return 0, err
	}
	oldCommitted := st.committed
	oldGeneration := st.generation
	st.writeRecordsLocked(first, recs)
	for si := firstShard; si <= lastShard; si++ {
		sh := st.shards[si]
		sh.committed = shardCommitted(sh, last)
	}
	st.committed = last
	st.generation++

	// Under sync durability, do not publish the commit to readers until the
	// canonical data, shard metadata, and manifest are durable. On failure,
	// roll back writer state; readers never observed the new IDs.
	if st.config.Durability == common.DurabilitySync {
		if err := st.persistLocked(); err != nil {
			st.committed = oldCommitted
			st.generation = oldGeneration
			for si := firstShard; si <= lastShard; si++ {
				sh := st.shards[si]
				sh.committed = shardCommitted(sh, oldCommitted)
			}
			return 0, err
		}
	}
	st.committedAtomic.Store(last)
	st.gen.Store(st.generation)
	return common.ImageID(first), nil
}

// ensureShardsLocked creates every shard up to lastShard and publishes the new
// shard set before any committed mark advances.
func (st *Store) ensureShardsLocked(lastShard uint64) error {
	created := false
	for uint64(len(st.shards)) <= lastShard {
		sh, err := st.createShardLocked(uint64(len(st.shards)))
		if err != nil {
			return err
		}
		st.shards = append(st.shards, sh)
		created = true
	}
	if created {
		st.publishLocked()
	}
	return nil
}

// writeRecordsLocked writes the append batch with arithmetic addressing only.
// Dense append IDs stay within one hash page for almost every batch, so it
// processes contiguous runs per page and hoists the column bases once per run
// instead of recomputing them per record.
//
// The records are not yet reader-visible, so pHash and PDQ use plain stores.
// Quality still uses the aligned 8-byte atomic read-modify-write: an
// unpublished slot can share its quality word with a published neighbour that a
// concurrent Quality read loads atomically.
func (st *Store) writeRecordsLocked(first uint64, recs []Record) {
	i := 0
	id := first
	for i < len(recs) {
		page := hashPageOf(id)
		sh := st.shards[page/st.pagesPerShard]
		pi := page - sh.firstPage
		slot := slotOf(id)
		run := slotsPerHashPage - slot
		if run > len(recs)-i {
			run = len(recs) - i
		}
		base := sh.base(pi)
		pdqBase := base + pdqColumnOffset
		qBase := base + qualityColumnOffset
		for k := 0; k < run; k++ {
			s := slot + k
			rec := &recs[i+k]
			*(*uint64)(unsafe.Pointer(&sh.arena[base+uint64(s)*8])) = rec.PHash
			w := hashWords(&rec.PDQ)
			off := pdqBase + uint64(s)*32
			*(*uint64)(unsafe.Pointer(&sh.arena[off])) = w[0]
			*(*uint64)(unsafe.Pointer(&sh.arena[off+8])) = w[1]
			*(*uint64)(unsafe.Pointer(&sh.arena[off+16])) = w[2]
			*(*uint64)(unsafe.Pointer(&sh.arena[off+24])) = w[3]
			shift := uint(s&7) * 8
			word := (*uint64)(unsafe.Pointer(&sh.arena[qBase+uint64(s&^7)]))
			atomic.StoreUint64(word, (atomic.LoadUint64(word)&^(0xff<<shift))|(uint64(rec.Quality)<<shift))
		}
		i += run
		id += uint64(run)
	}
}

// shardCommitted returns the number of valid records in shard sh up to lastID.
func shardCommitted(sh *shard, lastID uint64) uint64 {
	firstID := sh.firstPage << 16
	capacity := sh.hashPageCount * slotsPerHashPage
	if lastID >= firstID+capacity {
		return capacity
	}
	low := firstID
	if low < 1 {
		low = 1
	}
	if lastID < low {
		return 0
	}
	return lastID - low + 1
}

// Upsert updates one existing record. It shares the batch implementation and
// allocates nothing.
func (st *Store) Upsert(id common.ImageID, rec Record) error {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.singleWire[0] = Update{ImageID: id, Record: rec}
	return st.upsertLocked(st.singleWire[:])
}

// UpsertBatch updates up to maxBatchRecords existing records as one logical
// transaction: one redo WAL frame, grouped sequence transitions, and a single
// generation advancement. Duplicate ImageIDs are allowed; the last update in
// the batch wins.
func (st *Store) UpsertBatch(updates []Update) error {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.upsertLocked(updates)
}

func (st *Store) upsertLocked(updates []Update) error {
	if st.closed.Load() {
		return ErrClosed
	}
	if len(updates) == 0 {
		return nil
	}
	if len(updates) > maxBatchRecords {
		return ErrBatchTooLarge
	}
	if st.generation == ^uint64(0) {
		return ErrOverflow
	}
	committed := st.committedAtomic.Load()
	for i := range updates {
		if updates[i].ImageID == 0 || uint64(updates[i].ImageID) > committed {
			return ErrInvalidID
		}
	}
	base := st.generation
	next := base + 1
	if st.journal != nil {
		if err := st.journal.appendBatch(base, next, updates, st.config.Durability == common.DurabilitySync); err != nil {
			return err
		}
	}
	st.applyBatchLocked(updates)
	st.generation = next
	st.gen.Store(next)
	return nil
}

// applyBatchLocked applies all updates in place, grouping consecutive records
// that share a sequence block so the block's seqlock toggles once per run
// rather than once per record.
//
// Grouping uses the caller's order and does not sort. Benchmarks show sorting
// by ImageID helps only already-local batches (roughly 8%) and costs ~34% on
// random batches, where every record is in a different block and grouping buys
// nothing. Consecutive-run grouping captures local and sequential batches at no
// cost and never regresses random ones. It is a single linear pass with no
// allocation, map, or temporary index.
func (st *Store) applyBatchLocked(updates []Update) {
	i := 0
	for i < len(updates) {
		page := hashPageOf(uint64(updates[i].ImageID))
		block := st.geo.blockOf(slotOf(uint64(updates[i].ImageID)))
		j := i + 1
		for j < len(updates) {
			id := uint64(updates[j].ImageID)
			if hashPageOf(id) != page || st.geo.blockOf(slotOf(id)) != block {
				break
			}
			j++
		}
		st.applyBlockGroupLocked(updates[i:j])
		i = j
	}
}

// applyBlockGroupLocked toggles one sequence block around a non-empty group of
// updates that all share the same hash page and sequence block.
func (st *Store) applyBlockGroupLocked(group []Update) {
	page := hashPageOf(uint64(group[0].ImageID))
	sh := st.shards[page/st.pagesPerShard]
	pi := page - sh.firstPage
	seq := sh.seqPtr(pi, st.geo.blockOf(slotOf(uint64(group[0].ImageID))))
	atomic.AddUint64(seq, 1) // odd
	for k := range group {
		st.applyPointNoSeq(sh, pi, slotOf(uint64(group[k].ImageID)), group[k].Record)
	}
	atomic.AddUint64(seq, 1) // even
}

// applyPointNoSeq writes the canonical columns atomically. The caller owns the
// block seqlock.
func (st *Store) applyPointNoSeq(sh *shard, pi uint64, slot int, rec Record) {
	atomic.StoreUint64(sh.phash(pi, slot), rec.PHash)
	words := hashWords(&rec.PDQ)
	for k := 0; k < 4; k++ {
		atomic.StoreUint64(sh.pdqWord(pi, slot, k), words[k])
	}
	st.storeQuality(sh, pi, slot, rec.Quality)
}

// storeQuality updates the 1-byte quality column via an aligned 8-byte atomic
// read-modify-write. The writer is serialized, so there are no lost updates.
func (st *Store) storeQuality(sh *shard, pi uint64, slot int, quality uint8) {
	shift := qualityShift(slot)
	qw := sh.qualityWord(pi, slot)
	atomic.StoreUint64(qw, (atomic.LoadUint64(qw)&^(0xff<<shift))|(uint64(quality)<<shift))
}

// applyJournalUpdate rewrites a canonical tuple during recovery. Idempotent.
func (st *Store) applyJournalUpdate(id, phash uint64, pdq *PDQHash, quality uint8) error {
	page := hashPageOf(id)
	index := page / st.pagesPerShard
	if id == 0 || id > st.committed || index >= uint64(len(st.shards)) {
		return ErrCorrupt
	}
	sh := st.shards[index]
	pi := page - sh.firstPage
	st.applyPointNoSeq(sh, pi, slotOf(id), Record{PHash: phash, PDQ: *pdq, Quality: quality})
	return nil
}

// Sync publishes and flushes the durable state, then truncates the journal.
func (st *Store) Sync() error {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.closed.Load() {
		return ErrClosed
	}
	return st.persistLocked()
}

// persistLocked makes the current generation durable in commit order:
//
//	canonical data (already written in place)
//	    -> flush canonical mmap
//	    -> write new shard superblocks
//	    -> flush shard metadata
//	    -> publish the manifest LAST
//	    -> sync the directory
//	    -> reset the WAL
//
// The manifest is the final durable commit marker, so it can never claim a
// generation whose canonical data or shard metadata is not durable.
func (st *Store) persistLocked() error {
	for _, sh := range st.shards {
		if err := sh.mapped.Sync(); err != nil {
			return err
		}
	}
	for _, sh := range st.shards {
		sh.generation = st.generation
		if err := sh.writeMeta(); err != nil {
			return err
		}
	}
	for _, sh := range st.shards {
		if err := sh.mapped.Sync(); err != nil {
			return err
		}
	}
	if err := st.writeManifestLocked(); err != nil {
		return err
	}
	if err := storage.SyncDir(st.dir); err != nil {
		return err
	}
	if st.journal != nil {
		return st.journal.reset()
	}
	return nil
}
