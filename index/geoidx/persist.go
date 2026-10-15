package geoidx

import (
	"cmp"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"slices"

	schema "github.com/evanoberholster/imageIdx/schema"
	"github.com/evanoberholster/imageIdx/internal/pagealloc"
	"github.com/evanoberholster/imageIdx/internal/storage"
)

// persistStage identifies a persistence step for the test-only hooks. Production
// leaves persistFail and persistCrash nil.
type persistStage uint8

const (
	persistCreate    persistStage = iota // extent allocation (pre-commit)
	persistGrow                          // file extension
	persistMmap                          // mapping creation
	persistWrite                         // shard data
	persistDirectory                     // directory
	persistSync                          // data sync
	persistCommit                        // before the inactive superblock write
	persistSuperSync                     // before the superblock sync
	persistPublish                       // before in-memory publication (crash point)
	persistRetire                        // before region retirement (crash point)
	persistReclaim                       // before reclamation (crash point)
	persistRemove                        // after reclamation (crash point)
)

// persistFail injects an error at a fallible persistence step; persistCrash runs
// at a step that cannot itself fail (publication, retirement, reclamation) so
// tests can block or terminate the process.
var (
	persistFail  func(persistStage) error
	persistCrash func(persistStage)
)

// commitOutcome is the result of attempting to commit a generation.
type commitOutcome = storage.CommitOutcome

const (
	// commitAborted means no bytes of the inactive slot were modified, so the new
	// allocations may be rolled back.
	commitAborted = storage.CommitAborted
	// commitDurable means the new generation is written and synchronized, so it
	// must be published.
	commitDurable = storage.CommitDurable
	// commitUncertain means the inactive slot may have been modified but
	// durability is unconfirmed; the new extents must be protected and the attempt
	// retried.
	commitUncertain = storage.CommitUncertain
)

// writeSuperblockSlotFunc is a test seam for the superblock slot write;
// production copies the slot into the mapping. The production write is a single
// copy that cannot report a partial write, so the caller treats any write error as
// an uncertain commit.
var writeSuperblockSlotFunc = func(arena []byte, offset uint64, slot []byte) error {
	copy(arena[offset:offset+storage.SuperblockSlotBytes], slot)
	return nil
}

// syncArenaFunc is a test seam for the durability barrier.
var syncArenaFunc = func(mf *storage.MappedFile) error { return mf.Sync() }

func failStage(stage persistStage) error {
	if persistFail == nil {
		return nil
	}
	return persistFail(stage)
}

func crashStage(stage persistStage) {
	if persistCrash != nil {
		persistCrash(stage)
	}
}

const (
	geoidxFile           = "geoidx.dat"
	geoidxMagic   uint32 = 0x47454f32 // "GEO2"
	geoidxVersion uint16 = 2          // v2: reconstructible allocator, prior generation retained

	superblockSlots = 2
	superblockBytes = storage.SuperblockSlotBytes

	// secondSuperblockRef reserves block 1 (the second superblock slot); block 0
	// is reserved as the null Ref by pagealloc.
	secondSuperblockRef pagealloc.Ref = 1

	// superblockPayloadBytes is dirOffset(8)+dirCount(4)+reserved(4)+
	// totalEntries(8)+liveBytes(8)+arenaBytes(8).
	superblockPayloadBytes = 40

	// dirEntryBytes is one shard reference: firstKey(8)+firstID(8)+offset(8)+
	// count(4)+keyCRC(4)+idCRC(4)+reserved(4)+minID(8)+maxID(8).
	dirEntryBytes = 56

	lockName = "geoidx.lock"

	// growAlignment keeps arena growth coarse.
	growAlignment = 4 << 20
)

// region is one mmap of the single file. The current region receives new
// writes; a retired region stays mapped until no published shard references it
// and prior readers have drained.
type region struct {
	mf   *storage.MappedFile
	data []byte
}

func createRegion(path string, size int64) (*region, error) {
	mf, err := storage.CreateMapped(path, size)
	if err != nil {
		return nil, err
	}
	return &region{mf: mf, data: mf.Bytes()}, nil
}

func openRegion(path string) (*region, error) {
	mf, err := storage.OpenMapped(path)
	if err != nil {
		return nil, err
	}
	return &region{mf: mf, data: mf.Bytes()}, nil
}

func (r *region) close() {
	if r == nil || r.mf == nil {
		return
	}
	_ = r.mf.Close()
	r.mf, r.data = nil, nil
}

// extentRef identifies an allocated extent for the durable live set.
type extentRef struct {
	ref  pagealloc.Ref
	size uint32
}

// shardExtent returns the extent reference and payload size of a persistent shard.
func shardExtent(s *shard) extentRef {
	return extentRef{
		ref:  pagealloc.Ref(s.offset / uint64(pagealloc.MinBlockSize)),
		size: uint32(s.count()) * shardBytesPerRecord,
	}
}

// roundUp rounds v up to a multiple of align.
func roundUp(v, align uint64) uint64 {
	if align == 0 {
		return v
	}
	return (v + align - 1) / align * align
}

// encodeDirectory serializes shard references into a compact sorted directory.
func encodeDirectory(shards []*shard) []byte {
	buf := make([]byte, len(shards)*dirEntryBytes)
	for n, s := range shards {
		entry := buf[n*dirEntryBytes : (n+1)*dirEntryBytes]
		storage.EncodeShardDescriptor(entry, storage.ShardDescriptor{FirstKey: uint64(s.keys[0]), FirstID: uint64(s.ids[0]), Offset: s.offset, Count: uint32(s.count()), KeyCRC: s.keyCRC, IDCRC: s.idCRC, MinID: uint64(s.minID), MaxID: uint64(s.maxID)})
	}
	return buf
}

// decodeDirectory rebuilds shard views from a directory extent and validates the
// extent's placement (alignment, reserved region, bounds) and every referenced
// shard. Overlap between extents is detected when the allocator marks the live
// set.
func decodeDirectory(arena []byte, dirOffset uint64, dirCount uint32) ([]*shard, error) {
	if dirCount == 0 {
		return nil, nil
	}
	size := uint64(len(arena))
	length := uint64(dirCount) * dirEntryBytes
	// Subtraction-based bounds check: never compute dirOffset+length, which can
	// wrap for a hostile dirOffset.
	if dirOffset < 2*superblockBytes || dirOffset%uint64(pagealloc.MinBlockSize) != 0 {
		return nil, ErrCorrupt
	}
	if dirOffset > size || length > size-dirOffset {
		return nil, ErrCorrupt
	}
	buf := arena[dirOffset : dirOffset+length]
	shards := make([]*shard, dirCount)
	for n := range shards {
		s, err := decodeDirEntry(arena, buf[n*dirEntryBytes:(n+1)*dirEntryBytes])
		if err != nil {
			return nil, err
		}
		shards[n] = s
	}
	return shards, nil
}

func decodeDirEntry(arena, entry []byte) (*shard, error) {
	d := storage.DecodeShardDescriptor(entry)
	firstKey, firstID, offset, count, keyCRC, idCRC, minID, maxID := d.FirstKey, d.FirstID, d.Offset, d.Count, d.KeyCRC, d.IDCRC, d.MinID, d.MaxID
	if count == 0 {
		return nil, ErrCorrupt
	}
	length := uint64(count) * shardBytesPerRecord
	// Checked arithmetic: offset + length must not overflow or leave the arena,
	// and length must fit the uint32 extent representation.
	if offset%uint64(pagealloc.MinBlockSize) != 0 || length > (uint64(1)<<32)-1 ||
		length > uint64(len(arena)) || offset > uint64(len(arena))-length {
		return nil, ErrCorrupt
	}
	keys, err := storage.View[S2Key](arena, offset, uint64(count), ErrCorrupt, ErrCorrupt)
	if err != nil {
		return nil, err
	}
	ids, err := storage.View[schema.ImageID](arena, offset+uint64(count)*8, uint64(count), ErrCorrupt, ErrCorrupt)
	if err != nil {
		return nil, err
	}
	s := &shard{
		keys:   keys,
		ids:    ids,
		offset: offset,
		length: uint32(length),
		minID:  schema.ImageID(minID),
		maxID:  schema.ImageID(maxID),
	}
	if checksumKeys(s) != keyCRC || checksumIDs(s) != idCRC {
		return nil, ErrCorrupt
	}
	if uint64(keys[0]) != firstKey || uint64(ids[0]) != firstID {
		return nil, ErrCorrupt
	}
	if validateBase(keys, ids, s.maxID) != nil {
		return nil, ErrCorrupt
	}
	if minOfIDs(ids) != s.minID {
		return nil, ErrCorrupt
	}
	s.keyCRC, s.idCRC = keyCRC, idCRC
	return s, nil
}

func encodeSuperblockPayload(dst []byte, arenaBytes, dirOffset uint64, dirCount uint32, totalEntries, liveBytes uint64) {
	binary.LittleEndian.PutUint64(dst[0:8], dirOffset)
	binary.LittleEndian.PutUint32(dst[8:12], dirCount)
	binary.LittleEndian.PutUint32(dst[12:16], 0)
	binary.LittleEndian.PutUint64(dst[16:24], totalEntries)
	binary.LittleEndian.PutUint64(dst[24:32], liveBytes)
	binary.LittleEndian.PutUint64(dst[32:40], arenaBytes)
}

func decodeSuperblockPayload(payload []byte) (arenaBytes, dirOffset uint64, dirCount uint32, totalEntries, liveBytes uint64, ok bool) {
	if len(payload) < superblockPayloadBytes {
		return 0, 0, 0, 0, 0, false
	}
	return binary.LittleEndian.Uint64(payload[32:40]),
		binary.LittleEndian.Uint64(payload[0:8]),
		binary.LittleEndian.Uint32(payload[8:12]),
		binary.LittleEndian.Uint64(payload[16:24]),
		binary.LittleEndian.Uint64(payload[24:32]),
		true
}

func superblockOffset(slot int) uint64 { return uint64(slot) * superblockBytes }

// allocatorFromLive builds a fresh allocator over arenaBytes marking the second
// superblock and every live extent used. live must be deduplicated by
// dedupExtents. This is the durable allocator state: recovery reconstructs it
// from the committed directory set. Overlapping extents fail Reserve.
func allocatorFromLive(arenaBytes uint64, live []extentRef) (*pagealloc.Allocator, error) {
	extents := make([]storage.Extent, len(live))
	for n, e := range live {
		extents[n] = storage.Extent{Ref: e.ref, Size: e.size}
	}
	alloc, err := storage.ReconstructAllocator(arenaBytes, extents)
	if err != nil {
		return nil, errors.Join(ErrCorrupt, err)
	}
	return alloc, nil
}

// dedupExtents collapses identical extent references and rejects a shared offset
// with mismatched lengths. Two generations legitimately share the exact same
// extent; anything else at the same offset is corruption.
func dedupExtents(live []extentRef) ([]extentRef, error) {
	extents := make([]storage.Extent, len(live))
	for n, e := range live {
		extents[n] = storage.Extent{Ref: e.ref, Size: e.size}
	}
	union, err := storage.UnionExtents(extents)
	if err != nil {
		return nil, errors.Join(ErrCorrupt, err)
	}
	out := live[:0]
	for _, e := range union {
		out = append(out, extentRef{ref: e.Ref, size: e.Size})
	}
	return out, nil
}

// liveOf returns the extents referenced by a shard set and its directory.
func liveOf(shards []*shard, dirOffset uint64, dirLength uint32) []extentRef {
	live := make([]extentRef, 0, len(shards)+1)
	for _, s := range shards {
		if s.offset != 0 {
			live = append(live, shardExtent(s))
		}
	}
	if dirLength > 0 {
		live = append(live, extentRef{ref: pagealloc.Ref(dirOffset / uint64(pagealloc.MinBlockSize)), size: dirLength})
	}
	return live
}

// NewFile creates a persistent, empty single-file index at exactly path. Its
// lock file is path+".lock". Unlike New, it does not require the containing
// directory to be empty, so several indexes can share one directory.
func NewFile(path string, config Config) (*Index, error) {
	return createPersistent(path, filepath.Dir(path), filepath.Base(path)+".lock", false, config)
}

// New creates a persistent, empty single-file index in dir. It writes
// <dir>/geoidx.dat and locks <dir>/geoidx.lock, matching NewFile with the
// historical default file name while preserving the exclusive-directory check.
func New(dir string, config Config) (*Index, error) {
	return createPersistent(filepath.Join(dir, geoidxFile), dir, lockName, true, config)
}

// createPersistent validates config, acquires the lock named lockFile inside
// lockDir, and creates the empty index file at path. When exclusiveDir is true
// the lock acquisition also requires lockDir to be empty (the historical New
// contract); otherwise any existing sibling files are left untouched.
func createPersistent(path, lockDir, lockFile string, exclusiveDir bool, config Config) (*Index, error) {
	cfg, err := config.withDefaults()
	if err != nil {
		return nil, err
	}
	if cfg.MaxFileBytes < cfg.MaxShardBytes {
		return nil, ErrInvalidConfig
	}
	if !exclusiveDir {
		if err := os.MkdirAll(lockDir, 0o755); err != nil {
			return nil, err
		}
	}
	lock, err := storage.AcquireLock(lockDir, lockFile, exclusiveDir)
	if err != nil {
		return nil, err
	}
	size := int64(roundUp(uint64(cfg.MaxFileBytes), growAlignment))
	r, err := createRegion(path, size)
	if err != nil {
		_ = storage.ReleaseLock(lock)
		return nil, err
	}
	index := &Index{
		config:     cfg,
		persistent: true,
		path:       path,
		lock:       lock,
		arena:      r,
		arenaBytes: uint64(len(r.data)),
	}
	if err := index.createEmptyLocked(); err != nil {
		r.close()
		_ = storage.ReleaseLock(lock)
		return nil, err
	}
	return index, nil
}

// OpenFile opens the persistent index at exactly path, created by NewFile. Its
// lock file is path+".lock".
func OpenFile(path string, config Config) (*Index, error) {
	return openPersistent(path, filepath.Dir(path), filepath.Base(path)+".lock", config)
}

// Open opens a persistent index created by New at <dir>/geoidx.dat, locking
// <dir>/geoidx.lock.
func Open(dir string, config Config) (*Index, error) {
	return openPersistent(filepath.Join(dir, geoidxFile), dir, lockName, config)
}

// openPersistent validates config, acquires the lock named lockFile inside
// lockDir, and opens the existing index file at path.
func openPersistent(path, lockDir, lockFile string, config Config) (*Index, error) {
	cfg, err := config.withDefaults()
	if err != nil {
		return nil, err
	}
	if cfg.MaxFileBytes < cfg.MaxShardBytes {
		return nil, ErrInvalidConfig
	}
	lock, err := storage.AcquireLock(lockDir, lockFile, false)
	if err != nil {
		return nil, err
	}
	if _, statErr := os.Stat(path); statErr != nil {
		_ = storage.ReleaseLock(lock)
		// A pre-shard multi-file directory (manifest + shard files) is a legacy
		// format; report it explicitly rather than reinterpreting it.
		if _, legacyErr := os.Stat(filepath.Join(filepath.Dir(path), "manifest")); legacyErr == nil {
			return nil, ErrLegacyFormat
		}
		return nil, ErrCorrupt
	}
	r, err := openRegion(path)
	if err != nil {
		_ = storage.ReleaseLock(lock)
		return nil, err
	}
	index := &Index{
		config:     cfg,
		persistent: true,
		path:       path,
		lock:       lock,
		arena:      r,
		arenaBytes: uint64(len(r.data)),
	}
	if err := index.loadLocked(); err != nil {
		r.close()
		_ = storage.ReleaseLock(lock)
		return nil, err
	}
	return index, nil
}

func (i *Index) createEmptyLocked() error {
	alloc, err := allocatorFromLive(i.arenaBytes, nil)
	if err != nil {
		return err
	}
	i.alloc = alloc
	i.live = nil
	i.dirOffset, i.dirLength = 0, 0
	i.gen = 1
	outcome, err := i.commit(0, i.gen, 0, 0, 0, 0, i.arenaBytes)
	if err != nil {
		return err
	}
	if outcome != commitDurable {
		return errors.New("geoidx: initial superblock not durable")
	}
	i.slot = 0
	i.publishLocked()
	return nil
}

// loadLocked selects the newest fully valid generation (falling back to an
// older one), reconstructs the allocator from the selected and next-older
// generations, and publishes the recovered view.
func (i *Index) loadLocked() error {
	arena := i.arena.data
	if uint64(len(arena)) < 2*superblockBytes {
		return ErrCorrupt
	}
	type candidate struct {
		slot    int
		gen     uint64
		payload []byte
	}
	var candidates []candidate
	for slot := 0; slot < superblockSlots; slot++ {
		off := superblockOffset(slot)
		if off+superblockBytes > uint64(len(arena)) {
			continue
		}
		// A v1 superblock means the older single-file layout; rebuild required.
		if _, _, ok := storage.DecodeSuperblock(arena[off:off+superblockBytes], geoidxMagic, 1); ok {
			return ErrLegacyFormat
		}
		header, payload, ok := storage.DecodeSuperblock(arena[off:off+superblockBytes], geoidxMagic, geoidxVersion)
		if !ok {
			continue
		}
		candidates = append(candidates, candidate{slot: slot, gen: header.Generation, payload: payload})
	}
	slices.SortFunc(candidates, func(a, b candidate) int { return cmp.Compare(b.gen, a.gen) })
	fileBytes := uint64(len(arena))
	for ci, c := range candidates {
		shards, dirOffset, dirCount, err := decodeGeneration(arena, c.payload, fileBytes)
		if err != nil {
			continue
		}
		live := liveOf(shards, dirOffset, uint32(dirCount)*dirEntryBytes)
		// Retain the next-older valid generation so it stays recoverable.
		for _, older := range candidates[ci+1:] {
			if older.gen >= c.gen {
				continue
			}
			oldShards, oldDir, oldCount, oerr := decodeGeneration(arena, older.payload, fileBytes)
			if oerr != nil {
				continue
			}
			live = append(live, liveOf(oldShards, oldDir, uint32(oldCount)*dirEntryBytes)...)
			break
		}
		live, derr := dedupExtents(live)
		if derr != nil {
			continue
		}
		alloc, aerr := allocatorFromLive(fileBytes, live)
		if aerr != nil {
			continue
		}
		i.alloc = alloc
		i.live = live
		i.shards = shards
		i.dirOffset, i.dirLength = dirOffset, uint32(dirCount)*dirEntryBytes
		i.gen = c.gen
		i.slot = c.slot
		i.publishLocked()
		return nil
	}
	return ErrCorrupt
}

// decodeGeneration validates one superblock payload and its directory.
func decodeGeneration(arena, payload []byte, fileBytes uint64) ([]*shard, uint64, uint32, error) {
	arenaBytes, dirOffset, dirCount, totalEntryCount, _, ok := decodeSuperblockPayload(payload)
	if !ok || arenaBytes > fileBytes {
		return nil, 0, 0, ErrCorrupt
	}
	shards, err := decodeDirectory(arena, dirOffset, dirCount)
	if err != nil {
		return nil, 0, 0, err
	}
	if !validateDirectory(shards) {
		return nil, 0, 0, ErrCorrupt
	}
	if uint64(totalEntries(shards)) != totalEntryCount {
		return nil, 0, 0, ErrCorrupt
	}
	return shards, dirOffset, dirCount, nil
}

// commit writes the inactive superblock slot and synchronizes it. It reports
// commitAborted only when no bytes of the slot were modified (an encoding failure
// or the pre-write hook), commitUncertain when the slot may have been modified but
// durability is unconfirmed (a write or sync error), and commitDurable on success.
func (i *Index) commit(slot int, generation, dirOffset uint64, dirCount uint32, totalEntries, liveBytes, arenaBytes uint64) (commitOutcome, error) {
	var payload [superblockPayloadBytes]byte
	encodeSuperblockPayload(payload[:], arenaBytes, dirOffset, dirCount, totalEntries, liveBytes)
	return storage.CommitSuperblock(storage.SuperblockHeader{
		Magic:      geoidxMagic,
		Version:    geoidxVersion,
		Generation: generation,
	}, payload[:], func(data []byte) error {
		return writeSuperblockSlotFunc(i.arena.data, superblockOffset(slot), data)
	}, func() error {
		if err := failStage(persistSuperSync); err != nil {
			return err
		}
		return syncArenaFunc(i.arena.mf)
	})
}

// liveBytes sums the shard payload bytes.
func liveBytes(shards []*shard) uint64 {
	var total uint64
	for _, s := range shards {
		total += uint64(s.count()) * shardBytesPerRecord
	}
	return total
}

// shardPlan is one output slot of a compaction.
type shardPlan struct {
	keep *shard
	keys []S2Key
	ids  []schema.ImageID
}

func planBytes(p shardPlan) int64 {
	if p.keep != nil {
		return p.keep.bytes()
	}
	return int64(len(p.keys)) * shardBytesPerRecord
}

func planArrays(p shardPlan) ([]S2Key, []schema.ImageID) {
	if p.keep != nil {
		return p.keep.keys, p.keep.ids
	}
	return p.keys, p.ids
}

// prepareShards selects the affected shards for a frozen delta and rebuilds only
// those. After a growth the whole directory is rewritten once (migrateAll) so the
// old mapping can be released.
func (i *Index) prepareShards(current []*shard, frozen []mutation) ([]shardPlan, error) {
	adds := sortedByKey(frozen)
	shadow := newMutationSet(frozen, totalEntries(current))
	if len(current) == 0 {
		return shardsFromAdds(adds, i.config), nil
	}
	addSlices, dirty := partitionAdds(current, adds)

	if i.migrateAll {
		for j := range dirty {
			dirty[j] = true
		}
	} else {
		// A shard is affected by a delete or move only when one of its base IDs
		// is named by the delta. The delta's [min,max] ImageID prunes shards
		// whose own [minID,maxID] does not overlap.
		deltaMin, deltaMax := deltaIDRange(frozen)
		scanned := 0
		for j, s := range current {
			if dirty[j] {
				continue
			}
			if deltaMax < s.minID || deltaMin > s.maxID {
				continue
			}
			scanned++
			for _, id := range s.ids {
				if shadow.touched(id) {
					dirty[j] = true
					break
				}
			}
		}
		i.lastScannedShards = scanned
	}

	targetRecords := recordsForBytes(i.config.TargetShardBytes)
	plans := make([]shardPlan, 0, len(current)+1)
	for j, s := range current {
		if !dirty[j] {
			plans = append(plans, shardPlan{keep: s})
			continue
		}
		shardAdds := addSlices[j]
		count := exactCount(s.keys, s.ids, shadow, shardAdds)
		if count == 0 {
			continue
		}
		keys := make([]S2Key, count)
		ids := make([]schema.ImageID, count)
		written := mergeInto(keys, ids, s.keys, s.ids, shadow, shardAdds)
		if bounds := splitBounds(written, targetRecords); bounds == nil {
			plans = append(plans, shardPlan{keys: keys[:written], ids: ids[:written]})
		} else {
			for _, b := range bounds {
				plans = append(plans, shardPlan{keys: keys[b[0]:b[1]], ids: ids[b[0]:b[1]]})
			}
		}
	}
	return coalescePlans(plans, i.config), nil
}

// deltaIDRange returns the min and max ImageID named by the delta. An empty
// delta yields a range that never prunes.
func deltaIDRange(frozen []mutation) (schema.ImageID, schema.ImageID) {
	if len(frozen) == 0 {
		return schema.ImageID(^uint64(0)), 0
	}
	minID, maxID := frozen[0].id, frozen[0].id
	for _, m := range frozen[1:] {
		if m.id < minID {
			minID = m.id
		}
		if m.id > maxID {
			maxID = m.id
		}
	}
	return minID, maxID
}

// shardsFromAdds builds the initial directory directly from an insertion list
// sorted by (key, id), splitting into target-sized shards.
func shardsFromAdds(adds []mutation, cfg Config) []shardPlan {
	if len(adds) == 0 {
		return nil
	}
	keys := make([]S2Key, len(adds))
	ids := make([]schema.ImageID, len(adds))
	for n, m := range adds {
		keys[n] = m.key
		ids[n] = m.id
	}
	bounds := splitBounds(len(adds), recordsForBytes(cfg.TargetShardBytes))
	if bounds == nil {
		return []shardPlan{{keys: keys, ids: ids}}
	}
	plans := make([]shardPlan, 0, len(bounds))
	for _, b := range bounds {
		plans = append(plans, shardPlan{keys: keys[b[0]:b[1]], ids: ids[b[0]:b[1]]})
	}
	return plans
}

// coalescePlans merges adjacent plans while their combined payload fits the
// target and at least one is below the minimum.
func coalescePlans(plans []shardPlan, cfg Config) []shardPlan {
	out := make([]shardPlan, 0, len(plans))
	for _, p := range plans {
		if n := len(out); n > 0 {
			last := out[n-1]
			la, lb := planBytes(last), planBytes(p)
			if la+lb <= cfg.TargetShardBytes && (la < cfg.MinShardBytes || lb < cfg.MinShardBytes) {
				out[n-1] = concatPlans(last, p)
				continue
			}
		}
		out = append(out, p)
	}
	return out
}

// concatPlans builds a single new-content plan from two adjacent plans.
func concatPlans(a, b shardPlan) shardPlan {
	ak, ai := planArrays(a)
	bk, bi := planArrays(b)
	keys := make([]S2Key, 0, len(ak)+len(bk))
	keys = append(keys, ak...)
	keys = append(keys, bk...)
	ids := make([]schema.ImageID, 0, len(ai)+len(bi))
	ids = append(ids, ai...)
	ids = append(ids, bi...)
	return shardPlan{keys: keys, ids: ids}
}

// materialize turns plans into shards, writing new content into freshly
// allocated in-file extents. It returns pagealloc.ErrOutOfSpace so the caller
// can grow the arena and retry.
func (i *Index) materialize(plans []shardPlan, gen uint64) ([]*shard, []extentRef, error) {
	shards := make([]*shard, 0, len(plans))
	allocated := make([]extentRef, 0, len(plans))
	for _, p := range plans {
		if p.keep != nil {
			shards = append(shards, p.keep)
			continue
		}
		if len(p.keys) == 0 {
			continue
		}
		if !i.persistent {
			shards = append(shards, &shard{
				keys: p.keys, ids: p.ids, gen: gen,
				minID: minOfIDs(p.ids), maxID: maxOfIDs(p.ids),
			})
			continue
		}
		size := uint32(len(p.keys)) * shardBytesPerRecord
		ref, capacity, err := i.alloc.Alloc(size)
		if err != nil {
			i.rollback(allocated)
			return nil, nil, err
		}
		offset := uint64(ref) * uint64(pagealloc.MinBlockSize)
		data := i.arena.data
		keys, err := storage.View[S2Key](data, offset, uint64(len(p.keys)), ErrCorrupt, ErrCorrupt)
		if err != nil {
			i.rollback(append(allocated, extentRef{ref, capacity}))
			return nil, nil, err
		}
		ids, err := storage.View[schema.ImageID](data, offset+uint64(len(p.keys))*8, uint64(len(p.keys)), ErrCorrupt, ErrCorrupt)
		if err != nil {
			i.rollback(append(allocated, extentRef{ref, capacity}))
			return nil, nil, err
		}
		copy(keys, p.keys)
		copy(ids, p.ids)
		s := &shard{
			keys: keys, ids: ids, offset: offset, length: capacity, gen: gen,
			minID: minOfIDs(ids), maxID: maxOfIDs(ids), region: i.arena,
		}
		s.keyCRC, s.idCRC = checksumKeys(s), checksumIDs(s)
		shards = append(shards, s)
		allocated = append(allocated, extentRef{ref, capacity})
	}
	return shards, allocated, nil
}

// rollback frees extents allocated for an aborted compaction. The extents were
// just allocated by this compaction, so Free is infallible by construction; the
// caller already holds the primary error being unwound.
func (i *Index) rollback(allocated []extentRef) {
	for _, e := range allocated {
		_ = i.alloc.Free(e.ref, e.size)
	}
}

// writeDirectory writes a new directory extent for shards.
func (i *Index) writeDirectory(shards []*shard, allocated *[]extentRef) (uint64, uint32, error) {
	if len(shards) == 0 {
		return 0, 0, nil
	}
	buf := encodeDirectory(shards)
	ref, capacity, err := i.alloc.Alloc(uint32(len(buf)))
	if err != nil {
		return 0, 0, err
	}
	*allocated = append(*allocated, extentRef{ref, capacity})
	offset := uint64(ref) * uint64(pagealloc.MinBlockSize)
	copy(i.arena.data[offset:offset+uint64(len(buf))], buf)
	return offset, uint32(len(buf)), nil
}

// growArena extends the file, maps a new region, retires the old mapping, and
// rebuilds the allocator over the larger arena with the durable live set. It
// never remaps in place, so existing reader slices stay valid.
func (i *Index) growArena(need uint64) error {
	target := i.arenaBytes * 2
	if minTarget := i.arenaBytes + 2*need; minTarget > target {
		target = minTarget
	}
	target = roundUp(target, growAlignment)
	if target <= i.arenaBytes {
		target = i.arenaBytes + growAlignment
	}
	path := i.path
	if err := failStage(persistGrow); err != nil {
		return err
	}
	if err := os.Truncate(path, int64(target)); err != nil {
		return err
	}
	if err := failStage(persistMmap); err != nil {
		return err
	}
	nr, err := openRegion(path)
	if err != nil {
		return err
	}
	newBytes := uint64(len(nr.data))
	// Rebuild the allocator over the larger arena BEFORE swapping it in, so a
	// failed rebuild leaves the index's mapping and allocator consistent and the
	// growth can be retried safely. The durable live set (both recoverable
	// generations) is preserved, so no committed extent is freed by migration.
	alloc, err := allocatorFromLive(newBytes, i.live)
	if err != nil {
		nr.close()
		return err
	}
	old := i.arena
	i.arena = nr
	i.arenaBytes = newBytes
	i.retired = append(i.retired, old)
	i.alloc = alloc
	i.migrateAll = true
	return nil
}

// releaseRetiredRegions unmaps retired mappings that no published shard
// references. Callers have already drained prior readers.
func (i *Index) releaseRetiredRegions() {
	if len(i.retired) == 0 {
		return
	}
	used := make(map[*region]struct{}, 4)
	for _, s := range i.shards {
		if s.region != nil {
			used[s.region] = struct{}{}
		}
	}
	keep := i.retired[:0]
	for _, r := range i.retired {
		if _, ok := used[r]; ok {
			keep = append(keep, r)
			continue
		}
		r.close()
	}
	i.retired = keep
}

// runCompaction advances one compaction through its phases.
func (i *Index) runCompaction() {
	i.mu.Lock()
	snapshot := i.shards
	frozen := i.frozen
	i.mu.Unlock()

	gen := i.gen + 1
	if err := failStage(persistCreate); err != nil {
		i.finishCompaction(err)
		return
	}
	if !i.persistent {
		plans, err := i.prepareShards(snapshot, frozen)
		if err != nil {
			i.finishCompaction(err)
			return
		}
		newShards, _, err := i.materialize(plans, gen)
		if err != nil {
			i.finishCompaction(err)
			return
		}
		i.publishResult(newShards, gen)
		i.finishReclaim()
		return
	}

	var (
		newShards []*shard
		allocated []extentRef
		dirOffset uint64
		dirLength uint32
	)
	for attempt := 0; ; attempt++ {
		plans, err := i.prepareShards(snapshot, frozen)
		if err != nil {
			i.finishCompaction(err)
			return
		}
		if err := failStage(persistWrite); err != nil {
			i.finishCompaction(err)
			return
		}
		newShards, allocated, err = i.materialize(plans, gen)
		if err != nil {
			if errors.Is(err, pagealloc.ErrOutOfSpace) && attempt < 8 {
				if gerr := i.growArena(uint64(i.config.MaxShardBytes)); gerr != nil {
					i.finishCompaction(gerr)
					return
				}
				continue
			}
			i.finishCompaction(err)
			return
		}
		if err := failStage(persistDirectory); err != nil {
			i.rollback(allocated)
			i.finishCompaction(err)
			return
		}
		dirOffset, dirLength, err = i.writeDirectory(newShards, &allocated)
		if err != nil {
			if errors.Is(err, pagealloc.ErrOutOfSpace) && attempt < 8 {
				i.rollback(allocated)
				if gerr := i.growArena(uint64(i.config.MaxShardBytes)); gerr != nil {
					i.finishCompaction(gerr)
					return
				}
				continue
			}
			i.rollback(allocated)
			i.finishCompaction(err)
			return
		}
		break
	}

	if err := failStage(persistSync); err != nil {
		i.rollback(allocated)
		i.finishCompaction(err)
		return
	}
	if err := i.arena.mf.Sync(); err != nil {
		i.rollback(allocated)
		i.finishCompaction(err)
		return
	}

	// Durable live set: this generation plus the previously committed one, so the
	// prior generation stays recoverable until its slot is overwritten.
	combined, err := dedupExtents(append(liveOf(newShards, dirOffset, dirLength), liveOf(snapshot, i.dirOffset, i.dirLength)...))
	if err != nil {
		i.rollback(allocated)
		i.finishCompaction(err)
		return
	}
	nextAlloc, err := allocatorFromLive(i.arenaBytes, combined)
	if err != nil {
		i.rollback(allocated)
		i.finishCompaction(err)
		return
	}

	i.mu.Lock()
	if cerr := failStage(persistCommit); cerr != nil {
		i.mu.Unlock()
		i.rollback(allocated)
		i.finishCompaction(cerr)
		return
	}
	outcome, cerr := i.commit(i.slot^1, gen, dirOffset, uint32(len(newShards)), uint64(totalEntries(newShards)), liveBytes(newShards), i.arenaBytes)
	if outcome != commitDurable {
		if outcome == commitUncertain {
			// The slot may have been modified but durability is unknown. Keep the
			// previous in-memory generation authoritative, retain the frozen
			// snapshot so the caller can retry, and protect the union of this
			// attempt's extents, the previous generation's extents, and any prior
			// uncertain candidates. Rule: an uncertain candidate becomes
			// reclaimable only when a later durable commit overwrites the slot it
			// was written to (the next successful commit resets i.live to exactly
			// the two recoverable generations). Until then it is never reused.
			protected := combined
			if u, uerr := dedupExtents(append(append([]extentRef(nil), combined...), i.live...)); uerr == nil {
				protected = u
			}
			i.live = protected
			if alloc, aerr := allocatorFromLive(i.arenaBytes, protected); aerr == nil {
				i.alloc = alloc
			}
			i.mu.Unlock()
			i.finishCompaction(cerr)
			return
		}
		// commitAborted: no bytes of the slot were modified.
		i.mu.Unlock()
		i.rollback(allocated)
		i.finishCompaction(cerr)
		return
	}
	crashStage(persistPublish)
	i.shards = newShards
	i.frozen = nil
	i.compactionErr = nil
	i.gen = gen
	i.slot ^= 1
	i.dirOffset, i.dirLength = dirOffset, dirLength
	i.alloc = nextAlloc
	i.live = combined
	i.migrateAll = false
	i.deltaMax = maxImageID(i.maxShardIDLocked(), nil, i.layers, i.active)
	i.publishLocked()
	i.mu.Unlock()

	i.finishReclaim()
}

// publishResult installs a new in-memory (heap) directory for a memory index.
func (i *Index) publishResult(newShards []*shard, gen uint64) {
	i.mu.Lock()
	i.shards = newShards
	i.frozen = nil
	i.compactionErr = nil
	i.gen = gen
	i.deltaMax = maxImageID(i.maxShardIDLocked(), nil, i.layers, i.active)
	i.publishLocked()
	i.mu.Unlock()
}

// finishReclaim drains prior readers and releases mappings no longer referenced.
func (i *Index) finishReclaim() {
	crashStage(persistReclaim)
	i.tracker.WaitForPriorReaders()
	crashStage(persistRetire)
	i.releaseRetiredRegions()
	crashStage(persistRemove)
	i.mu.Lock()
	i.compacting = false
	close(i.compactionDone)
	i.mu.Unlock()
}

// finishCompaction records a pre-commit failure, retains the frozen snapshot so
// pending mutations are not lost, and releases any writer waiting on compaction.
func (i *Index) finishCompaction(err error) {
	i.mu.Lock()
	i.compactionErr = err
	i.compacting = false
	close(i.compactionDone)
	i.mu.Unlock()
}

// Close releases resources. It does not checkpoint; call Checkpoint first to
// make pending mutations durable.
func (i *Index) Close() error {
	i.mu.Lock()
	if i.closed.Load() {
		i.mu.Unlock()
		return nil
	}
	i.closed.Store(true)
	for i.compacting {
		done := i.compactionDone
		i.mu.Unlock()
		<-done
		i.mu.Lock()
	}
	i.tracker.WaitForPriorReaders()
	var err error
	if i.arena != nil {
		err = i.arena.mf.Close()
		i.arena = nil
	}
	for _, r := range i.retired {
		r.close()
	}
	i.retired = nil
	i.shards = nil
	if i.lock != nil {
		if releaseErr := storage.ReleaseLock(i.lock); err == nil {
			err = releaseErr
		}
		i.lock = nil
	}
	i.mu.Unlock()
	return err
}

// StorageStats reports allocation and fragmentation metrics for a persistent
// index. A memory index reports zero values.
type StorageStats struct {
	ArenaBytes   uint64
	LiveBytes    uint64
	FreeBytes    uint64
	LargestFree  uint64
	FreeRuns     uint64
	RetiredBytes uint64
	Mappings     int
}

// StorageStats returns the current allocation and fragmentation metrics.
func (i *Index) StorageStats() StorageStats {
	i.mu.Lock()
	defer i.mu.Unlock()
	if !i.persistent || i.alloc == nil {
		return StorageStats{}
	}
	var live uint64
	for _, s := range i.shards {
		live += uint64(s.count()) * shardBytesPerRecord
	}
	return StorageStats{
		ArenaBytes:   i.arenaBytes,
		LiveBytes:    live,
		FreeBytes:    i.alloc.FreeBytes(),
		LargestFree:  i.alloc.LargestFreeBytes(),
		FreeRuns:     i.alloc.FreeRunCount(),
		RetiredBytes: i.alloc.RetiredBytes(),
		Mappings:     1 + len(i.retired),
	}
}

// sortedByKey returns a copy of the insert mutations sorted by (key, imageID).
func sortedByKey(muts []mutation) []mutation {
	out := make([]mutation, 0, len(muts))
	for n := range muts {
		if muts[n].add {
			out = append(out, muts[n])
		}
	}
	slices.SortFunc(out, func(a, b mutation) int {
		if order := cmp.Compare(a.key, b.key); order != 0 {
			return order
		}
		return cmp.Compare(a.id, b.id)
	})
	return out
}

// maxOfIDs returns the largest ImageID in ids, or 0 when empty.
func maxOfIDs(ids []schema.ImageID) schema.ImageID {
	var best schema.ImageID
	for _, id := range ids {
		if id > best {
			best = id
		}
	}
	return best
}

// minOfIDs returns the smallest ImageID in ids, or 0 when empty.
func minOfIDs(ids []schema.ImageID) schema.ImageID {
	if len(ids) == 0 {
		return 0
	}
	best := ids[0]
	for _, id := range ids[1:] {
		if id < best {
			best = id
		}
	}
	return best
}

// validateBase reports whether a shard's records are well-formed and its
// recorded maximum ImageID matches.
func validateBase(keys []S2Key, ids []schema.ImageID, maxID schema.ImageID) error {
	if !validateShardOrder(keys, ids) {
		return ErrCorrupt
	}
	if maxOfIDs(ids) != maxID {
		return ErrCorrupt
	}
	return nil
}

// exactCount returns the number of live (key, imageID) pairs after applying the
// delta. It mirrors mergeInto's traversal exactly.
func exactCount(baseKeys []S2Key, baseIDs []schema.ImageID, shadow mutationSet, adds []mutation) uint64 {
	count := uint64(0)
	baseIndex, addIndex := 0, 0
	for baseIndex < len(baseKeys) || addIndex < len(adds) {
		for baseIndex < len(baseKeys) && shadow.touched(baseIDs[baseIndex]) {
			baseIndex++
		}
		switch {
		case baseIndex == len(baseKeys) && addIndex == len(adds):
		case addIndex == len(adds):
			count++
			baseIndex++
		case baseIndex == len(baseKeys):
			count++
			addIndex++
		default:
			baseKey, baseID := baseKeys[baseIndex], baseIDs[baseIndex]
			addKey, addID := adds[addIndex].key, adds[addIndex].id
			switch {
			case baseKey < addKey || (baseKey == addKey && baseID < addID):
				count++
				baseIndex++
			case addKey < baseKey || (addKey == baseKey && addID < baseID):
				count++
				addIndex++
			default:
				count++
				baseIndex++
				addIndex++
			}
		}
	}
	return count
}

// mergeInto writes the retained base entries and additions into keys and ids in
// (key, id) order, skipping base entries shadowed by the delta.
func mergeInto(keys []S2Key, ids []schema.ImageID, baseKeys []S2Key, baseIDs []schema.ImageID, shadow mutationSet, adds []mutation) int {
	baseIndex, addIndex, out := 0, 0, 0
	for baseIndex < len(baseKeys) || addIndex < len(adds) {
		for baseIndex < len(baseKeys) && shadow.touched(baseIDs[baseIndex]) {
			baseIndex++
		}
		switch {
		case baseIndex == len(baseKeys) && addIndex == len(adds):
		case addIndex == len(adds):
			keys[out], ids[out] = baseKeys[baseIndex], baseIDs[baseIndex]
			out++
			baseIndex++
		case baseIndex == len(baseKeys):
			keys[out], ids[out] = adds[addIndex].key, adds[addIndex].id
			out++
			addIndex++
		default:
			baseKey, baseID := baseKeys[baseIndex], baseIDs[baseIndex]
			addKey, addID := adds[addIndex].key, adds[addIndex].id
			switch {
			case baseKey < addKey || (baseKey == addKey && baseID < addID):
				keys[out], ids[out] = baseKey, baseID
				out++
				baseIndex++
			case addKey < baseKey || (addKey == baseKey && addID < baseID):
				keys[out], ids[out] = addKey, addID
				out++
				addIndex++
			default:
				keys[out], ids[out] = baseKey, baseID
				out++
				baseIndex++
				addIndex++
			}
		}
	}
	return out
}
