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
		page := pageOf(id)
		sh, err := st.ensureShardLocked(page)
		if err != nil {
			return 0, err
		}
		pi := page - sh.firstPage
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
		if sh.committed > sh.pageCount*slotsPerPage {
			sh.committed = sh.pageCount * slotsPerPage
		}
	}
	return first, nil
}

// Upsert updates an existing record in place under the block seqlock. The
// pHash, PDQ, and quality columns are fixed addresses and are not copied. A
// 32-byte PDQ write is not atomic, so the odd/even sequence validates the tuple
// for concurrent readers.
func (st *Store) Upsert(id common.ImageID, rec Record) error {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.closed.Load() {
		return ErrClosed
	}
	if id == 0 || uint64(id) > st.committed {
		return ErrInvalidID
	}
	page := pageOf(uint64(id))
	slot := slotOf(uint64(id))
	index := page / st.pagesPerShard
	if index >= uint64(len(st.shards)) {
		return ErrCorrupt
	}
	sh := st.shards[index]
	pi := page - sh.firstPage
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
	return nil
}

// Sync publishes and flushes every shard superblock, the manifest, and the
// mappings, matching index/radixhash's explicit checkpoint model. Append and
// point updates are visible to concurrent readers immediately but become
// crash-durable only after Sync or Checkpoint (unless DurabilitySync is set).
func (st *Store) Sync() error {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.closed.Load() {
		return ErrClosed
	}
	return st.persistLocked()
}

// persistLocked writes all shard superblocks and the manifest, then flushes.
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
	return storage.SyncDir(st.dir)
}
