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
	persistManifest
	persistPublish
	persistReclaim
	persistRemove
)

// persistFail is a test seam; production leaves it nil.
var persistFail func(persistStage) error

func failStage(stage persistStage) error {
	if persistFail == nil {
		return nil
	}
	return persistFail(stage)
}

const (
	baseMagic   uint32 = 0x47454f31 // "GEO1"
	baseVersion uint16 = 2

	manifestMagic   uint32 = 0x47454f4d // "GEOM"
	manifestVersion uint16 = 1

	basePayloadBytes = 24 // count(8) + maxID(8) + key CRC(4) + id CRC(4)
	manifestBytes    = 26 // magic(4)+version(2)+gen(8)+count(8)+crc(4)
	lockName         = "geoidx.lock"
)

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
	index := &Index{config: cfg, persistent: true, dir: dir, lock: lock}
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

func basePath(dir string, generation uint64) string {
	return filepath.Join(dir, fmt.Sprintf("base.%016x.dat", generation))
}

// baseIDsOffset returns the byte offset of the ImageID array for count keys.
// Key and ID arrays are 64-byte aligned.
func baseIDsOffset(count uint64) uint64 {
	return storage.Align(baseHeaderBytes+count*8, 64)
}

func baseFileSize(count uint64) uint64 {
	return baseIDsOffset(count) + count*8
}

// checkedBaseFileSize is baseFileSize with overflow checks so untrusted counts
// cannot wrap offsets.
func checkedBaseFileSize(count uint64) (uint64, bool) {
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

const baseHeaderBytes = uint64(2 * storage.SuperblockSlotBytes)

func encodeBaseMeta(dst []byte, generation, count, maxID uint64, keyCRC, idCRC uint32) error {
	var payload [basePayloadBytes]byte
	binary.LittleEndian.PutUint64(payload[0:8], count)
	binary.LittleEndian.PutUint64(payload[8:16], maxID)
	binary.LittleEndian.PutUint32(payload[16:20], keyCRC)
	binary.LittleEndian.PutUint32(payload[20:24], idCRC)
	return storage.EncodeSuperblock(dst, storage.SuperblockHeader{
		Magic:      baseMagic,
		Version:    baseVersion,
		Generation: generation,
	}, payload[:])
}

func encodeManifest(generation, count uint64) []byte {
	buf := make([]byte, manifestBytes)
	binary.LittleEndian.PutUint32(buf[0:4], manifestMagic)
	binary.LittleEndian.PutUint16(buf[4:6], manifestVersion)
	binary.LittleEndian.PutUint64(buf[6:14], generation)
	binary.LittleEndian.PutUint64(buf[14:22], count)
	binary.LittleEndian.PutUint32(buf[22:26], storage.Checksum(buf[:22]))
	return buf
}

func decodeManifest(data []byte) (generation, count uint64, ok bool) {
	if len(data) != manifestBytes {
		return 0, 0, false
	}
	if binary.LittleEndian.Uint32(data[0:4]) != manifestMagic ||
		binary.LittleEndian.Uint16(data[4:6]) != manifestVersion {
		return 0, 0, false
	}
	if storage.Checksum(data[:22]) != binary.LittleEndian.Uint32(data[22:26]) {
		return 0, 0, false
	}
	return binary.LittleEndian.Uint64(data[6:14]), binary.LittleEndian.Uint64(data[14:22]), true
}

func writeManifest(dir string, generation, count uint64) error {
	return storage.WriteFileAtomic(dir, "manifest.tmp", "manifest", encodeManifest(generation, count), 0o644)
}

func (i *Index) createEmptyLocked() error {
	const generation = uint64(1)
	path := basePath(i.dir, generation)
	mf, err := storage.CreateMapped(path, int64(baseFileSize(0)))
	if err != nil {
		return err
	}
	var slot [storage.SuperblockSlotBytes]byte
	if err := encodeBaseMeta(slot[:], generation, 0, 0, 0, 0); err != nil {
		_ = mf.Close()
		return err
	}
	copy(mf.Bytes()[0:storage.SuperblockSlotBytes], slot[:])
	if err := mf.Sync(); err != nil {
		_ = mf.Close()
		return err
	}
	if err := writeManifest(i.dir, generation, 0); err != nil {
		_ = mf.Close()
		return err
	}
	i.base = base{mf: mf, gen: generation}
	i.publishLocked()
	return nil
}

func (i *Index) loadLocked() error {
	data, err := os.ReadFile(filepath.Join(i.dir, "manifest"))
	if err != nil {
		return err
	}
	generation, count, ok := decodeManifest(data)
	if !ok {
		return ErrCorrupt
	}
	size, ok := checkedBaseFileSize(count)
	if !ok {
		return ErrCorrupt
	}
	mf, err := storage.OpenMapped(basePath(i.dir, generation))
	if err != nil {
		return err
	}
	arena := mf.Bytes()
	if uint64(len(arena)) < size {
		_ = mf.Close()
		return ErrCorrupt
	}
	slots := [storage.SuperblockSlots][]byte{
		arena[0:storage.SuperblockSlotBytes],
		arena[storage.SuperblockSlotBytes : 2*storage.SuperblockSlotBytes],
	}
	_, header, payload, ok := storage.SelectSuperblock(slots, baseMagic, baseVersion)
	if !ok || header.Generation != generation || len(payload) < basePayloadBytes {
		_ = mf.Close()
		return ErrCorrupt
	}
	if binary.LittleEndian.Uint64(payload[0:8]) != count {
		_ = mf.Close()
		return ErrCorrupt
	}
	maxID := common.ImageID(binary.LittleEndian.Uint64(payload[8:16]))
	keyCRC := binary.LittleEndian.Uint32(payload[16:20])
	idCRC := binary.LittleEndian.Uint32(payload[20:24])
	keys, err := storage.View[S2Key](arena, baseHeaderBytes, count, ErrCorrupt, ErrCorrupt)
	if err != nil {
		_ = mf.Close()
		return err
	}
	idsOffset := baseIDsOffset(count)
	ids, err := storage.View[common.ImageID](arena, idsOffset, count, ErrCorrupt, ErrCorrupt)
	if err != nil {
		_ = mf.Close()
		return err
	}
	if count > 0 {
		keyBytes := arena[baseHeaderBytes : baseHeaderBytes+count*8]
		idBytes := arena[idsOffset : idsOffset+count*8]
		if storage.Checksum(keyBytes) != keyCRC || storage.Checksum(idBytes) != idCRC {
			_ = mf.Close()
			return ErrCorrupt
		}
	}
	if err := validateBase(keys, ids, maxID); err != nil {
		_ = mf.Close()
		return err
	}
	i.base = base{keys: keys, ids: ids, mf: mf, gen: generation, maxID: maxID}
	i.publishLocked()
	return nil
}

// validateBase performs a structural integrity pass over a loaded base: every
// key is a valid S2Level cell, every ImageID is non-zero, entries are strictly
// ordered by (key, id), and the recorded maximum ImageID matches. It returns
// ErrCorrupt on any violation so malformed input never reaches a query.
func validateBase(keys []S2Key, ids []common.ImageID, maxID common.ImageID) error {
	if len(keys) != len(ids) {
		return ErrCorrupt
	}
	var prevKey S2Key
	var prevID common.ImageID
	var max common.ImageID
	for n := range keys {
		key, id := keys[n], ids[n]
		if id == 0 {
			return ErrCorrupt
		}
		if _, err := CellIDFromKey(key); err != nil {
			return ErrCorrupt
		}
		if n > 0 && (key < prevKey || (key == prevKey && id <= prevID)) {
			return ErrCorrupt
		}
		if id > max {
			max = id
		}
		prevKey, prevID = key, id
	}
	if max != maxID {
		return ErrCorrupt
	}
	return nil
}

// runCompaction is the background worker: it folds the frozen layer into a new
// base without holding the writer lock during the rewrite, then publishes. On
// failure the frozen layer is retained so nothing is lost and a later trigger
// retries.
func (i *Index) runCompaction() {
	i.mu.Lock()
	snapshot := i.base
	frozen := i.frozen
	i.mu.Unlock()

	var newBase base
	var err error
	if err = failStage(persistCreate); err == nil {
		adds := sortedByKey(frozen)
		count := exactCount(snapshot.keys, snapshot.ids, frozen, adds)
		newBase, err = i.buildBase(snapshot, frozen, adds, count)
	}
	if err == nil {
		err = failStage(persistPublish)
	}

	i.mu.Lock()
	defer i.mu.Unlock()
	if err != nil || i.closed.Load() {
		if newBase.mf != nil {
			_ = newBase.mf.Close()
		}
		i.compactionErr = err
		i.compacting = false
		close(i.compactionDone)
		return
	}
	old := i.base
	i.base = newBase
	i.frozen = nil
	i.compactionErr = nil
	i.deltaMax = maxImageID(0, nil, i.layers, i.active)
	i.publishLocked()
	i.tracker.WaitForPriorReaders()
	// Reclamation is best-effort: the commit is already durable and published,
	// so a cleanup failure must not fail the compaction.
	_ = failStage(persistReclaim)
	if old.mf != nil {
		_ = old.mf.Close()
	}
	if old.gen != 0 {
		_ = failStage(persistRemove)
		_ = storage.RemoveObsolete(i.dir, "base.*.dat", basePath(i.dir, newBase.gen))
	}
	i.compacting = false
	close(i.compactionDone)
}

// buildBase streams base+delta into a new immutable base. For a file-backed
// index it writes and syncs a new generation file and publishes the manifest;
// otherwise it builds heap arrays. It runs without the writer lock.
func (i *Index) buildBase(snapshot base, shadow, adds []mutation, count uint64) (base, error) {
	generation := snapshot.gen + 1
	if !i.persistent {
		keys := make([]S2Key, count)
		ids := make([]common.ImageID, count)
		written := mergeInto(keys, ids, snapshot.keys, snapshot.ids, shadow, adds)
		return base{keys: keys[:written], ids: ids[:written], gen: generation, maxID: maxOfIDs(ids[:written])}, nil
	}
	path := basePath(i.dir, generation)
	size, ok := checkedBaseFileSize(count)
	if !ok || size > math.MaxInt64 {
		return base{}, ErrOverflow
	}
	mf, err := storage.CreateMapped(path, int64(size))
	if err != nil {
		return base{}, err
	}
	arena := mf.Bytes()
	keys, err := storage.View[S2Key](arena, baseHeaderBytes, count, ErrCorrupt, ErrCorrupt)
	if err != nil {
		_ = mf.Close()
		return base{}, err
	}
	idsOffset := baseIDsOffset(count)
	ids, err := storage.View[common.ImageID](arena, idsOffset, count, ErrCorrupt, ErrCorrupt)
	if err != nil {
		_ = mf.Close()
		return base{}, err
	}
	written := mergeInto(keys, ids, snapshot.keys, snapshot.ids, shadow, adds)
	if err := failStage(persistWrite); err != nil {
		_ = mf.Close()
		return base{}, err
	}
	maxID := maxOfIDs(ids[:written])
	var slot [storage.SuperblockSlotBytes]byte
	keyBytes := arena[baseHeaderBytes : baseHeaderBytes+count*8]
	idBytes := arena[idsOffset : idsOffset+count*8]
	if err := encodeBaseMeta(slot[:], generation, uint64(written), uint64(maxID), storage.Checksum(keyBytes), storage.Checksum(idBytes)); err != nil {
		_ = mf.Close()
		return base{}, err
	}
	copy(arena[0:storage.SuperblockSlotBytes], slot[:])
	if err := failStage(persistSync); err != nil {
		_ = mf.Close()
		return base{}, err
	}
	if err := mf.Sync(); err != nil {
		_ = mf.Close()
		return base{}, err
	}
	if err := failStage(persistManifest); err != nil {
		_ = mf.Close()
		return base{}, err
	}
	if err := writeManifest(i.dir, generation, uint64(written)); err != nil {
		_ = mf.Close()
		return base{}, err
	}
	return base{keys: keys[:written], ids: ids[:written], mf: mf, gen: generation, maxID: maxID}, nil
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
	if i.base.mf != nil {
		err = i.base.mf.Close()
		i.base.mf = nil
	}
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

// exactCount returns the number of live (key, imageID) pairs after applying the
// delta, matching mergeInto's dedup of an insert that repeats a live base pair.
func exactCount(baseKeys []S2Key, baseIDs []common.ImageID, shadow, adds []mutation) uint64 {
	count := uint64(0)
	for n := range baseKeys {
		if _, touched := mutationIndex(shadow, baseIDs[n]); !touched {
			count++
		}
	}
	for _, m := range adds {
		if !baseHasLive(baseKeys, baseIDs, shadow, m.key, m.id) {
			count++
		}
	}
	return count
}

func baseHasLive(baseKeys []S2Key, baseIDs []common.ImageID, shadow []mutation, key S2Key, id common.ImageID) bool {
	index := 0
	if len(baseKeys) > 0 {
		index = searchKey(baseKeys, key)
	}
	for index < len(baseKeys) && baseKeys[index] == key {
		if baseIDs[index] == id {
			_, touched := mutationIndex(shadow, id)
			return !touched
		}
		index++
	}
	return false
}

func searchKey(keys []S2Key, key S2Key) int {
	low, high := 0, len(keys)
	for low < high {
		mid := int(uint(low+high) >> 1)
		if keys[mid] < key {
			low = mid + 1
		} else {
			high = mid
		}
	}
	return low
}

// mergeInto writes the retained base entries and additions into keys and ids in
// (key, id) order, skipping base entries shadowed by the delta and de-duplicating
// equal pairs. It returns the number written.
func mergeInto(keys []S2Key, ids []common.ImageID, baseKeys []S2Key, baseIDs []common.ImageID, shadow, adds []mutation) int {
	baseIndex, addIndex, out := 0, 0, 0
	for baseIndex < len(baseKeys) || addIndex < len(adds) {
		for baseIndex < len(baseKeys) {
			if _, touched := mutationIndex(shadow, baseIDs[baseIndex]); touched {
				baseIndex++
				continue
			}
			break
		}
		switch {
		case baseIndex >= len(baseKeys):
			keys[out], ids[out] = adds[addIndex].key, adds[addIndex].id
			out++
			addIndex++
		case addIndex >= len(adds):
			keys[out], ids[out] = baseKeys[baseIndex], baseIDs[baseIndex]
			out++
			baseIndex++
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
