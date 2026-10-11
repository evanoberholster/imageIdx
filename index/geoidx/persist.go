package geoidx

import (
	"cmp"
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"

	common "github.com/evanoberholster/imageIdx/common"
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

// writeManifestFunc is a test seam for the manifest commit; production uses
// writeManifest directly.
var writeManifestFunc = writeManifest

func failStage(stage persistStage) error {
	if persistFail == nil {
		return nil
	}
	return persistFail(stage)
}

const (
	shardMagic   uint32 = 0x47454f53 // "GEOS"
	shardVersion uint16 = 1

	manifestMagic   uint32 = 0x47454f4d // "GEOM"
	manifestVersion uint16 = 2

	manifestHeaderBytes = 18 // magic(4)+version(2)+dirGen(8)+count(4)
	manifestEntryBytes  = 32 // gen(8)+count(8)+firstKey(8)+firstID(8)

	basePayloadBytes = 24 // count(8) + maxID(8) + key CRC(4) + id CRC(4)

	lockName     = "geoidx.lock"
	manifestTmp  = "manifest.tmp"
	manifestName = "manifest"
	shardPattern = "shard.*.dat"
)

const baseHeaderBytes = uint64(2 * storage.SuperblockSlotBytes)

// shardPath is the file name of one shard generation.
func shardPath(dir string, generation uint64) string {
	return filepath.Join(dir, fmt.Sprintf("shard.%016x.dat", generation))
}

// shardIDsOffset returns the byte offset of the ImageID array for count keys.
// Key and ID arrays are 64-byte aligned.
func shardIDsOffset(count uint64) uint64 {
	return storage.Align(baseHeaderBytes+count*8, 64)
}

func shardFileSize(count uint64) uint64 {
	return shardIDsOffset(count) + count*8
}

// checkedShardFileSize is shardFileSize with overflow checks so untrusted counts
// cannot wrap offsets.
func checkedShardFileSize(count uint64) (uint64, bool) {
	if count > (math.MaxUint64-baseHeaderBytes)/8 {
		return 0, false
	}
	end := baseHeaderBytes + count*8
	ids := storage.Align(end, 64)
	if ids < end {
		return 0, false
	}
	if count > (math.MaxUint64-ids)/8 {
		return 0, false
	}
	return ids + count*8, true
}

func encodeShardMeta(dst []byte, generation, count, maxID uint64, keyCRC, idCRC uint32) error {
	var payload [basePayloadBytes]byte
	binary.LittleEndian.PutUint64(payload[0:8], count)
	binary.LittleEndian.PutUint64(payload[8:16], maxID)
	binary.LittleEndian.PutUint32(payload[16:20], keyCRC)
	binary.LittleEndian.PutUint32(payload[20:24], idCRC)
	return storage.EncodeSuperblock(dst, storage.SuperblockHeader{
		Magic:      shardMagic,
		Version:    shardVersion,
		Generation: generation,
	}, payload[:])
}

// writeShardMapped writes keys and ids to a new mmap-backed shard file and
// returns the shard viewing it. The file is synced before return.
func writeShardMapped(dir string, generation uint64, keys []S2Key, ids []common.ImageID) (*shard, error) {
	count := uint64(len(keys))
	size, ok := checkedShardFileSize(count)
	if !ok || size > math.MaxInt64 {
		return nil, ErrOverflow
	}
	mf, err := storage.CreateMapped(shardPath(dir, generation), int64(size))
	if err != nil {
		return nil, err
	}
	arena := mf.Bytes()
	keyView, err := storage.View[S2Key](arena, baseHeaderBytes, count, ErrCorrupt, ErrCorrupt)
	if err != nil {
		_ = mf.Close()
		return nil, err
	}
	idsOffset := shardIDsOffset(count)
	idView, err := storage.View[common.ImageID](arena, idsOffset, count, ErrCorrupt, ErrCorrupt)
	if err != nil {
		_ = mf.Close()
		return nil, err
	}
	copy(keyView, keys)
	copy(idView, ids)
	if err := failStage(persistWrite); err != nil {
		_ = mf.Close()
		return nil, err
	}
	maxID := maxOfIDs(ids)
	keyBytes := arena[baseHeaderBytes : baseHeaderBytes+count*8]
	idBytes := arena[idsOffset : idsOffset+count*8]
	var slot [storage.SuperblockSlotBytes]byte
	if err := encodeShardMeta(slot[:], generation, count, uint64(maxID), storage.Checksum(keyBytes), storage.Checksum(idBytes)); err != nil {
		_ = mf.Close()
		return nil, err
	}
	copy(arena[0:storage.SuperblockSlotBytes], slot[:])
	if err := failStage(persistSync); err != nil {
		_ = mf.Close()
		return nil, err
	}
	if err := mf.Sync(); err != nil {
		_ = mf.Close()
		return nil, err
	}
	return &shard{keys: keyView, ids: idView, mf: mf, gen: generation, maxID: maxID}, nil
}

// openShard opens and validates one committed shard file.
func openShard(dir string, generation, expectedCount, expectedFirstKey, expectedFirstID uint64) (*shard, error) {
	mf, err := storage.OpenMapped(shardPath(dir, generation))
	if err != nil {
		return nil, err
	}
	fail := func() (*shard, error) {
		_ = mf.Close()
		return nil, ErrCorrupt
	}
	size, ok := checkedShardFileSize(expectedCount)
	if !ok {
		return fail()
	}
	arena := mf.Bytes()
	if uint64(len(arena)) < size {
		return fail()
	}
	slots := [storage.SuperblockSlots][]byte{
		arena[0:storage.SuperblockSlotBytes],
		arena[storage.SuperblockSlotBytes : 2*storage.SuperblockSlotBytes],
	}
	_, header, payload, ok := storage.SelectSuperblock(slots, shardMagic, shardVersion)
	if !ok || header.Generation != generation || len(payload) < basePayloadBytes {
		return fail()
	}
	count := binary.LittleEndian.Uint64(payload[0:8])
	if count != expectedCount {
		return fail()
	}
	maxID := common.ImageID(binary.LittleEndian.Uint64(payload[8:16]))
	keyCRC := binary.LittleEndian.Uint32(payload[16:20])
	idCRC := binary.LittleEndian.Uint32(payload[20:24])
	keys, err := storage.View[S2Key](arena, baseHeaderBytes, count, ErrCorrupt, ErrCorrupt)
	if err != nil {
		return fail()
	}
	idsOffset := shardIDsOffset(count)
	ids, err := storage.View[common.ImageID](arena, idsOffset, count, ErrCorrupt, ErrCorrupt)
	if err != nil {
		return fail()
	}
	if count > 0 {
		keyBytes := arena[baseHeaderBytes : baseHeaderBytes+count*8]
		idBytes := arena[idsOffset : idsOffset+count*8]
		if storage.Checksum(keyBytes) != keyCRC || storage.Checksum(idBytes) != idCRC {
			return fail()
		}
		if uint64(keys[0]) != expectedFirstKey || uint64(ids[0]) != expectedFirstID {
			return fail()
		}
	}
	if validateBase(keys, ids, maxID) != nil {
		return fail()
	}
	return &shard{keys: keys, ids: ids, mf: mf, gen: generation, maxID: maxID}, nil
}

// manifestEntry is one shard boundary recorded in the manifest.
type manifestEntry struct {
	gen      uint64
	count    uint64
	firstKey uint64
	firstID  uint64
}

// manifestBytesEncode returns the serialized v2 manifest for the directory.
func encodeManifest(dirGen uint64, shards []*shard) []byte {
	buf := make([]byte, manifestHeaderBytes+manifestEntryBytes*len(shards)+4)
	binary.LittleEndian.PutUint32(buf[0:4], manifestMagic)
	binary.LittleEndian.PutUint16(buf[4:6], manifestVersion)
	binary.LittleEndian.PutUint64(buf[6:14], dirGen)
	binary.LittleEndian.PutUint32(buf[14:18], uint32(len(shards)))
	off := manifestHeaderBytes
	for _, s := range shards {
		fk, fi := s.firstPair()
		binary.LittleEndian.PutUint64(buf[off:off+8], s.gen)
		binary.LittleEndian.PutUint64(buf[off+8:off+16], uint64(s.count()))
		binary.LittleEndian.PutUint64(buf[off+16:off+24], uint64(fk))
		binary.LittleEndian.PutUint64(buf[off+24:off+32], uint64(fi))
		off += manifestEntryBytes
	}
	binary.LittleEndian.PutUint32(buf[off:off+4], storage.Checksum(buf[:off]))
	return buf
}

// decodeManifest parses a manifest. legacy is true when it is the pre-shard v1
// format, which is rejected rather than reinterpreted.
func decodeManifest(data []byte) (dirGen uint64, entries []manifestEntry, legacy bool, ok bool) {
	if len(data) < 6 || binary.LittleEndian.Uint32(data[0:4]) != manifestMagic {
		return 0, nil, false, false
	}
	switch binary.LittleEndian.Uint16(data[4:6]) {
	case 1:
		return 0, nil, true, false
	case manifestVersion:
	default:
		return 0, nil, false, false
	}
	if len(data) < manifestHeaderBytes+4 {
		return 0, nil, false, false
	}
	n := int(binary.LittleEndian.Uint32(data[14:18]))
	if n < 0 || len(data) != manifestHeaderBytes+manifestEntryBytes*n+4 {
		return 0, nil, false, false
	}
	if storage.Checksum(data[:len(data)-4]) != binary.LittleEndian.Uint32(data[len(data)-4:]) {
		return 0, nil, false, false
	}
	dirGen = binary.LittleEndian.Uint64(data[6:14])
	entries = make([]manifestEntry, n)
	off := manifestHeaderBytes
	for i := 0; i < n; i++ {
		entries[i] = manifestEntry{
			gen:      binary.LittleEndian.Uint64(data[off : off+8]),
			count:    binary.LittleEndian.Uint64(data[off+8 : off+16]),
			firstKey: binary.LittleEndian.Uint64(data[off+16 : off+24]),
			firstID:  binary.LittleEndian.Uint64(data[off+24 : off+32]),
		}
		off += manifestEntryBytes
	}
	return dirGen, entries, false, true
}

func writeManifest(dir string, data []byte) error {
	return storage.WriteFileAtomic(dir, manifestTmp, manifestName, data, 0o644)
}

// readManifest returns the committed directory generation and shard entries, or
// legacy=true when the directory holds a pre-shard index.
func readManifest(dir string) (dirGen uint64, entries []manifestEntry, legacy, ok bool) {
	data, err := os.ReadFile(filepath.Join(dir, manifestName))
	if err != nil {
		return 0, nil, false, false
	}
	return decodeManifest(data)
}

// New creates a persistent, empty index in dir.
func New(dir string, config Config) (*Index, error) {
	cfg, err := config.withDefaults()
	if err != nil {
		return nil, err
	}
	lock, err := storage.AcquireLock(dir, lockName, true)
	if err != nil {
		return nil, err
	}
	index := &Index{config: cfg, persistent: true, dir: dir, lock: lock, dirGen: 1, nextGen: 2}
	if err := index.createEmptyLocked(); err != nil {
		_ = storage.ReleaseLock(lock)
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
	index := &Index{config: cfg, persistent: true, dir: dir, lock: lock}
	if err := index.loadLocked(); err != nil {
		_ = storage.ReleaseLock(lock)
		return nil, err
	}
	return index, nil
}

func (i *Index) createEmptyLocked() error {
	if err := writeManifest(i.dir, encodeManifest(i.dirGen, nil)); err != nil {
		return err
	}
	i.publishLocked()
	return nil
}

// loadLocked reads, validates, and publishes the committed shard directory.
func (i *Index) loadLocked() error {
	dirGen, entries, legacy, ok := readManifest(i.dir)
	if legacy {
		return ErrLegacyFormat
	}
	if !ok {
		return ErrCorrupt
	}
	shards := make([]*shard, 0, len(entries))
	for _, e := range entries {
		s, err := openShard(i.dir, e.gen, e.count, e.firstKey, e.firstID)
		if err != nil {
			closeShards(shards)
			return err
		}
		shards = append(shards, s)
	}
	if !validateDirectory(shards) {
		closeShards(shards)
		return ErrCorrupt
	}
	i.shards = shards
	i.dirGen = dirGen
	i.nextGen = dirGen + 1
	i.publishLocked()
	cleanupOrphanShards(i.dir, shards)
	return nil
}

// closeShards unmaps every shard that owns a mapping.
func closeShards(shards []*shard) {
	for _, s := range shards {
		if s.mf != nil {
			_ = s.mf.Close()
		}
	}
}

// referencedShardGens returns the set of committed shard generations.
func referencedShardGens(shards []*shard) map[uint64]struct{} {
	set := make(map[uint64]struct{}, len(shards))
	for _, s := range shards {
		if s.gen != 0 {
			set[s.gen] = struct{}{}
		}
	}
	return set
}

// cleanupOrphanShards removes shard files not referenced by the committed
// directory. It is best-effort and runs before any reader is using the index.
func cleanupOrphanShards(dir string, shards []*shard) {
	keep := referencedShardGens(shards)
	paths, err := filepath.Glob(filepath.Join(dir, shardPattern))
	if err != nil {
		return
	}
	for _, path := range paths {
		var gen uint64
		if _, err := fmt.Sscanf(filepath.Base(path), "shard.%x.dat", &gen); err != nil {
			continue
		}
		if _, ok := keep[gen]; ok {
			continue
		}
		_ = os.Remove(path)
	}
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
// MaxShardBytes is split, and adjacent underutilized plans are coalesced with
// hysteresis so split/merge cannot thrash.
func (i *Index) prepareShards(current []*shard, frozen []mutation) ([]shardPlan, error) {
	adds := sortedByKey(frozen)
	shadow := newMutationSet(frozen, totalEntries(current))
	if len(current) == 0 {
		// No committed shards yet (bulk load): the adds form the directory.
		return shardsFromAdds(adds, i.config), nil
	}
	addSlices, dirty := partitionAdds(current, adds)

	// An ImageID-authoritative delete can name a base entry in any shard, so a
	// shard is also dirty when any of its base ids is shadowed by the delta.
	for j, s := range current {
		if dirty[j] {
			continue
		}
		for _, id := range s.ids {
			if shadow.touched(id) {
				dirty[j] = true
				break
			}
		}
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
			continue // shard fully removed
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

// concatPlans builds a single new-content plan from two adjacent plans. The
// inputs are disjoint and ordered, so concatenation stays globally sorted.
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

// materialize turns plans into shards, writing new files for new content.
func (i *Index) materialize(plans []shardPlan) ([]*shard, []*shard, error) {
	shards := make([]*shard, 0, len(plans))
	written := make([]*shard, 0, len(plans))
	for _, p := range plans {
		if p.keep != nil {
			shards = append(shards, p.keep)
			continue
		}
		if len(p.keys) == 0 {
			continue
		}
		if i.persistent {
			generation := i.nextGen
			i.nextGen++
			s, err := writeShardMapped(i.dir, generation, p.keys, p.ids)
			if err != nil {
				closeShards(written)
				return nil, written, err
			}
			shards = append(shards, s)
			written = append(written, s)
			continue
		}
		shards = append(shards, &shard{keys: p.keys, ids: p.ids, maxID: maxOfIDs(p.ids)})
	}
	return shards, written, nil
}

// discardShards removes newly written shard files and unmaps them.
func discardShards(dir string, shards []*shard, persistent bool) {
	for _, s := range shards {
		if s.mf != nil {
			_ = s.mf.Close()
		}
		if persistent && s.gen != 0 {
			_ = os.Remove(shardPath(dir, s.gen))
		}
	}
}

// runCompaction advances one compaction through its phases:
//
//	prepare  (off-lock) rebuild affected shards
//	commit   (locked)   publish the manifest (the durable linearization point)
//	publish  (locked)   swap in the new in-memory directory
//	reclaim  (off-lock) drain readers, unmap retired shards, clean up
//
// The writer mutex is held only for the short commit/publish step; shard
// construction and reclamation run outside it. A failure before the manifest
// commit leaves the previous durable directory authoritative and retains the
// frozen snapshot. Once the manifest commits the new directory is authoritative
// and is always published, so a later failure cannot leave the index operating
// as if it were never committed.
func (i *Index) runCompaction() {
	i.mu.Lock()
	snapshot := append([]*shard(nil), i.shards...)
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
	newShards, written, err := i.materialize(plans)
	if err != nil {
		i.finishCompaction(err)
		return
	}

	i.mu.Lock()
	dirGen := i.nextGen
	i.nextGen++
	if i.persistent {
		if cerr := failStage(persistCommit); cerr != nil {
			i.mu.Unlock()
			discardShards(i.dir, written, true)
			i.finishCompaction(cerr)
			return
		}
		data := encodeManifest(dirGen, newShards)
		if cerr := writeManifestFunc(i.dir, data); cerr != nil {
			// The manifest write is atomic, but an error after the rename (for
			// example a directory sync failure) can still have committed the new
			// directory. Reconcile with what is durable before treating it as
			// uncommitted.
			if gen, _, _, ok := readManifest(i.dir); !ok || gen != dirGen {
				i.mu.Unlock()
				discardShards(i.dir, written, true)
				i.finishCompaction(cerr)
				return
			}
		}
	}
	_ = failStage(persistPublish)
	old := i.shards
	i.shards = newShards
	i.frozen = nil
	i.compactionErr = nil
	i.dirGen = dirGen
	i.deltaMax = maxImageID(i.maxShardIDLocked(), nil, i.layers, i.active)
	i.publishLocked()
	i.mu.Unlock()

	// Reclaim without the writer mutex. The reader tracker drains every reader
	// that pinned the old directory; only then are retired shards unmapped.
	_ = failStage(persistReclaim)
	i.tracker.WaitForPriorReaders()
	referenced := map[*shard]struct{}{}
	for _, s := range newShards {
		referenced[s] = struct{}{}
	}
	for _, s := range old {
		if _, ok := referenced[s]; ok {
			continue
		}
		if s.mf != nil {
			_ = s.mf.Close()
		}
		if i.persistent && s.gen != 0 {
			_ = os.Remove(shardPath(i.dir, s.gen))
		}
	}
	_ = failStage(persistRemove)

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
	for _, s := range i.shards {
		if s.mf != nil {
			if closeErr := s.mf.Close(); err == nil {
				err = closeErr
			}
		}
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

// sortedByKey returns a copy of the insert mutations sorted by (key, imageID).
// Deletes are excluded: they only shadow base and older entries. The input layer
// is id-sorted and published to readers, so it must not be reordered in place.
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
// temporary. Keeping the two traversals structurally identical is what makes the
// count and the merge output agree.
func exactCount(baseKeys []S2Key, baseIDs []common.ImageID, shadow mutationSet, adds []mutation) uint64 {
	count := uint64(0)
	baseIndex, addIndex := 0, 0
	for baseIndex < len(baseKeys) || addIndex < len(adds) {
		for baseIndex < len(baseKeys) && shadow.touched(baseIDs[baseIndex]) {
			baseIndex++
		}
		switch {
		case baseIndex == len(baseKeys) && addIndex == len(adds):
			// Both exhausted.
		case addIndex == len(adds):
			count++ // remaining live base entry
			baseIndex++
		case baseIndex == len(baseKeys):
			count++ // remaining insertion
			addIndex++
		default:
			baseKey, baseID := baseKeys[baseIndex], baseIDs[baseIndex]
			addKey, addID := adds[addIndex].key, adds[addIndex].id
			switch {
			case baseKey < addKey || (baseKey == addKey && baseID < addID):
				count++ // live base entry with no matching insertion
				baseIndex++
			case addKey < baseKey || (addKey == baseKey && addID < baseID):
				count++ // insertion not already present as a live base pair
				addIndex++
			default:
				count++ // insertion repeats a live base pair: counted once
				baseIndex++
				addIndex++
			}
		}
	}
	return count
}

// mergeInto writes the retained base entries and additions into keys and ids in
// (key, id) order, skipping base entries shadowed by the delta and
// de-duplicating equal pairs. It mirrors exactCount's traversal exactly, so the
// number written always equals the count for the same inputs. shadow is the
// membership set over the id-sorted delta.
func mergeInto(keys []S2Key, ids []common.ImageID, baseKeys []S2Key, baseIDs []common.ImageID, shadow mutationSet, adds []mutation) int {
	baseIndex, addIndex, out := 0, 0, 0
	for baseIndex < len(baseKeys) || addIndex < len(adds) {
		for baseIndex < len(baseKeys) && shadow.touched(baseIDs[baseIndex]) {
			baseIndex++
		}
		switch {
		case baseIndex == len(baseKeys) && addIndex == len(adds):
			// Both exhausted.
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
