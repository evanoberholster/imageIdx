package geoidx

import (
	"cmp"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"slices"

	common "github.com/evanoberholster/imageIdx/common"
	"github.com/evanoberholster/imageIdx/internal/storage"
)

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
	mf, err := storage.OpenMapped(basePath(i.dir, generation))
	if err != nil {
		return err
	}
	arena := mf.Bytes()
	if uint64(len(arena)) < baseFileSize(count) {
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
	i.base = base{keys: keys, ids: ids, mf: mf, gen: generation, maxID: maxID}
	i.publishLocked()
	return nil
}

// Compact folds the current mutation delta into a new immutable base. A
// file-backed index writes and syncs the new base and publishes the manifest; a
// memory-backed index replaces its heap arrays. The old base is reclaimed after
// a reader grace period.
func (i *Index) Compact() error {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.closed.Load() {
		return ErrClosed
	}
	if len(i.muts) == 0 {
		return nil
	}
	return i.compactLocked()
}

// Checkpoint makes pending mutations durable. It is a no-op for a memory-backed
// index. Close does not checkpoint.
func (i *Index) Checkpoint() error {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.closed.Load() {
		return ErrClosed
	}
	if !i.persistent || len(i.muts) == 0 {
		return nil
	}
	return i.compactLocked()
}

// Sync is an alias for Checkpoint, matching the other index packages.
func (i *Index) Sync() error { return i.Checkpoint() }

func (i *Index) compactLocked() error {
	v := i.view.Load()
	adds := liveAdds(v.muts)
	count := exactCount(v, adds)
	if i.persistent {
		return i.writeBaseLocked(v, adds, count)
	}
	keys := make([]S2Key, count)
	ids := make([]common.ImageID, count)
	written := mergeInto(keys, ids, v, adds)
	i.base = base{keys: keys[:written], ids: ids[:written], gen: i.base.gen + 1, maxID: maxOfIDs(ids[:written])}
	i.muts = nil
	i.publishLocked()
	return nil
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

func (i *Index) writeBaseLocked(v *view, adds []mutation, count uint64) error {
	generation := i.base.gen + 1
	path := basePath(i.dir, generation)
	mf, err := storage.CreateMapped(path, int64(baseFileSize(count)))
	if err != nil {
		return err
	}
	arena := mf.Bytes()
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
	written := mergeInto(keys, ids, v, adds)
	var slot [storage.SuperblockSlotBytes]byte
	keyBytes := arena[baseHeaderBytes : baseHeaderBytes+count*8]
	idBytes := arena[idsOffset : idsOffset+count*8]
	maxID := maxOfIDs(ids[:written])
	if err := encodeBaseMeta(slot[:], generation, uint64(written), uint64(maxID), storage.Checksum(keyBytes), storage.Checksum(idBytes)); err != nil {
		_ = mf.Close()
		return err
	}
	copy(arena[0:storage.SuperblockSlotBytes], slot[:])
	if err := mf.Sync(); err != nil {
		_ = mf.Close()
		return err
	}
	if err := writeManifest(i.dir, generation, uint64(written)); err != nil {
		_ = mf.Close()
		return err
	}
	old := i.base
	i.base = base{keys: keys[:written], ids: ids[:written], mf: mf, gen: generation, maxID: maxID}
	i.muts = nil
	i.publishLocked()
	i.tracker.WaitForPriorReaders()
	if old.mf != nil {
		_ = old.mf.Close()
	}
	if old.gen != 0 {
		_ = storage.RemoveObsolete(i.dir, "base.*.dat", path)
	}
	return nil
}

// Close releases resources. It does not checkpoint; call Checkpoint first to
// make pending mutations durable.
func (i *Index) Close() error {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.closed.Load() {
		return nil
	}
	i.closed.Store(true)
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
	return err
}

// liveAdds returns the live inserts sorted by (key, imageID).
func liveAdds(muts []mutation) []mutation {
	adds := make([]mutation, 0, len(muts))
	for n := range muts {
		if muts[n].add {
			adds = append(adds, muts[n])
		}
	}
	slices.SortFunc(adds, func(a, b mutation) int {
		if order := cmp.Compare(a.key, b.key); order != 0 {
			return order
		}
		return cmp.Compare(a.id, b.id)
	})
	return adds
}

// exactCount returns the number of live (key, imageID) pairs after applying the
// delta, matching mergeInto's dedup of an insert that repeats a live base pair.
func exactCount(v *view, adds []mutation) uint64 {
	count := uint64(0)
	for n := range v.keys {
		if _, mutated := mutationIndex(v.muts, v.ids[n]); !mutated {
			count++
		}
	}
	for _, m := range adds {
		if !baseHasLive(v, m.key, m.id) {
			count++
		}
	}
	return count
}

func baseHasLive(v *view, key S2Key, id common.ImageID) bool {
	index := 0
	if len(v.keys) > 0 {
		index = searchKey(v.keys, key)
	}
	for index < len(v.keys) && v.keys[index] == key {
		if v.ids[index] == id {
			_, mutated := mutationIndex(v.muts, id)
			return !mutated
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

// mergeInto writes the live base and additions into keys and ids in (key, id)
// order, skipping shadowed base entries and de-duplicating equal pairs. It
// returns the number written.
func mergeInto(keys []S2Key, ids []common.ImageID, v *view, adds []mutation) int {
	baseIndex, addIndex, out := 0, 0, 0
	for baseIndex < len(v.keys) || addIndex < len(adds) {
		for baseIndex < len(v.keys) {
			if _, mutated := mutationIndex(v.muts, v.ids[baseIndex]); mutated {
				baseIndex++
				continue
			}
			break
		}
		switch {
		case baseIndex >= len(v.keys):
			keys[out], ids[out] = adds[addIndex].key, adds[addIndex].id
			out++
			addIndex++
		case addIndex >= len(adds):
			keys[out], ids[out] = v.keys[baseIndex], v.ids[baseIndex]
			out++
			baseIndex++
		default:
			baseKey, baseID := v.keys[baseIndex], v.ids[baseIndex]
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
