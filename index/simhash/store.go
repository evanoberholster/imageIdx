package simhash

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"

	common "github.com/evanoberholster/imageIdx/common"
	"github.com/evanoberholster/imageIdx/internal/storage"
)

const (
	manifestMagic    uint32 = 0x534D414E // "SMAN"
	manifestVersion  uint16 = 1
	manifestFields          = 5 * 8
	manifestBytes           = 8 + manifestFields + 4
	manifestFilename        = "manifest"
)

// manifest is the store-level metadata and the authoritative committed
// generation. Shard superblocks are validated against it on open; a shard
// generation newer than the manifest is an interrupted commit and is not
// selected.
type manifest struct {
	pagesPerShard   uint64
	seqBlockRecords uint64
	committed       uint64
	generation      uint64
	shardCount      uint64
}

func encodeManifest(m manifest) []byte {
	buf := make([]byte, 0, manifestBytes)
	buf = binary.LittleEndian.AppendUint32(buf, manifestMagic)
	buf = binary.LittleEndian.AppendUint16(buf, manifestVersion)
	buf = binary.LittleEndian.AppendUint16(buf, 0)
	buf = binary.LittleEndian.AppendUint64(buf, m.pagesPerShard)
	buf = binary.LittleEndian.AppendUint64(buf, m.seqBlockRecords)
	buf = binary.LittleEndian.AppendUint64(buf, m.committed)
	buf = binary.LittleEndian.AppendUint64(buf, m.generation)
	buf = binary.LittleEndian.AppendUint64(buf, m.shardCount)
	return binary.LittleEndian.AppendUint32(buf, storage.Checksum(buf))
}

func decodeManifest(data []byte) (manifest, bool) {
	if len(data) != manifestBytes {
		return manifest{}, false
	}
	body := data[:len(data)-4]
	if binary.LittleEndian.Uint32(data[len(data)-4:]) != storage.Checksum(body) {
		return manifest{}, false
	}
	if binary.LittleEndian.Uint32(body[0:4]) != manifestMagic || binary.LittleEndian.Uint16(body[4:6]) != manifestVersion {
		return manifest{}, false
	}
	return manifest{
		pagesPerShard:   binary.LittleEndian.Uint64(body[8:16]),
		seqBlockRecords: binary.LittleEndian.Uint64(body[16:24]),
		committed:       binary.LittleEndian.Uint64(body[24:32]),
		generation:      binary.LittleEndian.Uint64(body[32:40]),
		shardCount:      binary.LittleEndian.Uint64(body[40:48]),
	}, true
}

func manifestPath(dir string) string { return filepath.Join(dir, manifestFilename) }

func shardPath(dir string, index uint64) string {
	return filepath.Join(dir, fmt.Sprintf("shard-%06d.bin", index))
}

func readManifest(dir string) (manifest, bool, error) {
	data, err := os.ReadFile(manifestPath(dir))
	if errors.Is(err, os.ErrNotExist) {
		return manifest{}, false, nil
	}
	if err != nil {
		return manifest{}, false, err
	}
	m, ok := decodeManifest(data)
	if !ok {
		return manifest{}, false, fmt.Errorf("%w: manifest", ErrCorrupt)
	}
	return m, true, nil
}

// shardFileSize returns header + pages*hashPageStride with an overflow check.
func shardFileSize(pagesPerShard uint64, geo geometry) (uint64, bool) {
	if geo.hashPageStride != 0 && pagesPerShard > (^uint64(0)-headerBytes)/geo.hashPageStride {
		return 0, false
	}
	return uint64(headerBytes) + pagesPerShard*geo.hashPageStride, true
}

// view is one immutable, atomically published reader view: the committed
// high-water mark, the format geometry, and the mapped shards. Sequence
// counters are runtime-only and are reset on open.
type view struct {
	generation    uint64
	committed     uint64
	pagesPerShard uint64
	geo           geometry
	shards        []*shard
}

func (v *view) shardForHashPage(hashPage uint64) (*shard, uint64) {
	sh := v.shards[hashPage/v.pagesPerShard]
	return sh, hashPage - sh.firstPage
}

// Store is a sharded, mmap-backed similarity store with lock-free readers and
// one serialized writer.
type Store struct {
	dir           string
	config        Config
	geo           geometry
	pagesPerShard uint64

	mu     sync.Mutex
	view   atomic.Pointer[view]
	closed atomic.Bool
	// tracker pins readers against shard unmap during Close.
	tracker storage.ReaderTracker

	// Writer-owned state, touched only under mu.
	shards     []*shard
	committed  uint64
	generation uint64
	journal    *journal
}

// Open opens or creates a store in dir. If dir has no manifest, an empty store
// is created with the geometry selected by config. It restores the committed
// generation from the manifest, validates every shard against it, resets the
// runtime sequence counters, then replays the update journal.
func Open(dir string, config Config) (*Store, error) {
	cfg, err := config.withDefaults()
	if err != nil {
		return nil, err
	}
	geo := newGeometry(cfg.SeqBlockRecords)
	size, ok := shardFileSize(1, geo)
	if !ok || cfg.MaxShardBytes < size {
		return nil, ErrInvalidConfig
	}
	pagesPerShard := (cfg.MaxShardBytes - headerBytes) / geo.hashPageStride
	if pagesPerShard == 0 {
		return nil, ErrInvalidConfig
	}
	if size, ok := shardFileSize(pagesPerShard, geo); !ok || size > cfg.MaxShardBytes {
		return nil, ErrInvalidConfig
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	st := &Store{dir: dir, config: cfg, geo: geo, pagesPerShard: pagesPerShard}

	mf, ok, err := readManifest(dir)
	if err != nil {
		return nil, err
	}
	if ok {
		if mf.seqBlockRecords != uint64(cfg.SeqBlockRecords) {
			return nil, fmt.Errorf("%w: sequence block size mismatch", ErrCorrupt)
		}
		st.pagesPerShard = mf.pagesPerShard
		st.committed = mf.committed
		st.generation = mf.generation
		for i := uint64(0); i < mf.shardCount; i++ {
			sh, err := st.openShard(i)
			if err != nil {
				_ = st.closeShards()
				return nil, err
			}
			st.shards = append(st.shards, sh)
		}
	} else {
		if err := st.writeManifestLocked(); err != nil {
			return nil, err
		}
	}
	if err := st.recoverJournal(); err != nil {
		_ = st.closeShards()
		return nil, err
	}
	// Sequence counters are ephemeral: after journal recovery, reset every
	// counter to an even value so a crash that left one odd cannot make a
	// reader spin forever.
	st.resetSequences()
	st.publishLocked()
	return st, nil
}

// requiredShardCommitted is the number of committed records shard index must
// contain given the manifest high-water mark.
func (st *Store) requiredShardCommitted(index uint64) uint64 {
	firstID := index * st.pagesPerShard * slotsPerHashPage
	if firstID >= st.committed {
		return 0
	}
	capacity := st.pagesPerShard * slotsPerHashPage
	lastID := firstID + capacity - 1
	if lastID > st.committed {
		lastID = st.committed
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

// openShard maps an existing shard file and selects the newest superblock whose
// generation was committed by the manifest (generation <= committedGeneration).
// It rejects a shard that does not cover the committed records.
func (st *Store) openShard(index uint64) (*shard, error) {
	mapped, err := storage.OpenMapped(shardPath(st.dir, index))
	if err != nil {
		return nil, err
	}
	sh := &shard{
		index:         index,
		mapped:        mapped,
		arena:         mapped.Bytes(),
		hashPageCount: st.pagesPerShard,
		stride:        st.geo.hashPageStride,
	}
	expected, ok := shardFileSize(st.pagesPerShard, st.geo)
	if !ok || uint64(len(sh.arena)) < expected {
		_ = mapped.Close()
		return nil, fmt.Errorf("%w: shard %d size", ErrCorrupt, index)
	}
	var (
		best shardMeta
		slot = -1
	)
	for cand := 0; cand < storage.SuperblockSlots; cand++ {
		off := cand * storage.SuperblockSlotBytes
		m, ok := decodeShardMeta(sh.arena[off : off+storage.SuperblockSlotBytes])
		if !ok {
			continue
		}
		if m.generation > st.generation {
			continue // never select metadata the manifest did not commit
		}
		if m.index != index || m.firstPage != index*st.pagesPerShard || m.hashPageCount != st.pagesPerShard {
			continue
		}
		if slot == -1 || m.generation > best.generation {
			best, slot = m, cand
		}
	}
	if slot == -1 {
		_ = mapped.Close()
		return nil, fmt.Errorf("%w: no committed shard superblock for shard %d", ErrCorrupt, index)
	}
	if best.committed < st.requiredShardCommitted(index) {
		_ = mapped.Close()
		return nil, fmt.Errorf("%w: shard %d missing committed records", ErrCorrupt, index)
	}
	sh.slot = slot
	sh.firstPage = best.firstPage
	sh.committed = best.committed
	sh.generation = best.generation
	return sh, nil
}

// createShardLocked creates and maps shard index and writes its first
// superblock.
func (st *Store) createShardLocked(index uint64) (*shard, error) {
	firstPage := index * st.pagesPerShard
	size, ok := shardFileSize(st.pagesPerShard, st.geo)
	if !ok {
		return nil, ErrInvalidConfig
	}
	mapped, err := storage.CreateMapped(shardPath(st.dir, index), int64(size))
	if err != nil {
		return nil, err
	}
	sh := &shard{
		index:         index,
		mapped:        mapped,
		arena:         mapped.Bytes(),
		firstPage:     firstPage,
		hashPageCount: st.pagesPerShard,
		stride:        st.geo.hashPageStride,
		slot:          1,
	}
	if err := sh.writeMeta(); err != nil {
		_ = mapped.Close()
		return nil, err
	}
	return sh, nil
}

// ensureShardLocked returns the shard covering hashPage, creating shards as
// needed.
func (st *Store) ensureShardLocked(hashPage uint64) (*shard, error) {
	index := hashPage / st.pagesPerShard
	for index >= uint64(len(st.shards)) {
		sh, err := st.createShardLocked(uint64(len(st.shards)))
		if err != nil {
			return nil, err
		}
		st.shards = append(st.shards, sh)
	}
	return st.shards[index], nil
}

func (st *Store) writeManifestLocked() error {
	data := encodeManifest(manifest{
		pagesPerShard:   st.pagesPerShard,
		seqBlockRecords: uint64(st.geo.seqBlockRecords),
		committed:       st.committed,
		generation:      st.generation,
		shardCount:      uint64(len(st.shards)),
	})
	return storage.WriteFileAtomic(st.dir, manifestFilename+".tmp", manifestFilename, data, 0o644)
}

func (st *Store) publishLocked() {
	st.view.Store(&view{
		generation:    st.generation,
		committed:     st.committed,
		pagesPerShard: st.pagesPerShard,
		geo:           st.geo,
		shards:        append([]*shard(nil), st.shards...),
	})
}

// resetSequences zeroes every sequence counter. Sequence counters are runtime
// synchronization, not persistent state: after a crash that left a counter odd,
// open must reset it to even so readers never spin forever.
func (st *Store) resetSequences() {
	for _, sh := range st.shards {
		sh.resetSequences(st.geo.seqBlocksPerPage)
	}
}

// recoverJournal replays valid point-update records into the canonical columns
// and repairs an interrupted tail.
func (st *Store) recoverJournal() error {
	path := filepath.Join(st.dir, journalFilename)
	j, err := openJournal(path)
	if err != nil {
		return err
	}
	st.journal = j
	end, err := replayJournal(path, func(r journalRecord) error {
		if r.imageID == 0 || r.imageID > st.committed {
			return fmt.Errorf("%w: journal id %d", ErrCorrupt, r.imageID)
		}
		return st.applyJournalRecord(r)
	})
	if err != nil {
		_ = j.close()
		return err
	}
	return j.wal.RepairTail(end)
}

func (st *Store) closeShards() error {
	var err error
	for _, sh := range st.shards {
		if e := sh.mapped.Close(); e != nil && err == nil {
			err = e
		}
	}
	return err
}

// Len returns the committed high-water mark: valid ImageIDs are 1..Len.
func (st *Store) Len() uint64 { return st.view.Load().committed }

// Generation returns the current published logical store version.
func (st *Store) Generation() uint64 { return st.view.Load().generation }

// PagesPerShard returns the configured hash pages per shard file.
func (st *Store) PagesPerShard() uint64 { return st.pagesPerShard }

// ShardCount returns the number of mapped shard files.
func (st *Store) ShardCount() int { return len(st.view.Load().shards) }

// Checkpoint persists every shard superblock and the manifest, flushes the
// mappings, and truncates the update journal. It is the durable commit point
// for appends and point updates.
func (st *Store) Checkpoint() error {
	return st.Sync()
}

// Close waits for in-flight readers and unmaps every shard and the journal. It
// does not checkpoint: like index/radixhash, explicit Sync/Checkpoint is
// required to make pending writes durable. It is safe to call more than once.
func (st *Store) Close() error {
	if !st.closed.CompareAndSwap(false, true) {
		return nil
	}
	st.tracker.WaitForPriorReaders()
	st.mu.Lock()
	defer st.mu.Unlock()
	var err error
	if st.journal != nil {
		if e := st.journal.close(); e != nil {
			err = e
		}
	}
	if e := st.closeShards(); e != nil && err == nil {
		err = e
	}
	return err
}

// Get returns the canonical record for id, validated by the block sequence.
func (st *Store) Get(id common.ImageID) (Record, bool) {
	token := st.tracker.Acquire()
	defer st.tracker.Release(token)
	v := st.view.Load()
	if id == 0 || uint64(id) > v.committed {
		return Record{}, false
	}
	hashPage := hashPageOf(uint64(id))
	slot := slotOf(uint64(id))
	sh, pi := v.shardForHashPage(hashPage)
	seq := sh.seqPtr(pi, slot/v.geo.seqBlockRecords)
	for {
		a := waitSeqEven(seq)
		var rec Record
		rec.PHash = atomic.LoadUint64(sh.phash(pi, slot))
		words := hashWords(&rec.PDQ)
		for k := 0; k < 4; k++ {
			words[k] = atomic.LoadUint64(sh.pdqWord(pi, slot, k))
		}
		rec.Quality = uint8(atomic.LoadUint64(sh.qualityWord(pi, slot)) >> qualityShift(slot))
		if atomic.LoadUint64(seq) == a {
			return rec, true
		}
	}
}
