package geoidx

import (
	"cmp"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"slices"

	common "github.com/evanoberholster/imageIdx/common"
	"github.com/evanoberholster/imageIdx/internal/pagealloc"
	"github.com/evanoberholster/imageIdx/internal/storage"
)

// persistStage identifies a persistence step for the test-only persistFail
// seam. Production leaves persistFail nil.
type persistStage uint8

const (
	persistCreate persistStage = iota
	persistWrite
	persistSync
	persistCommit
	persistPublish
	persistReclaim
	persistRemove
)

// persistFail is a test seam; production leaves it nil.
var persistFail func(persistStage) error

// commitSuperblockFunc is a test seam for the durable superblock write;
// production copies the slot into the mapping (the following Sync makes it
// durable).
var commitSuperblockFunc = func(arena []byte, offset uint64, slot []byte) error {
	copy(arena[offset:offset+storage.SuperblockSlotBytes], slot)
	return nil
}

func failStage(stage persistStage) error {
	if persistFail == nil {
		return nil
	}
	return persistFail(stage)
}

const (
	geoidxFile           = "geoidx.dat"
	geoidxMagic   uint32 = 0x47454f32 // "GEO2"
	geoidxVersion uint16 = 1

	superblockSlots = 2
	superblockBytes = storage.SuperblockSlotBytes

	// directoryHeaderBlocks reserves block 1 (the second superblock slot) in the
	// allocator; block 0 is reserved as the null Ref by pagealloc.
	secondSuperblockRef pagealloc.Ref = 1

	// superblockPayloadBytes is the geoidx payload: dirOffset(8)+dirCount(4)+
	// reserved(4)+metaOffset(8)+metaLength(4)+reserved(4)+totalEntries(8)+
	// liveBytes(8)+arenaBytes(8).
	superblockPayloadBytes = 56

	// dirEntryBytes is one shard reference: firstKey(8)+firstID(8)+offset(8)+
	// count(4)+reserved(4)+minID(8)+maxID(8)+crc(4)+reserved(4).
	dirEntryBytes = 56

	lockName = "geoidx.lock"
)

// superblockOffset is the file offset of one superblock slot.
func superblockOffset(slot int) uint64 { return uint64(slot) * superblockBytes }

// extentRef identifies an allocated extent for rollback and retirement.
type extentRef struct {
	ref  pagealloc.Ref
	size uint32
}

// shardExtent returns the extent reference and payload size of a persistent shard.
func shardExtent(s *shard) extentRef {
	return extentRef{ref: pagealloc.Ref(s.offset / uint64(pagealloc.MinBlockSize)), size: uint32(s.count()) * shardBytesPerRecord}
}

// encodeDirectory serializes shard references into a compact sorted directory.
func encodeDirectory(shards []*shard) []byte {
	buf := make([]byte, len(shards)*dirEntryBytes)
	for n, s := range shards {
		entry := buf[n*dirEntryBytes : (n+1)*dirEntryBytes]
		binary.LittleEndian.PutUint64(entry[0:8], uint64(s.keys[0]))
		binary.LittleEndian.PutUint64(entry[8:16], uint64(s.ids[0]))
		binary.LittleEndian.PutUint64(entry[16:24], s.offset)
		binary.LittleEndian.PutUint32(entry[24:28], uint32(s.count()))
		binary.LittleEndian.PutUint32(entry[28:32], s.keyCRC)
		binary.LittleEndian.PutUint32(entry[32:36], s.idCRC)
		binary.LittleEndian.PutUint64(entry[40:48], uint64(s.minID))
		binary.LittleEndian.PutUint64(entry[48:56], uint64(s.maxID))
	}
	return buf
}

// decodeDirectory rebuilds shard views from a directory extent and validates
// every referenced extent against the allocator state.
func decodeDirectory(arena []byte, dirOffset uint64, dirCount uint32, alloc *pagealloc.Allocator) ([]*shard, error) {
	if dirCount == 0 {
		return nil, nil
	}
	length := uint64(dirCount) * dirEntryBytes
	if dirOffset+length > uint64(len(arena)) {
		return nil, ErrCorrupt
	}
	buf := arena[dirOffset : dirOffset+length]
	shards := make([]*shard, dirCount)
	for n := range shards {
		s, err := decodeDirEntry(arena, buf[n*dirEntryBytes:(n+1)*dirEntryBytes], alloc)
		if err != nil {
			return nil, err
		}
		shards[n] = s
	}
	return shards, nil
}

func decodeDirEntry(arena, entry []byte, alloc *pagealloc.Allocator) (*shard, error) {
	firstKey := binary.LittleEndian.Uint64(entry[0:8])
	firstID := binary.LittleEndian.Uint64(entry[8:16])
	offset := binary.LittleEndian.Uint64(entry[16:24])
	count := binary.LittleEndian.Uint32(entry[24:28])
	keyCRC := binary.LittleEndian.Uint32(entry[28:32])
	idCRC := binary.LittleEndian.Uint32(entry[32:36])
	minID := binary.LittleEndian.Uint64(entry[40:48])
	maxID := binary.LittleEndian.Uint64(entry[48:56])
	if count == 0 {
		return nil, ErrCorrupt
	}
	length := uint64(count) * shardBytesPerRecord
	if offset%uint64(pagealloc.MinBlockSize) != 0 || offset+length > uint64(len(arena)) {
		return nil, ErrCorrupt
	}
	if used, err := alloc.Used(pagealloc.Ref(offset/uint64(pagealloc.MinBlockSize)), uint32(length)); err != nil || !used {
		return nil, ErrCorrupt
	}
	keys, err := storage.View[S2Key](arena, offset, uint64(count), ErrCorrupt, ErrCorrupt)
	if err != nil {
		return nil, err
	}
	ids, err := storage.View[common.ImageID](arena, offset+uint64(count)*8, uint64(count), ErrCorrupt, ErrCorrupt)
	if err != nil {
		return nil, err
	}
	s := &shard{
		keys:   keys,
		ids:    ids,
		offset: offset,
		length: uint32(length),
		minID:  common.ImageID(minID),
		maxID:  common.ImageID(maxID),
	}
	if checksumKeys(s) != keyCRC || checksumIDs(s) != idCRC {
		return nil, ErrCorrupt
	}
	s.keyCRC, s.idCRC = keyCRC, idCRC
	if uint64(keys[0]) != firstKey || uint64(ids[0]) != firstID {
		return nil, ErrCorrupt
	}
	if validateBase(keys, ids, s.maxID) != nil {
		return nil, ErrCorrupt
	}
	if minOfIDs(ids) != s.minID {
		return nil, ErrCorrupt
	}
	return s, nil
}

func encodeSuperblockPayload(dst []byte, arenaBytes, dirOffset uint64, dirCount uint32, metaOffset uint64, metaLength uint32, totalEntries, liveBytes uint64) {
	binary.LittleEndian.PutUint64(dst[0:8], dirOffset)
	binary.LittleEndian.PutUint32(dst[8:12], dirCount)
	binary.LittleEndian.PutUint32(dst[12:16], 0)
	binary.LittleEndian.PutUint64(dst[16:24], metaOffset)
	binary.LittleEndian.PutUint32(dst[24:28], metaLength)
	binary.LittleEndian.PutUint32(dst[28:32], 0)
	binary.LittleEndian.PutUint64(dst[32:40], totalEntries)
	binary.LittleEndian.PutUint64(dst[40:48], liveBytes)
	binary.LittleEndian.PutUint64(dst[48:56], arenaBytes)
}

func decodeSuperblockPayload(payload []byte) (arenaBytes, dirOffset uint64, dirCount uint32, metaOffset uint64, metaLength uint32, totalEntries, liveBytes uint64, ok bool) {
	if len(payload) < superblockPayloadBytes {
		return 0, 0, 0, 0, 0, 0, 0, false
	}
	return binary.LittleEndian.Uint64(payload[48:56]),
		binary.LittleEndian.Uint64(payload[0:8]),
		binary.LittleEndian.Uint32(payload[8:12]),
		binary.LittleEndian.Uint64(payload[16:24]),
		binary.LittleEndian.Uint32(payload[24:28]),
		binary.LittleEndian.Uint64(payload[32:40]),
		binary.LittleEndian.Uint64(payload[40:48]),
		true
}

// selectSuperblock returns the highest-generation valid superblock in the arena.
func selectSuperblock(arena []byte) (slot int, header storage.SuperblockHeader, payload []byte, ok bool) {
	slot = -1
	for candidate := 0; candidate < superblockSlots; candidate++ {
		offset := superblockOffset(candidate)
		if offset+superblockBytes > uint64(len(arena)) {
			continue
		}
		h, p, valid := storage.DecodeSuperblock(arena[offset:offset+superblockBytes], geoidxMagic, geoidxVersion)
		if !valid {
			continue
		}
		if slot == -1 || h.Generation > header.Generation {
			slot, header, payload = candidate, h, p
		}
	}
	return slot, header, payload, slot != -1
}

// buildAllocMetadata builds the durable allocator state for one generation: a
// fresh bitmap with only the second superblock and the live extents marked used.
// Retired and uncommitted extents are free, so a reopen reclaims them.
func buildAllocMetadata(arenaBytes uint64, live []extentRef) ([]byte, error) {
	alloc, err := pagealloc.New(arenaBytes)
	if err != nil {
		return nil, err
	}
	if err := alloc.Reserve(secondSuperblockRef, pagealloc.MinBlockSize); err != nil {
		return nil, err
	}
	for _, e := range live {
		if err := alloc.Reserve(e.ref, e.size); err != nil {
			return nil, err
		}
	}
	return alloc.Encode(), nil
}

// New creates a persistent, empty single-file index in dir.
func New(dir string, config Config) (*Index, error) {
	cfg, err := config.withDefaults()
	if err != nil {
		return nil, err
	}
	lock, err := storage.AcquireLock(dir, lockName, true)
	if err != nil {
		return nil, err
	}
	index, err := newPersistent(dir, lock, cfg)
	if err != nil {
		_ = storage.ReleaseLock(lock)
		return nil, err
	}
	return index, nil
}

func newPersistent(dir string, lock *os.File, cfg Config) (*Index, error) {
	alloc, err := pagealloc.New(uint64(cfg.MaxFileBytes))
	if err != nil {
		return nil, err
	}
	mapped, err := storage.CreateMapped(filepath.Join(dir, geoidxFile), int64(alloc.TotalBytes()))
	if err != nil {
		return nil, err
	}
	if err := alloc.Reserve(secondSuperblockRef, pagealloc.MinBlockSize); err != nil {
		_ = mapped.Close()
		return nil, err
	}
	index := &Index{
		config:     cfg,
		persistent: true,
		dir:        dir,
		lock:       lock,
		arena:      mapped,
		alloc:      alloc,
		arenaBytes: alloc.TotalBytes(),
		gen:        1,
	}
	if err := index.createEmptyLocked(); err != nil {
		_ = mapped.Close()
		return nil, err
	}
	return index, nil
}

// Open opens a persistent index created by New.
func Open(dir string, config Config) (*Index, error) {
	cfg, err := config.withDefaults()
	if err != nil {
		return nil, err
	}
	lock, err := storage.AcquireLock(dir, lockName, false)
	if err != nil {
		return nil, err
	}
	if _, statErr := os.Stat(filepath.Join(dir, geoidxFile)); statErr != nil {
		_ = storage.ReleaseLock(lock)
		// A pre-shard multi-file directory (manifest + shard files) is a legacy
		// format; report it explicitly rather than reinterpreting it.
		if _, legacyErr := os.Stat(filepath.Join(dir, "manifest")); legacyErr == nil {
			return nil, ErrLegacyFormat
		}
		return nil, ErrCorrupt
	}
	mapped, err := storage.OpenMapped(filepath.Join(dir, geoidxFile))
	if err != nil {
		_ = storage.ReleaseLock(lock)
		return nil, err
	}
	index := &Index{config: cfg, persistent: true, dir: dir, lock: lock, arena: mapped}
	if err := index.loadLocked(); err != nil {
		_ = mapped.Close()
		_ = storage.ReleaseLock(lock)
		return nil, err
	}
	return index, nil
}

func (i *Index) createEmptyLocked() error {
	arena := i.arena.Bytes()
	metaSize, err := pagealloc.EncodedSize(i.arenaBytes)
	if err != nil {
		return err
	}
	metaRef, metaCapacity, err := i.alloc.Alloc(uint32(metaSize))
	if err != nil {
		return err
	}
	metaOffset := uint64(metaRef) * uint64(pagealloc.MinBlockSize)
	blob, err := buildAllocMetadata(i.arenaBytes, []extentRef{{ref: metaRef, size: metaCapacity}})
	if err != nil {
		return err
	}
	copy(arena[metaOffset:metaOffset+uint64(len(blob))], blob)
	if err := i.arena.Sync(); err != nil {
		return err
	}
	i.metaOffset, i.metaLength = metaOffset, uint32(len(blob))
	i.dirOffset, i.dirLength = 0, 0
	if err := i.commitSuperblock(0, i.gen, 0, 0, i.metaOffset, i.metaLength, 0, 0); err != nil {
		return err
	}
	i.slot = 0
	i.publishLocked()
	return nil
}

// loadLocked validates the committed generation and publishes the recovered view.
func (i *Index) loadLocked() error {
	arena := i.arena.Bytes()
	if uint64(len(arena)) < 2*superblockBytes {
		return ErrCorrupt
	}
	slot, header, payload, ok := selectSuperblock(arena)
	if !ok {
		return ErrCorrupt
	}
	arenaBytes, dirOffset, dirCount, metaOffset, metaLength, totalEntryCount, _, ok := decodeSuperblockPayload(payload)
	if !ok || arenaBytes != uint64(len(arena)) {
		return ErrCorrupt
	}
	if metaOffset+uint64(metaLength) > uint64(len(arena)) {
		return ErrCorrupt
	}
	alloc, err := pagealloc.Decode(arena[metaOffset : metaOffset+uint64(metaLength)])
	if err != nil {
		return ErrCorrupt
	}
	if used, err := alloc.Used(secondSuperblockRef, pagealloc.MinBlockSize); err != nil || !used {
		return ErrCorrupt
	}
	shards, err := decodeDirectory(arena, dirOffset, dirCount, alloc)
	if err != nil {
		return ErrCorrupt
	}
	if !validateDirectory(shards) {
		return ErrCorrupt
	}
	if uint64(totalEntryCount) != uint64(totalEntries(shards)) {
		return ErrCorrupt
	}
	i.arenaBytes = arenaBytes
	i.alloc = alloc
	i.shards = shards
	i.dirOffset, i.dirLength = dirOffset, uint32(dirCount)*dirEntryBytes
	i.metaOffset, i.metaLength = metaOffset, metaLength
	i.gen = header.Generation
	i.slot = slot
	i.publishLocked()
	return nil
}

func (i *Index) commitSuperblock(slot int, generation uint64, dirOffset uint64, dirCount uint32, metaOffset uint64, metaLength uint32, totalEntries, liveBytes uint64) error {
	var payload [superblockPayloadBytes]byte
	encodeSuperblockPayload(payload[:], i.arenaBytes, dirOffset, dirCount, metaOffset, metaLength, totalEntries, liveBytes)
	var slotBytes [storage.SuperblockSlotBytes]byte
	if err := storage.EncodeSuperblock(slotBytes[:], storage.SuperblockHeader{
		Magic:      geoidxMagic,
		Version:    geoidxVersion,
		Generation: generation,
	}, payload[:]); err != nil {
		return err
	}
	if err := commitSuperblockFunc(i.arena.Bytes(), superblockOffset(slot), slotBytes[:]); err != nil {
		return err
	}
	return i.arena.Sync()
}

// writeSuperblock persists the current committed state into a slot. It is used
// only by tests and recovery, not by ordinary compaction.
func (i *Index) writeSuperblock(slot int) error {
	return i.commitSuperblock(slot, i.gen, i.dirOffset, uint32(len(i.shards)), i.metaOffset, i.metaLength, uint64(totalEntries(i.shards)), liveBytes(i.shards))
}

// liveBytes sums the shard payload bytes.
func liveBytes(shards []*shard) uint64 {
	var total uint64
	for _, s := range shards {
		total += uint64(s.count()) * shardBytesPerRecord
	}
	return total
}

// shardPlan is one output slot of a compaction: either an existing shard kept
// unchanged, or new heap content to write.
type shardPlan struct {
	keep *shard
	keys []S2Key
	ids  []common.ImageID
}

func planBytes(p shardPlan) int64 {
	if p.keep != nil {
		return p.keep.bytes()
	}
	return int64(len(p.keys)) * shardBytesPerRecord
}

func planArrays(p shardPlan) ([]S2Key, []common.ImageID) {
	if p.keep != nil {
		return p.keep.keys, p.keep.ids
	}
	return p.keys, p.ids
}

// prepareShards selects the affected shards for a frozen delta and rebuilds only
// those. Unaffected shards are kept by reference. A rebuilt shard larger than
// MaxShardBytes is split, and adjacent underutilized plans are coalesced.
func (i *Index) prepareShards(current []*shard, frozen []mutation) ([]shardPlan, error) {
	adds := sortedByKey(frozen)
	shadow := newMutationSet(frozen, totalEntries(current))
	if len(current) == 0 {
		return shardsFromAdds(adds, i.config), nil
	}
	addSlices, dirty := partitionAdds(current, adds)

	// A shard is affected by a delete or move only when one of its base IDs is
	// named by the delta. The delta's [min,max] ImageID prunes shards whose own
	// [minID,maxID] does not overlap, which is the common case for monotonically
	// increasing or geographically clustered IDs.
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
		ids := make([]common.ImageID, count)
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
// delta yields a range that never prunes (min=MaxUint64, max=0).
func deltaIDRange(frozen []mutation) (common.ImageID, common.ImageID) {
	if len(frozen) == 0 {
		return common.ImageID(^uint64(0)), 0
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
	ids := make([]common.ImageID, len(adds))
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
// target and at least one is below the minimum, so normal-sized shards are left
// alone and split/merge cannot cycle.
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
	ids := make([]common.ImageID, 0, len(ai)+len(bi))
	ids = append(ids, ai...)
	ids = append(ids, bi...)
	return shardPlan{keys: keys, ids: ids}
}

// materialize turns plans into shards. For a persistent index it writes new
// content into freshly allocated in-file extents and records them for rollback.
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
			return nil, nil, fmt.Errorf("%w: %v", ErrOverflow, err)
		}
		offset := uint64(ref) * uint64(pagealloc.MinBlockSize)
		arena := i.arena.Bytes()
		keys, err := storage.View[S2Key](arena, offset, uint64(len(p.keys)), ErrCorrupt, ErrCorrupt)
		if err != nil {
			i.rollback(append(allocated, extentRef{ref, capacity}))
			return nil, nil, err
		}
		ids, err := storage.View[common.ImageID](arena, offset+uint64(len(p.keys))*8, uint64(len(p.keys)), ErrCorrupt, ErrCorrupt)
		if err != nil {
			i.rollback(append(allocated, extentRef{ref, capacity}))
			return nil, nil, err
		}
		copy(keys, p.keys)
		copy(ids, p.ids)
		s := &shard{
			keys: keys, ids: ids, offset: offset, length: capacity, gen: gen,
			minID: minOfIDs(ids), maxID: maxOfIDs(ids),
		}
		s.keyCRC, s.idCRC = checksumKeys(s), checksumIDs(s)
		shards = append(shards, s)
		allocated = append(allocated, extentRef{ref, capacity})
	}
	return shards, allocated, nil
}

// rollback frees extents allocated for an aborted compaction.
func (i *Index) rollback(allocated []extentRef) {
	for _, e := range allocated {
		_ = i.alloc.Free(e.ref, e.size)
	}
}

// writeDirectory writes a new directory extent for shards and returns its offset
// and payload length.
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
	copy(i.arena.Bytes()[offset:offset+uint64(len(buf))], buf)
	return offset, uint32(len(buf)), nil
}

// writeMetadata builds the durable allocator state for the new generation and
// writes it into a meta extent that includes itself in the live set.
func (i *Index) writeMetadata(shards []*shard, dirOffset uint64, dirLength uint32, allocated *[]extentRef) (uint64, uint32, error) {
	metaSize, err := pagealloc.EncodedSize(i.arenaBytes)
	if err != nil {
		return 0, 0, err
	}
	metaRef, metaCapacity, err := i.alloc.Alloc(uint32(metaSize))
	if err != nil {
		return 0, 0, err
	}
	live := make([]extentRef, 0, len(shards)+3)
	live = append(live, extentRef{ref: metaRef, size: metaCapacity})
	if dirLength > 0 {
		live = append(live, extentRef{ref: pagealloc.Ref(dirOffset / uint64(pagealloc.MinBlockSize)), size: dirLength})
	}
	for _, s := range shards {
		if s.offset != 0 {
			live = append(live, shardExtent(s))
		}
	}
	blob, err := buildAllocMetadata(i.arenaBytes, live)
	if err != nil {
		i.rollback(append(*allocated, extentRef{metaRef, metaCapacity}))
		return 0, 0, err
	}
	offset := uint64(metaRef) * uint64(pagealloc.MinBlockSize)
	copy(i.arena.Bytes()[offset:offset+uint64(len(blob))], blob)
	*allocated = append(*allocated, extentRef{metaRef, metaCapacity})
	return offset, uint32(len(blob)), nil
}

// runCompaction advances one compaction through its phases.
func (i *Index) runCompaction() {
	i.mu.Lock()
	snapshot := i.shards
	frozen := i.frozen
	i.mu.Unlock()

	if err := failStage(persistCreate); err != nil {
		i.finishCompaction(err)
		return
	}
	plans, err := i.prepareShards(snapshot, frozen)
	if err != nil {
		i.finishCompaction(err)
		return
	}

	newGen := i.gen + 1
	newShards, allocated, err := i.materialize(plans, newGen)
	if err != nil {
		i.finishCompaction(err)
		return
	}
	if !i.persistent {
		i.publishCompaction(newShards, newGen, 0, 0, 0, 0)
		i.finishReclaim(nil, extentRef{}, extentRef{})
		return
	}

	if err := failStage(persistWrite); err != nil {
		i.rollback(allocated)
		i.finishCompaction(err)
		return
	}
	dirOffset, dirLength, err := i.writeDirectory(newShards, &allocated)
	if err != nil {
		i.rollback(allocated)
		i.finishCompaction(err)
		return
	}
	metaOffset, metaLength, err := i.writeMetadata(newShards, dirOffset, dirLength, &allocated)
	if err != nil {
		i.rollback(allocated)
		i.finishCompaction(err)
		return
	}
	if err := failStage(persistSync); err != nil {
		i.rollback(allocated)
		i.finishCompaction(err)
		return
	}
	if err := i.arena.Sync(); err != nil {
		i.rollback(allocated)
		i.finishCompaction(err)
		return
	}

	oldDir := extentRef{}
	if i.dirLength > 0 {
		oldDir = extentRef{ref: pagealloc.Ref(i.dirOffset / uint64(pagealloc.MinBlockSize)), size: i.dirLength}
	}
	oldMeta := extentRef{ref: pagealloc.Ref(i.metaOffset / uint64(pagealloc.MinBlockSize)), size: i.metaLength}

	i.mu.Lock()
	if cerr := failStage(persistCommit); cerr != nil {
		i.mu.Unlock()
		i.rollback(allocated)
		i.finishCompaction(cerr)
		return
	}
	if err := i.commitSuperblock(i.slot^1, newGen, dirOffset, uint32(len(newShards)), metaOffset, metaLength, uint64(totalEntries(newShards)), liveBytes(newShards)); err != nil {
		// A post-write sync error can still have committed the slot. Reconcile
		// with what is actually durable before treating it as uncommitted.
		gen, _, _, ok := i.committedGeneration()
		if !ok || gen != newGen {
			i.mu.Unlock()
			i.rollback(allocated)
			i.finishCompaction(err)
			return
		}
		_ = i.arena.Sync()
	}
	_ = failStage(persistPublish)
	old := i.shards
	i.shards = newShards
	i.frozen = nil
	i.compactionErr = nil
	i.gen = newGen
	i.slot ^= 1
	i.dirOffset, i.dirLength = dirOffset, dirLength
	i.metaOffset, i.metaLength = metaOffset, metaLength
	i.deltaMax = maxImageID(i.maxShardIDLocked(), nil, i.layers, i.active)
	i.publishLocked()
	i.mu.Unlock()

	i.finishReclaim(old, oldDir, oldMeta)
}

// publishCompaction installs a new in-memory (heap) directory.
func (i *Index) publishCompaction(newShards []*shard, newGen uint64, _ uint64, _ uint32, _ uint64, _ uint32) {
	i.mu.Lock()
	i.shards = newShards
	i.frozen = nil
	i.compactionErr = nil
	i.gen = newGen
	i.deltaMax = maxImageID(i.maxShardIDLocked(), nil, i.layers, i.active)
	i.publishLocked()
	i.mu.Unlock()
}

// finishReclaim retires replaced extents and reclaims them after readers drain.
func (i *Index) finishReclaim(old []*shard, oldDir, oldMeta extentRef) {
	if i.persistent {
		current := map[*shard]struct{}{}
		for _, s := range i.shards {
			current[s] = struct{}{}
		}
		for _, s := range old {
			if _, ok := current[s]; ok || s.offset == 0 {
				continue
			}
			_ = i.alloc.Retire(shardExtent(s).ref, shardExtent(s).size)
		}
		if oldDir.size > 0 {
			_ = i.alloc.Retire(oldDir.ref, oldDir.size)
		}
		if oldMeta.size > 0 {
			_ = i.alloc.Retire(oldMeta.ref, oldMeta.size)
		}
	}
	_ = failStage(persistReclaim)
	i.tracker.WaitForPriorReaders()
	if i.persistent {
		_ = i.alloc.Reclaim()
	}
	_ = failStage(persistRemove)
	i.mu.Lock()
	i.compacting = false
	close(i.compactionDone)
	i.mu.Unlock()
}

// committedGeneration returns the highest committed generation in the arena.
func (i *Index) committedGeneration() (uint64, int, []byte, bool) {
	slot, header, payload, ok := selectSuperblock(i.arena.Bytes())
	return header.Generation, slot, payload, ok
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
		err = i.arena.Close()
		i.arena = nil
	}
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
		ArenaBytes:   i.alloc.TotalBytes(),
		LiveBytes:    live,
		FreeBytes:    i.alloc.FreeBytes(),
		LargestFree:  i.alloc.LargestFreeBytes(),
		FreeRuns:     i.alloc.FreeRunCount(),
		RetiredBytes: i.alloc.RetiredBytes(),
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
func maxOfIDs(ids []common.ImageID) common.ImageID {
	var best common.ImageID
	for _, id := range ids {
		if id > best {
			best = id
		}
	}
	return best
}

// minOfIDs returns the smallest ImageID in ids, or 0 when empty.
func minOfIDs(ids []common.ImageID) common.ImageID {
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

// validateBase reports whether a shard's records are well-formed (strict
// (key, id) order, valid S2 level-19 keys, no reserved zero ID) and its recorded
// maximum ImageID matches. It returns ErrCorrupt on any violation.
func validateBase(keys []S2Key, ids []common.ImageID, maxID common.ImageID) error {
	if !validateShardOrder(keys, ids) {
		return ErrCorrupt
	}
	if maxOfIDs(ids) != maxID {
		return ErrCorrupt
	}
	return nil
}

// exactCount returns the number of live (key, imageID) pairs after applying the
// delta. It mirrors mergeInto's traversal exactly (one pass over the two
// (key, id)-sorted sequences), so a run of identical S2Keys is walked a single
// time instead of being rescanned per insertion, and it allocates no index-sized
// temporary.
func exactCount(baseKeys []S2Key, baseIDs []common.ImageID, shadow mutationSet, adds []mutation) uint64 {
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
// (key, id) order, skipping base entries shadowed by the delta and
// de-duplicating equal pairs. It mirrors exactCount's traversal exactly.
func mergeInto(keys []S2Key, ids []common.ImageID, baseKeys []S2Key, baseIDs []common.ImageID, shadow mutationSet, adds []mutation) int {
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
