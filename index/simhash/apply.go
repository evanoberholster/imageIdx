package simhash

import (
	"sync/atomic"

	common "github.com/evanoberholster/imageIdx/common"
	"github.com/evanoberholster/imageIdx/internal/storage"
)

// Append appends one record and returns its assigned ImageID.
func (st *Store) Append(rec Record) (common.ImageID, error) {
	return st.appendBatch([]Record{rec})
}

// AppendBatch appends records densely and returns the first assigned ImageID.
// The assigned IDs are first..first+len(records)-1.
func (st *Store) AppendBatch(recs []Record) (common.ImageID, error) {
	return st.appendBatch(recs)
}

func (st *Store) appendBatch(recs []Record) (common.ImageID, error) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.closed.Load() {
		return 0, ErrClosed
	}
	if len(recs) == 0 {
		return common.ImageID(st.committed + 1), nil
	}
	first, err := st.applyLocked(recs)
	if err != nil {
		return 0, err
	}
	st.generation++
	st.publishLocked()
	st.gen.Store(st.generation)
	if st.config.Durability == common.DurabilitySync {
		if err := st.persistLocked(); err != nil {
			return 0, err
		}
	}
	return common.ImageID(first), nil
}

// applyLocked writes the columns for a dense batch and advances the high-water
// mark. New slots are invisible until the caller publishes the view, so their
// column writes need no sequence protection.
func (st *Store) applyLocked(recs []Record) (uint64, error) {
	first := st.committed + 1
	for i := range recs {
		id := st.committed + 1
		hashPage := hashPageOf(id)
		sh, err := st.ensureShardLocked(hashPage)
		if err != nil {
			return 0, err
		}
		pi := hashPage - sh.firstPage
		slot := slotOf(id)
		words := hashWords(&recs[i].PDQ)
		*sh.phash(pi, slot) = recs[i].PHash
		for k := 0; k < 4; k++ {
			*sh.pdqWord(pi, slot, k) = words[k]
		}
		shift := qualityShift(slot)
		qw := sh.qualityWord(pi, slot)
		old := atomic.LoadUint64(qw)
		atomic.StoreUint64(qw, (old&^(0xff<<shift))|(uint64(recs[i].Quality)<<shift))
		st.committed = id
		sh.committed = id - (sh.firstPage << 16) + 1
		if sh.committed > sh.hashPageCount*slotsPerHashPage {
			sh.committed = sh.hashPageCount * slotsPerHashPage
		}
	}
	return first, nil
}

// Upsert journals and then applies an in-place point update under the block
// seqlock, and advances the published generation. The journal record is written
// (and fsynced under DurabilitySync) before the canonical columns change, so an
// interrupted update is completed by replay on the next Open.
func (st *Store) Upsert(id common.ImageID, rec Record) error {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.closed.Load() {
		return ErrClosed
	}
	if id == 0 || uint64(id) > st.committed {
		return ErrInvalidID
	}
	if st.journal != nil {
		jr := journalRecord{imageID: uint64(id), phash: rec.PHash, pdq: rec.PDQ, quality: rec.Quality}
		if err := st.journal.appendRecord(jr, st.config.Durability == common.DurabilitySync); err != nil {
			return err
		}
	}
	st.applyPointLocked(uint64(id), rec)
	st.generation++
	// An in-place update does not change the committed high-water mark or the
	// shard set, so only the published generation changes; no view allocation.
	st.gen.Store(st.generation)
	return nil
}

// applyPointLocked updates the canonical columns in place under the block
// seqlock. The caller holds the writer mutex.
func (st *Store) applyPointLocked(id uint64, rec Record) {
	hashPage := hashPageOf(id)
	slot := slotOf(id)
	sh := st.shards[hashPage/st.pagesPerShard]
	pi := hashPage - sh.firstPage
	seq := sh.seqPtr(pi, slot/st.geo.seqBlockRecords)
	atomic.AddUint64(seq, 1) // odd: update in progress
	atomic.StoreUint64(sh.phash(pi, slot), rec.PHash)
	words := hashWords(&rec.PDQ)
	for k := 0; k < 4; k++ {
		atomic.StoreUint64(sh.pdqWord(pi, slot, k), words[k])
	}
	shift := qualityShift(slot)
	qw := sh.qualityWord(pi, slot)
	atomic.StoreUint64(qw, (atomic.LoadUint64(qw)&^(0xff<<shift))|(uint64(rec.Quality)<<shift))
	atomic.AddUint64(seq, 1) // even: update complete
}

// applyJournalRecord rewrites a canonical tuple from a journal record during
// recovery. It is idempotent.
func (st *Store) applyJournalRecord(r journalRecord) error {
	hashPage := hashPageOf(r.imageID)
	slot := slotOf(r.imageID)
	index := hashPage / st.pagesPerShard
	if index >= uint64(len(st.shards)) {
		return ErrCorrupt
	}
	sh := st.shards[index]
	pi := hashPage - sh.firstPage
	atomic.StoreUint64(sh.phash(pi, slot), r.phash)
	words := hashWords(&r.pdq)
	for k := 0; k < 4; k++ {
		atomic.StoreUint64(sh.pdqWord(pi, slot, k), words[k])
	}
	shift := qualityShift(slot)
	qw := sh.qualityWord(pi, slot)
	atomic.StoreUint64(qw, (atomic.LoadUint64(qw)&^(0xff<<shift))|(uint64(r.quality)<<shift))
	return nil
}

// Sync publishes and flushes every shard superblock, the manifest, and the
// mappings, then truncates the update journal. It is the durable commit point
// for appends and point updates, matching index/radixhash's explicit checkpoint
// model.
func (st *Store) Sync() error {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.closed.Load() {
		return ErrClosed
	}
	return st.persistLocked()
}

// persistLocked writes all shard superblocks and the manifest, flushes them,
// and only then clears the journal, so canonical state is durable before replay
// records are discarded.
func (st *Store) persistLocked() error {
	for _, sh := range st.shards {
		sh.generation = st.generation
		if err := sh.writeMeta(); err != nil {
			return err
		}
	}
	if err := st.writeManifestLocked(); err != nil {
		return err
	}
	for _, sh := range st.shards {
		if err := sh.mapped.Sync(); err != nil {
			return err
		}
	}
	if err := storage.SyncDir(st.dir); err != nil {
		return err
	}
	if st.journal != nil {
		return st.journal.reset()
	}
	return nil
}
