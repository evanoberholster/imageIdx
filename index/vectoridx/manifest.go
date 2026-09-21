package vectoridx

import (
	"encoding/binary"
	"hash/crc32"
	"os"
	"path/filepath"
	"sort"

	"github.com/evanoberholster/imageIdx/internal/indexutil"
	"github.com/evanoberholster/imageIdx/internal/persist"
)

// Delta checkpoints and manifest (VectorIdx.MD section 8).
//
// A checkpoint appends an immutable delta file holding the mutations since
// the previous checkpoint instead of rewriting the whole base generation.
// The manifest names the base generation and the ordered delta files, so
// Open loads base regions, applies deltas oldest to newest, and then replays
// the WAL tail. Vacuum merges base plus deltas into a fresh base and removes
// the delta files; Compact rebuilds the graph first and then vacuums.
//
// Crash safety: a delta file is written atomically before the manifest that
// names it, so an orphan delta is ignored (and removed by the next vacuum).
// The WAL is truncated only after both are durable.

const (
	manifestMagic   = 0x4E414D56 // "VMAN" little-endian
	manifestVersion = 1

	deltaMagic   = 0x544C4456 // "VDLT" little-endian
	deltaVersion = 1

	deltaTypePut = 1
	deltaTypeDel = 2

	deltaHeaderSize = 8 + 2 + 1 + 1 + 8 + 8 // magic, version, rtype pad, gen, count
	deltaRecFixed   = 4 + 4 + 2 + 2         // id, payloadLen, level, flags
	deltaEntrySize  = 8 + 8 + 8 + 8 + 2     // gen, records, tombstones, bytes, name length
)

// deltaName returns the file name of one delta generation.
func deltaName(base string, gen uint64) string {
	return base + ".delta." + formatGen(gen)
}

func formatGen(gen uint64) string {
	var buf [16]byte
	for i := 15; i >= 0; i-- {
		buf[i] = byte('0' + gen%10)
		gen /= 10
	}
	return string(buf[:])
}

// manifestDelta is one delta generation's entry in the manifest.
type manifestDelta struct {
	gen        uint64
	records    uint64 // put records
	tombstones uint64 // delete records
	bytes      uint64 // file size
	name       string
}

// manifestData is the decoded manifest: the base generation plus ordered
// deltas and the checkpoint LSN they cover.
type manifestData struct {
	baseGeneration uint64
	checkpointLSN  uint64
	deltas         []manifestDelta
}

func (m *manifestData) clone() *manifestData {
	c := *m
	c.deltas = append([]manifestDelta(nil), m.deltas...)
	return &c
}

// deltaRecord is one mutation in a delta file. Put records carry the level
// the writer assigned so replay does not depend on the RNG.
type deltaRecord struct {
	rtype uint8
	level uint16
	id    NodeID
	raw   []byte
}

// encodeManifest serializes the manifest with a trailing CRC32.
func encodeManifest(m *manifestData) []byte {
	size := 4 + 2 + 2 + 8 + 8 + 2 + len(m.deltas)*deltaEntrySize
	for _, d := range m.deltas {
		size += len(d.name)
	}
	size += 4
	return persist.BuildManifest(size, manifestMagic, manifestVersion, size-4, size-4, func(data []byte) {
		binary.LittleEndian.PutUint64(data[8:16], m.baseGeneration)
		binary.LittleEndian.PutUint64(data[16:24], m.checkpointLSN)
		binary.LittleEndian.PutUint16(data[24:26], uint16(len(m.deltas)))
		off := 26
		for _, d := range m.deltas {
			binary.LittleEndian.PutUint64(data[off:], d.gen)
			binary.LittleEndian.PutUint64(data[off+8:], d.records)
			binary.LittleEndian.PutUint64(data[off+16:], d.tombstones)
			binary.LittleEndian.PutUint64(data[off+24:], d.bytes)
			binary.LittleEndian.PutUint16(data[off+32:], uint16(len(d.name)))
			copy(data[off+34:], d.name)
			off += deltaEntrySize + len(d.name)
		}
	})
}

// decodeManifest validates and parses manifest bytes.
func decodeManifest(data []byte) (*manifestData, error) {
	if len(data) < 26+4 {
		return nil, ErrCorruptStorage
	}
	if err := persist.VerifyManifest(data, len(data), manifestMagic, manifestVersion, len(data)-4, len(data)-4); err != nil {
		return nil, ErrCorruptStorage
	}
	m := &manifestData{
		baseGeneration: binary.LittleEndian.Uint64(data[8:16]),
		checkpointLSN:  binary.LittleEndian.Uint64(data[16:24]),
	}
	count := int(binary.LittleEndian.Uint16(data[24:26]))
	off := 26
	for i := 0; i < count; i++ {
		if off+deltaEntrySize > len(data)-4 {
			return nil, ErrCorruptStorage
		}
		d := manifestDelta{
			gen:        binary.LittleEndian.Uint64(data[off:]),
			records:    binary.LittleEndian.Uint64(data[off+8:]),
			tombstones: binary.LittleEndian.Uint64(data[off+16:]),
			bytes:      binary.LittleEndian.Uint64(data[off+24:]),
		}
		nameLen := int(binary.LittleEndian.Uint16(data[off+32:]))
		if off+deltaEntrySize+nameLen > len(data)-4 {
			return nil, ErrCorruptStorage
		}
		d.name = string(data[off+34 : off+34+nameLen])
		if d.name == "" || filepath.Base(d.name) != d.name {
			return nil, ErrCorruptStorage
		}
		m.deltas = append(m.deltas, d)
		off += deltaEntrySize + nameLen
	}
	return m, nil
}

// writeManifestLocked publishes the manifest atomically. Caller holds writeMu.
func (v *VectorIdx) writeManifestLocked(m *manifestData) error {
	name := filepath.Base(v.path) + ".manifest"
	if err := indexutil.WriteFileAtomic(filepath.Dir(v.path), name+".tmp", name, encodeManifest(m), 0o644); err != nil {
		return err
	}
	v.manifest.Store(m.clone())
	return nil
}

// readManifest loads the manifest, returning nil when none exists (legacy
// single-file index).
func readManifest(base string) (*manifestData, error) {
	data, err := os.ReadFile(base + ".manifest")
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	return decodeManifest(data)
}

// encodeDeltaFile serializes one delta generation with a trailing CRC32.
func encodeDeltaFile(gen uint64, records []deltaRecord) []byte {
	size := deltaHeaderSize + 4
	for _, r := range records {
		size += deltaRecFixed + len(r.raw)
	}
	data := make([]byte, size)
	binary.LittleEndian.PutUint32(data[0:4], deltaMagic)
	binary.LittleEndian.PutUint16(data[4:6], deltaVersion)
	data[6] = 0
	data[7] = 0
	binary.LittleEndian.PutUint64(data[8:16], gen)
	binary.LittleEndian.PutUint64(data[16:24], uint64(len(records)))
	off := deltaHeaderSize
	for _, r := range records {
		binary.LittleEndian.PutUint32(data[off:], uint32(r.id))
		binary.LittleEndian.PutUint32(data[off+4:], uint32(len(r.raw)))
		binary.LittleEndian.PutUint16(data[off+8:], r.level)
		data[off+10] = r.rtype
		data[off+11] = 0
		copy(data[off+deltaRecFixed:], r.raw)
		off += deltaRecFixed + len(r.raw)
	}
	binary.LittleEndian.PutUint32(data[size-4:], crc32.ChecksumIEEE(data[:size-4]))
	return data
}

// decodeDeltaFile parses one delta generation.
func decodeDeltaFile(data []byte) (uint64, []deltaRecord, error) {
	if len(data) < deltaHeaderSize+4 {
		return 0, nil, ErrCorruptStorage
	}
	if binary.LittleEndian.Uint32(data[0:4]) != deltaMagic ||
		binary.LittleEndian.Uint16(data[4:6]) != deltaVersion {
		return 0, nil, ErrCorruptStorage
	}
	if binary.LittleEndian.Uint32(data[len(data)-4:]) != crc32.ChecksumIEEE(data[:len(data)-4]) {
		return 0, nil, ErrCorruptStorage
	}
	gen := binary.LittleEndian.Uint64(data[8:16])
	count := binary.LittleEndian.Uint64(data[16:24])
	if count > uint64(len(data)) {
		return 0, nil, ErrCorruptStorage
	}
	records := make([]deltaRecord, 0, count)
	off := deltaHeaderSize
	for i := uint64(0); i < count; i++ {
		if off+deltaRecFixed > len(data)-4 {
			return 0, nil, ErrCorruptStorage
		}
		r := deltaRecord{
			id:    NodeID(binary.LittleEndian.Uint32(data[off:])),
			level: binary.LittleEndian.Uint16(data[off+8:]),
			rtype: data[off+10],
		}
		payloadLen := int(binary.LittleEndian.Uint32(data[off+4:]))
		if off+deltaRecFixed+payloadLen > len(data)-4 {
			return 0, nil, ErrCorruptStorage
		}
		switch r.rtype {
		case deltaTypePut:
			raw := make([]byte, payloadLen)
			copy(raw, data[off+deltaRecFixed:off+deltaRecFixed+payloadLen])
			r.raw = raw
		case deltaTypeDel:
			if payloadLen != 0 {
				return 0, nil, ErrCorruptStorage
			}
		default:
			return 0, nil, ErrCorruptStorage
		}
		records = append(records, r)
		off += deltaRecFixed + payloadLen
	}
	if off != len(data)-4 {
		return 0, nil, ErrCorruptStorage
	}
	return gen, records, nil
}

// loadDeltaFile reads and decodes one delta generation.
func loadDeltaFile(path string) ([]deltaRecord, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrCorruptStorage
		}
		return nil, err
	}
	_, records, err := decodeDeltaFile(data)
	return records, err
}

// deltaFragmentation reports the fraction of delta records that are
// tombstones or superseded puts, over all deltas. It is zero when clean.
func (m *manifestData) fragmentation() float64 {
	var records, tombstones uint64
	for _, d := range m.deltas {
		records += d.records + d.tombstones
		tombstones += d.tombstones
	}
	if records == 0 {
		return 0
	}
	return float64(tombstones) / float64(records)
}

// removeDeltaFiles deletes delta files not named by keep.
func removeDeltaFiles(base string, keep map[string]struct{}) error {
	paths, err := filepath.Glob(base + ".delta.*")
	if err != nil {
		return err
	}
	sort.Strings(paths)
	for _, path := range paths {
		if _, ok := keep[filepath.Base(path)]; ok {
			continue
		}
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

// checkpointLocked appends the mutations since the last checkpoint as one
// delta generation and publishes it in the manifest, then removes WAL
// coverage. Caller holds the writer lock.
func (v *VectorIdx) checkpointLocked() error {
	if len(v.pending) == 0 {
		return nil
	}
	records := v.collectPendingLocked()
	gen := v.nextDeltaGen
	name := filepath.Base(deltaName(v.path, gen))
	data := encodeDeltaFile(gen, records)
	dir := filepath.Dir(v.path)
	if err := indexutil.WriteFileAtomic(dir, name+".tmp", name, data, 0o644); err != nil {
		return err
	}
	m := v.manifest.Load().clone()
	var puts, dels uint64
	for _, r := range records {
		if r.rtype == deltaTypeDel {
			dels++
		} else {
			puts++
		}
	}
	m.deltas = append(m.deltas, manifestDelta{
		gen:        gen,
		records:    puts,
		tombstones: dels,
		bytes:      uint64(len(data)),
		name:       name,
	})
	m.checkpointLSN = v.lsn
	if err := v.writeManifestLocked(m); err != nil {
		return err
	}
	if v.pendingMutations > len(records) {
		v.coalescedRecords.Add(uint64(v.pendingMutations - len(records)))
	}
	v.pending = nil
	v.pendingMutations = 0
	v.nextDeltaGen = gen + 1
	v.checkpointLSN = v.lsn
	v.checkpointLSNPub.Store(v.lsn)
	return v.truncateWALLocked()
}

// collectPendingLocked materializes the final state of every pending NodeID
// as delta records: live nodes become puts carrying level and vector, and
// tombstoned nodes become deletes. Superseded intermediate mutations are
// dropped here (record promotion).
func (v *VectorIdx) collectPendingLocked() []deltaRecord {
	ids := make([]NodeID, 0, len(v.pending))
	for id := range v.pending {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	cur := v.view.Load()
	records := make([]deltaRecord, 0, len(ids))
	for _, id := range ids {
		ord, ok := cur.locate(id)
		if !ok {
			continue
		}
		if cur.tomb.Contains(uint64(id)) {
			records = append(records, deltaRecord{rtype: deltaTypeDel, id: id})
			continue
		}
		raw := make([]byte, v.vecBytes)
		copy(raw, cur.vectorAt(ord))
		records = append(records, deltaRecord{
			rtype: deltaTypePut,
			level: uint16(cur.levels[ord]),
			id:    id,
			raw:   raw,
		})
	}
	return records
}

// applyDeltasLocked replays published delta generations in order on top of
// the loaded base. It runs during Open before the WAL tail is replayed.
func (v *VectorIdx) applyDeltasLocked(m *manifestData) error {
	if m == nil || len(m.deltas) == 0 {
		return nil
	}
	wal := v.wal
	v.wal = nil
	v.loadingDelta = true
	defer func() {
		v.loadingDelta = false
		v.wal = wal
	}()
	dir := filepath.Dir(v.path)
	for _, d := range m.deltas {
		records, err := loadDeltaFile(filepath.Join(dir, d.name))
		if err != nil {
			return err
		}
		for _, r := range records {
			cur := v.view.Load()
			switch r.rtype {
			case deltaTypePut:
				ord, exists := cur.locate(r.id)
				var ns *vectorSnapshot
				var err error
				if exists {
					ns, err = v.resurrectLocked(cur, ord, r.id, r.raw)
				} else {
					ns, err = v.addRawLockedAtLevel(cur, r.id, r.raw, int(r.level))
				}
				if err != nil {
					return err
				}
				v.view.Store(ns)
			case deltaTypeDel:
				if _, ok := cur.locate(r.id); !ok {
					continue
				}
				ns := cloneSnapshot(cur)
				ns.tombSorted = insertTombSorted(ns.tombSorted, r.id)
				ns.tomb, ns.tombSorted = rebuildTombeset(ns.tombSorted)
				ns.gen = v.gen.Add(1)
				v.view.Store(ns)
			}
		}
	}
	return nil
}

// vacuumLocked writes the current state as a fresh base generation, resets
// the manifest, and removes delta files and WAL coverage. Caller holds the
// writer lock.
func (v *VectorIdx) vacuumLocked() error {
	if err := v.writeBaseLocked(); err != nil {
		return err
	}
	m := &manifestData{baseGeneration: v.gen.Load(), checkpointLSN: v.lsn}
	if err := v.writeManifestLocked(m); err != nil {
		return err
	}
	v.pending = nil
	v.pendingMutations = 0
	v.coalescedRecords.Store(0)
	v.nextDeltaGen = v.gen.Load() + 1
	if err := removeDeltaFiles(v.path, nil); err != nil {
		return err
	}
	return v.truncateWALLocked()
}

// Vacuum merges the base generation and every delta into a fresh base,
// dropping superseded and tombstone records, then removes the delta files
// and the covered WAL. It does not rebuild the graph; Compact does that
// first.
func (v *VectorIdx) Vacuum() error {
	v.writeMu.Lock()
	defer v.writeMu.Unlock()
	if v.closed.Load() {
		return ErrClosed
	}
	if v.path == "" {
		return ErrNotPersistent
	}
	return v.vacuumLocked()
}

// markPendingLocked records that id changed since the last checkpoint.
func (v *VectorIdx) markPendingLocked(id NodeID) {
	if v.path == "" || v.loadingDelta {
		return
	}
	if v.pending == nil {
		v.pending = make(map[NodeID]struct{})
	}
	v.pending[id] = struct{}{}
	v.pendingMutations++
}
