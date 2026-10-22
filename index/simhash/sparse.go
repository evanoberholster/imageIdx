package simhash

import (
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	schema "github.com/evanoberholster/imageIdx/schema"
)

const (
	sparseManifestMagic  uint32 = 0x53534D32 // "SSM2"
	sparseSegmentMagic   uint32 = 0x53534732 // "SSG2"
	sparseFormatVersion  uint16 = 2
	sparseManifestHeader        = 40
	sparseSegmentHeader         = 24
	sparseRecordBytes           = 50
	sparseCRCBytes              = 4
	maxSparseSegments           = 64
	sparseWriteChunk            = 32 << 10
)

var sparseCRCTable = crc32.MakeTable(crc32.IEEE)

// sparseFail is a test-only crash seam. It is nil in production.
var sparseFail func(string) error

func sparseHook(stage string) error {
	if sparseFail == nil {
		return nil
	}
	return sparseFail(stage)
}

// sparseRecord is one sorted external-ID record. Deleted records are
// tombstones; they suppress older versions until compaction removes them.
type sparseRecord struct {
	id      schema.ImageID
	record  Record
	deleted bool
}

// sparseSegment is one immutable, generation-ordered sorted record run.
type sparseSegment struct {
	id      uint64
	records []sparseRecord
}

// sparseView is the immutable reader-visible set of sparse records. The base
// segment and overlay slices are never mutated after publication.
type sparseView struct {
	base    sparseSegment
	overlay []sparseRecord // newest-wins merged delta records, tombstones kept
	live    uint64
}

type sparseState struct {
	segments         []sparseSegment
	generation       uint64
	sourceGeneration uint64
	live             uint64
	stale            uint64
}

type sparseCursor struct {
	records    []sparseRecord
	pos        int
	generation uint64
}

func openSparse(l layout) (sparseState, error) {
	if _, err := os.Stat(l.sparseLegacyPath()); err == nil {
		return sparseState{}, fmt.Errorf("%w: legacy %s; rebuild the index", ErrUnsupportedSparseFormat, l.sparseLegacyName())
	} else if !os.IsNotExist(err) {
		return sparseState{}, err
	}
	data, err := os.ReadFile(l.sparseManifestPath())
	if os.IsNotExist(err) {
		if err := cleanupSparseOrphans(l, nil); err != nil {
			return sparseState{}, err
		}
		return sparseState{}, nil
	}
	if err != nil {
		return sparseState{}, err
	}
	state, ids, err := parseSparseManifest(data)
	if err != nil {
		return sparseState{}, err
	}
	keep := make(map[uint64]bool, len(ids))
	for _, id := range ids {
		keep[id] = true
	}
	segments := make([]sparseSegment, 0, len(ids))
	var total uint64
	for _, id := range ids {
		seg, err := readSparseSegment(l, id)
		if err != nil {
			return sparseState{}, err
		}
		total += uint64(len(seg.records))
		segments = append(segments, seg)
	}
	if state.live > total {
		return sparseState{}, fmt.Errorf("%w: sparse live count exceeds records", ErrCorrupt)
	}
	state.segments = segments
	tombstones := sparseTombstoneCount(segments)
	if total < state.live+tombstones {
		return sparseState{}, fmt.Errorf("%w: sparse counts", ErrCorrupt)
	}
	state.stale = total - state.live - tombstones
	if err := cleanupSparseOrphans(l, keep); err != nil {
		return sparseState{}, err
	}
	return state, nil
}

func parseSparseManifest(data []byte) (sparseState, []uint64, error) {
	if len(data) < sparseManifestHeader+sparseCRCBytes {
		return sparseState{}, nil, fmt.Errorf("%w: short sparse manifest", ErrCorrupt)
	}
	if binary.LittleEndian.Uint32(data[0:4]) != sparseManifestMagic || binary.LittleEndian.Uint16(data[4:6]) != sparseFormatVersion {
		return sparseState{}, nil, fmt.Errorf("%w: sparse manifest header", ErrCorrupt)
	}
	if binary.LittleEndian.Uint16(data[6:8]) != 0 {
		return sparseState{}, nil, fmt.Errorf("%w: sparse manifest flags", ErrCorrupt)
	}
	generation := binary.LittleEndian.Uint64(data[8:16])
	sourceGeneration := binary.LittleEndian.Uint64(data[16:24])
	live := binary.LittleEndian.Uint64(data[24:32])
	count := binary.LittleEndian.Uint32(data[32:36])
	if binary.LittleEndian.Uint32(data[36:40]) != 0 || count > maxSparseSegments ||
		len(data) != sparseManifestHeader+int(count)*8+sparseCRCBytes ||
		binary.LittleEndian.Uint32(data[len(data)-4:]) != crc32.ChecksumIEEE(data[:len(data)-4]) {
		return sparseState{}, nil, fmt.Errorf("%w: sparse manifest body", ErrCorrupt)
	}
	ids := make([]uint64, count)
	for i := range ids {
		ids[i] = binary.LittleEndian.Uint64(data[sparseManifestHeader+i*8:])
		if ids[i] == 0 || (i > 0 && ids[i] <= ids[i-1]) {
			return sparseState{}, nil, fmt.Errorf("%w: sparse segment ids", ErrCorrupt)
		}
	}
	if count == 0 {
		if generation != 0 {
			return sparseState{}, nil, fmt.Errorf("%w: sparse generation", ErrCorrupt)
		}
	} else if generation != ids[len(ids)-1] {
		return sparseState{}, nil, fmt.Errorf("%w: sparse generation", ErrCorrupt)
	}
	return sparseState{generation: generation, sourceGeneration: sourceGeneration, live: live}, ids, nil
}

func encodeSparseManifest(ids []uint64, generation, sourceGeneration, live uint64) []byte {
	size := sparseManifestHeader + len(ids)*8 + sparseCRCBytes
	data := make([]byte, size)
	binary.LittleEndian.PutUint32(data[0:4], sparseManifestMagic)
	binary.LittleEndian.PutUint16(data[4:6], sparseFormatVersion)
	binary.LittleEndian.PutUint64(data[8:16], generation)
	binary.LittleEndian.PutUint64(data[16:24], sourceGeneration)
	binary.LittleEndian.PutUint64(data[24:32], live)
	binary.LittleEndian.PutUint32(data[32:36], uint32(len(ids)))
	for i, id := range ids {
		binary.LittleEndian.PutUint64(data[sparseManifestHeader+i*8:], id)
	}
	binary.LittleEndian.PutUint32(data[len(data)-4:], crc32.ChecksumIEEE(data[:len(data)-4]))
	return data
}

func readSparseSegment(l layout, id uint64) (sparseSegment, error) {
	path := l.sparseSegmentPath(id)
	f, err := os.Open(path)
	if err != nil {
		return sparseSegment{}, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return sparseSegment{}, err
	}
	var header [sparseSegmentHeader]byte
	if _, err := io.ReadFull(f, header[:]); err != nil {
		return sparseSegment{}, fmt.Errorf("%w: sparse segment header", ErrCorrupt)
	}
	if binary.LittleEndian.Uint32(header[0:4]) != sparseSegmentMagic ||
		binary.LittleEndian.Uint16(header[4:6]) != sparseFormatVersion ||
		binary.LittleEndian.Uint16(header[6:8]) != sparseSegmentHeader ||
		binary.LittleEndian.Uint64(header[8:16]) != id ||
		binary.LittleEndian.Uint32(header[20:24]) != 0 {
		return sparseSegment{}, fmt.Errorf("%w: sparse segment header %d", ErrCorrupt, id)
	}
	count := binary.LittleEndian.Uint32(header[16:20])
	expected := uint64(sparseSegmentHeader) + uint64(count)*sparseRecordBytes + sparseCRCBytes
	if uint64(info.Size()) != expected {
		return sparseSegment{}, fmt.Errorf("%w: sparse segment size %d", ErrCorrupt, id)
	}
	records := make([]sparseRecord, count)
	crc := crc32.Update(0, sparseCRCTable, header[:])
	buf := make([]byte, 1024*sparseRecordBytes)
	remaining := int(count)
	pos := 0
	for remaining > 0 {
		n := remaining
		if n > 1024 {
			n = 1024
		}
		body := buf[:n*sparseRecordBytes]
		if _, err := io.ReadFull(f, body); err != nil {
			return sparseSegment{}, fmt.Errorf("%w: sparse segment body %d", ErrCorrupt, id)
		}
		crc = crc32.Update(crc, sparseCRCTable, body)
		for i := 0; i < n; i++ {
			frame := body[i*sparseRecordBytes : (i+1)*sparseRecordBytes]
			rec, err := decodeSparseRecord(frame)
			if err != nil {
				return sparseSegment{}, fmt.Errorf("%w: sparse segment %d", err, id)
			}
			if pos > 0 && rec.id <= records[pos-1].id {
				return sparseSegment{}, fmt.Errorf("%w: sparse segment order %d", ErrCorrupt, id)
			}
			records[pos] = rec
			pos++
		}
		remaining -= n
	}
	var footer [sparseCRCBytes]byte
	if _, err := io.ReadFull(f, footer[:]); err != nil || binary.LittleEndian.Uint32(footer[:]) != crc {
		return sparseSegment{}, fmt.Errorf("%w: sparse segment crc %d", ErrCorrupt, id)
	}
	return sparseSegment{id: id, records: records}, nil
}

func decodeSparseRecord(frame []byte) (sparseRecord, error) {
	if len(frame) != sparseRecordBytes || binary.LittleEndian.Uint64(frame[0:8]) == 0 || frame[49] > 1 {
		return sparseRecord{}, ErrCorrupt
	}
	var pdq PDQHash
	copy(pdq[:], frame[16:48])
	return sparseRecord{
		id: schema.ImageID(binary.LittleEndian.Uint64(frame[0:8])),
		record: Record{
			PHash:   binary.LittleEndian.Uint64(frame[8:16]),
			PDQ:     pdq,
			Quality: frame[48],
		},
		deleted: frame[49] == 1,
	}, nil
}

func appendSparseRecordFrame(dst []byte, rec sparseRecord) []byte {
	start := len(dst)
	dst = append(dst, make([]byte, sparseRecordBytes)...)
	frame := dst[start:]
	binary.LittleEndian.PutUint64(frame[0:8], uint64(rec.id))
	binary.LittleEndian.PutUint64(frame[8:16], rec.record.PHash)
	copy(frame[16:48], rec.record.PDQ[:])
	frame[48] = rec.record.Quality
	if rec.deleted {
		frame[49] = 1
	}
	return dst
}

func (st *Store) writeSparseSegment(id uint64, records []sparseRecord, sync bool) error {
	if uint64(len(records)) > uint64(^uint32(0)) {
		return ErrBatchTooLarge
	}
	if err := sparseHook("segment-before"); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(st.layout.dir, st.layout.sparseTempPattern())
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	keep := false
	defer func() {
		_ = tmp.Close()
		if !keep {
			_ = os.Remove(tmpName)
		}
	}()
	var header [sparseSegmentHeader]byte
	binary.LittleEndian.PutUint32(header[0:4], sparseSegmentMagic)
	binary.LittleEndian.PutUint16(header[4:6], sparseFormatVersion)
	binary.LittleEndian.PutUint16(header[6:8], sparseSegmentHeader)
	binary.LittleEndian.PutUint64(header[8:16], id)
	binary.LittleEndian.PutUint32(header[16:20], uint32(len(records)))
	if _, err := tmp.Write(header[:]); err != nil {
		return err
	}
	crc := crc32.Update(0, sparseCRCTable, header[:])
	if cap(st.sparseWriteBuf) < sparseWriteChunk {
		st.sparseWriteBuf = make([]byte, 0, sparseWriteChunk)
	}
	buf := st.sparseWriteBuf[:0]
	flush := func() error {
		if len(buf) == 0 {
			return nil
		}
		if _, err := tmp.Write(buf); err != nil {
			return err
		}
		crc = crc32.Update(crc, sparseCRCTable, buf)
		buf = buf[:0]
		return nil
	}
	for _, rec := range records {
		buf = appendSparseRecordFrame(buf, rec)
		if len(buf) >= sparseWriteChunk {
			if err := flush(); err != nil {
				return err
			}
		}
	}
	if err := flush(); err != nil {
		return err
	}
	var footer [sparseCRCBytes]byte
	binary.LittleEndian.PutUint32(footer[:], crc)
	if _, err := tmp.Write(footer[:]); err != nil {
		return err
	}
	st.sparseWriteBuf = buf
	if err := sparseHook("segment-before-sync"); err != nil {
		return err
	}
	if sync {
		if err := tmp.Sync(); err != nil {
			return err
		}
	}
	if err := sparseHook("segment-before-rename"); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, st.layout.sparseSegmentPath(id)); err != nil {
		return err
	}
	keep = true
	if sync {
		if err := syncDirectory(st.layout.dir); err != nil {
			return err
		}
	}
	return sparseHook("segment-after-rename")
}

func (st *Store) writeSparseManifest(ids []uint64, generation, sourceGeneration, live uint64, sync bool) error {
	if err := sparseHook("manifest-before"); err != nil {
		return err
	}
	data := encodeSparseManifest(ids, generation, sourceGeneration, live)
	tmp, err := os.CreateTemp(st.layout.dir, st.layout.sparseTempPattern())
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	keep := false
	defer func() {
		_ = tmp.Close()
		if !keep {
			_ = os.Remove(tmpName)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		return err
	}
	if sync {
		if err := tmp.Sync(); err != nil {
			return err
		}
	}
	if err := sparseHook("manifest-before-rename"); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, st.layout.sparseManifestPath()); err != nil {
		return err
	}
	keep = true
	if sync {
		if err := syncDirectory(st.layout.dir); err != nil {
			return err
		}
	}
	return sparseHook("manifest-after-rename")
}

func syncDirectory(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	err = f.Sync()
	closeErr := f.Close()
	if err != nil {
		return err
	}
	return closeErr
}

func cleanupSparseOrphans(l layout, keep map[uint64]bool) error {
	entries, err := os.ReadDir(l.dir)
	if err != nil {
		return err
	}
	segPrefix := "sparse-"
	tempPrefix := "sparse.tmp-"
	if l.prefix != "" {
		segPrefix = l.prefix + ".sparse-"
		tempPrefix = l.prefix + ".sparse.tmp-"
	}
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(name, tempPrefix) {
			_ = os.Remove(filepath.Join(l.dir, name))
			continue
		}
		if !strings.HasPrefix(name, segPrefix) || !strings.HasSuffix(name, ".seg") {
			continue
		}
		hex := strings.TrimSuffix(strings.TrimPrefix(name, segPrefix), ".seg")
		id, err := strconv.ParseUint(hex, 16, 64)
		if err != nil || id == 0 {
			continue
		}
		if keep != nil && keep[id] {
			continue
		}
		_ = os.Remove(filepath.Join(l.dir, name))
	}
	return nil
}

func sparseIndex(records []sparseRecord, id schema.ImageID) int {
	lo, hi := 0, len(records)
	for lo < hi {
		mid := int(uint(lo+hi) >> 1)
		if records[mid].id < id {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	return lo
}

func sparseLookup(segments []sparseSegment, id schema.ImageID) (sparseRecord, bool) {
	for i := len(segments) - 1; i >= 0; i-- {
		records := segments[i].records
		index := sparseIndex(records, id)
		if index < len(records) && records[index].id == id {
			return records[index], true
		}
	}
	return sparseRecord{}, false
}

func nextSparseRecord(cursors []sparseCursor) (sparseRecord, bool) {
	var minID schema.ImageID
	found := false
	for i := range cursors {
		if cursors[i].pos < len(cursors[i].records) {
			id := cursors[i].records[cursors[i].pos].id
			if !found || id < minID {
				minID, found = id, true
			}
		}
	}
	if !found {
		return sparseRecord{}, false
	}
	var chosen sparseRecord
	var chosenGeneration uint64
	chosenSet := false
	for i := range cursors {
		records := cursors[i].records
		for cursors[i].pos < len(records) && records[cursors[i].pos].id == minID {
			rec := records[cursors[i].pos]
			if !chosenSet || cursors[i].generation > chosenGeneration {
				chosen, chosenGeneration, chosenSet = rec, cursors[i].generation, true
			}
			cursors[i].pos++
		}
	}
	return chosen, true
}

func nextSparseLive(cursors []sparseCursor) (sparseRecord, bool) {
	for {
		rec, ok := nextSparseRecord(cursors)
		if !ok {
			return sparseRecord{}, false
		}
		if !rec.deleted {
			return rec, true
		}
	}
}

func mergeSparseOverlay(segments []sparseSegment) []sparseRecord {
	if len(segments) == 0 {
		return nil
	}
	cursors := make([]sparseCursor, len(segments))
	for i, seg := range segments {
		cursors[i] = sparseCursor{records: seg.records, generation: seg.id}
	}
	out := make([]sparseRecord, 0, sparseLiveLowerBound(segments))
	for {
		rec, ok := nextSparseRecord(cursors)
		if !ok {
			return out
		}
		out = append(out, rec)
	}
}

func makeSparseView(segments []sparseSegment, live uint64) *sparseView {
	view := &sparseView{live: live}
	if len(segments) == 0 {
		return view
	}
	view.base = segments[0]
	view.overlay = mergeSparseOverlay(segments[1:])
	return view
}

func mergeSparseSegments(segments []sparseSegment, committed uint64) []sparseRecord {
	cursors := make([]sparseCursor, len(segments))
	for i, seg := range segments {
		cursors[i] = sparseCursor{records: seg.records, generation: seg.id}
	}
	merged := make([]sparseRecord, 0, sparseLiveLowerBound(segments))
	for {
		rec, ok := nextSparseRecord(cursors)
		if !ok {
			return merged
		}
		// A tombstone above the dense high-water mark has no older dense
		// record to suppress and can be retired. Tombstones at or below it
		// must survive so a compacted sparse view still overrides dense data.
		if rec.deleted && uint64(rec.id) > committed {
			continue
		}
		merged = append(merged, rec)
	}
}

func sparseTombstoneCount(segments []sparseSegment) uint64 {
	var count uint64
	for _, seg := range segments {
		for _, rec := range seg.records {
			if rec.deleted {
				count++
			}
		}
	}
	return count
}

func sparseLiveLowerBound(segments []sparseSegment) int {
	total := 0
	for _, seg := range segments {
		total += len(seg.records)
	}
	return total
}

func sparseDeltaSegments(segments []sparseSegment) []sparseSegment {
	if len(segments) <= 1 {
		return nil
	}
	return segments[1:]
}

func sparseTotalRecords(segments []sparseSegment) uint64 {
	var total uint64
	for _, seg := range segments {
		total += uint64(len(seg.records))
	}
	return total
}

// SparseGeneration returns the durable sparse segment generation.
func (st *Store) SparseGeneration() uint64 {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.sparseGeneration
}

// SparseSourceGeneration returns the index publication generation represented
// by the durable sparse state.
func (st *Store) SparseSourceGeneration() uint64 {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.sparseSourceGeneration
}

// SparseSegmentCount returns the active compacted/delta segment count.
func (st *Store) SparseSegmentCount() int {
	st.mu.Lock()
	defer st.mu.Unlock()
	return len(st.sparseSegments)
}

func (st *Store) sparseSnapshot() *sparseView {
	return st.sparseView.Load()
}

func (v *sparseView) contains(id schema.ImageID) bool {
	if v == nil {
		return false
	}
	if j := sparseIndex(v.overlay, id); j < len(v.overlay) && v.overlay[j].id == id {
		return true
	}
	j := sparseIndex(v.base.records, id)
	return j < len(v.base.records) && v.base.records[j].id == id
}

func (st *Store) sparseRecord(id schema.ImageID) (sparseRecord, bool) {
	view := st.sparseSnapshot()
	if view == nil {
		return sparseRecord{}, false
	}
	if j := sparseIndex(view.overlay, id); j < len(view.overlay) && view.overlay[j].id == id {
		return view.overlay[j], true
	}
	j := sparseIndex(view.base.records, id)
	if j < len(view.base.records) && view.base.records[j].id == id {
		return view.base.records[j], true
	}
	return sparseRecord{}, false
}

// ApplyUpdates is the public arbitrary-external-ID update entry point. It keeps
// the dense append fast path for contiguous IDs and falls back to the durable
// sparse segment overlay for gaps, high IDs and out-of-order upserts.
func (st *Store) ApplyUpdates(updates []Update) error {
	return st.applyUpdates(updates, nil, 0)
}

// ApplyUpdatesAt is ApplyUpdates carrying the index publication generation.
// Replaying an interval whose source generation is already represented is a
// no-op.
func (st *Store) ApplyUpdatesAt(updates []Update, sourceGeneration uint64) error {
	return st.applyUpdates(updates, nil, sourceGeneration)
}

// DeleteUpdates writes sparse tombstones for external IDs. The unified index
// normally relies on its liveness bitmap for deletes; this API completes the
// sparse store's own newest-wins semantics and crash tests.
func (st *Store) DeleteUpdates(ids []schema.ImageID) error {
	return st.applyUpdates(nil, ids, 0)
}

func (st *Store) applyUpdates(updates []Update, deletes []schema.ImageID, sourceGeneration uint64) error {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.closed.Load() {
		return ErrClosed
	}
	if len(updates) == 0 && len(deletes) == 0 {
		return nil
	}
	if len(updates) > maxBatchRecords || len(deletes) > maxBatchRecords {
		return ErrBatchTooLarge
	}
	for i := range updates {
		if updates[i].ImageID == 0 {
			return ErrInvalidID
		}
	}
	for _, id := range deletes {
		if id == 0 {
			return ErrInvalidID
		}
	}
	if st.sparseUsed && sourceGeneration != 0 && st.sparseSourceGeneration != 0 && sourceGeneration <= st.sparseSourceGeneration {
		return nil
	}
	if !st.sparseUsed && len(deletes) == 0 {
		denseExisting := true
		for i := range updates {
			if uint64(updates[i].ImageID) > st.committed {
				denseExisting = false
				break
			}
		}
		if denseExisting {
			return st.upsertLocked(updates)
		}
		denseAppend := true
		expected := st.committed + 1
		for i := range updates {
			if uint64(updates[i].ImageID) != expected {
				denseAppend = false
				break
			}
			expected++
		}
		if denseAppend {
			if cap(st.denseScratch) < len(updates) {
				st.denseScratch = make([]Record, len(updates))
			}
			recs := st.denseScratch[:len(updates)]
			for i := range updates {
				recs[i] = updates[i].Record
			}
			_, err := st.appendBatchLocked(recs)
			return err
		}
	}
	return st.applySparseLocked(updates, deletes, sourceGeneration)
}

func (st *Store) applySparseLocked(updates []Update, deletes []schema.ImageID, sourceGeneration uint64) error {
	ops := make([]sparseRecord, 0, len(updates)+len(deletes))
	for _, update := range updates {
		ops = append(ops, sparseRecord{id: update.ImageID, record: update.Record})
	}
	for _, id := range deletes {
		ops = append(ops, sparseRecord{id: id, deleted: true})
	}
	slices.SortStableFunc(ops, func(a, b sparseRecord) int {
		if a.id < b.id {
			return -1
		}
		if a.id > b.id {
			return 1
		}
		return 0
	})
	unique := ops[:0]
	for _, op := range ops {
		if len(unique) != 0 && unique[len(unique)-1].id == op.id {
			unique[len(unique)-1] = op
			continue
		}
		unique = append(unique, op)
	}
	ops = unique
	if len(ops) == 0 {
		return nil
	}

	nextGeneration := st.sparseGeneration + 1
	if nextGeneration == 0 {
		return ErrOverflow
	}
	newSourceGeneration := st.sparseSourceGeneration
	if sourceGeneration > newSourceGeneration {
		newSourceGeneration = sourceGeneration
	}
	oldSegments := st.sparseSegments
	oldLive := st.sparseLive
	newLive := oldLive
	for _, op := range ops {
		current, found := sparseLookup(oldSegments, op.id)
		switch {
		case !found:
			if !op.deleted {
				newLive++
			}
		case current.deleted && !op.deleted:
			newLive++
		case !current.deleted && op.deleted:
			newLive--
		}
	}
	sync := st.config.Durability == schema.DurabilitySync
	deltaRecords := uint64(len(ops)) + sparseTotalRecords(sparseDeltaSegments(oldSegments))
	fullCompact := st.sparseStale+uint64(len(ops)) >= maxSparseStaleRecords(newLive) ||
		deltaRecords >= maxSparseDeltaRecords(newLive)
	deltaCompact := len(oldSegments)+1 >= maxSparseSegments && !fullCompact
	if fullCompact {
		segments := make([]sparseSegment, 0, len(oldSegments)+1)
		segments = append(segments, oldSegments...)
		segments = append(segments, sparseSegment{id: nextGeneration, records: ops})
		merged := mergeSparseSegments(segments, st.committed)
		liveCount := uint64(0)
		for _, rec := range merged {
			if !rec.deleted {
				liveCount++
			}
		}
		if err := st.writeSparseSegment(nextGeneration, merged, sync); err != nil {
			return err
		}
		if err := st.writeSparseManifest([]uint64{nextGeneration}, nextGeneration, newSourceGeneration, liveCount, sync); err != nil {
			return err
		}
		st.sparseSegments = []sparseSegment{{id: nextGeneration, records: merged}}
		st.sparseLive = liveCount
		st.sparseStale = 0
		st.sparseGeneration = nextGeneration
		st.sparseSourceGeneration = newSourceGeneration
		st.sparseUsed = true
		if err := sparseHook("before-publish"); err != nil {
			return err
		}
		st.sparseView.Store(makeSparseView(st.sparseSegments, st.sparseLive))
		for _, seg := range oldSegments {
			_ = os.Remove(st.layout.sparseSegmentPath(seg.id))
		}
		return nil
	}
	if deltaCompact {
		// Merge only the delta tier; the base segment is not copied.
		segments := make([]sparseSegment, 0, len(oldSegments))
		segments = append(segments, sparseDeltaSegments(oldSegments)...)
		segments = append(segments, sparseSegment{id: nextGeneration, records: ops})
		merged := mergeSparseSegments(segments, st.committed)
		liveCount := recomputeSparseLive(oldSegments[0], merged)
		if err := st.writeSparseSegment(nextGeneration, merged, sync); err != nil {
			return err
		}
		ids := []uint64{oldSegments[0].id, nextGeneration}
		if err := st.writeSparseManifest(ids, nextGeneration, newSourceGeneration, liveCount, sync); err != nil {
			return err
		}
		st.sparseSegments = []sparseSegment{oldSegments[0], {id: nextGeneration, records: merged}}
		st.sparseLive = liveCount
		st.sparseStale = sparseTotalRecords(st.sparseSegments) - liveCount - sparseTombstoneCount(st.sparseSegments)
		st.sparseGeneration = nextGeneration
		st.sparseSourceGeneration = newSourceGeneration
		st.sparseUsed = true
		if err := sparseHook("before-publish"); err != nil {
			return err
		}
		st.sparseView.Store(makeSparseView(st.sparseSegments, st.sparseLive))
		for _, seg := range oldSegments[1:] {
			_ = os.Remove(st.layout.sparseSegmentPath(seg.id))
		}
		return nil
	}

	if err := st.writeSparseSegment(nextGeneration, ops, sync); err != nil {
		return err
	}
	ids := make([]uint64, 0, len(oldSegments)+1)
	for _, seg := range oldSegments {
		ids = append(ids, seg.id)
	}
	ids = append(ids, nextGeneration)
	if err := st.writeSparseManifest(ids, nextGeneration, newSourceGeneration, newLive, sync); err != nil {
		return err
	}
	segments := make([]sparseSegment, 0, len(oldSegments)+1)
	segments = append(segments, oldSegments...)
	segments = append(segments, sparseSegment{id: nextGeneration, records: ops})
	st.sparseSegments = segments
	st.sparseLive = newLive
	st.sparseStale = sparseTotalRecords(segments) - newLive - sparseTombstoneCount(segments)
	st.sparseGeneration = nextGeneration
	st.sparseSourceGeneration = newSourceGeneration
	st.sparseUsed = true
	if err := sparseHook("before-publish"); err != nil {
		return err
	}
	st.sparseView.Store(makeSparseView(segments, newLive))
	return nil
}

func maxSparseDeltaRecords(live uint64) uint64 {
	if live < 16_384 {
		return 4_096
	}
	return live / 4
}

func recomputeSparseLive(base sparseSegment, overlay []sparseRecord) uint64 {
	var live uint64
	i, j := 0, 0
	for i < len(base.records) || j < len(overlay) {
		switch {
		case j >= len(overlay) || (i < len(base.records) && base.records[i].id < overlay[j].id):
			if !base.records[i].deleted {
				live++
			}
			i++
		case i >= len(base.records) || overlay[j].id < base.records[i].id:
			if !overlay[j].deleted {
				live++
			}
			j++
		default:
			if !overlay[j].deleted {
				live++
			}
			i++
			j++
		}
	}
	return live
}

func maxSparseStaleRecords(live uint64) uint64 {
	if live < 64 {
		return 64
	}
	return live
}

func (st *Store) syncSparseLocked() error {
	for _, seg := range st.sparseSegments {
		f, err := os.Open(st.layout.sparseSegmentPath(seg.id))
		if err != nil {
			return err
		}
		syncErr := f.Sync()
		closeErr := f.Close()
		if syncErr != nil {
			return syncErr
		}
		if closeErr != nil {
			return closeErr
		}
	}
	if len(st.sparseSegments) != 0 {
		f, err := os.Open(st.layout.sparseManifestPath())
		if err != nil {
			return err
		}
		syncErr := f.Sync()
		closeErr := f.Close()
		if syncErr != nil {
			return syncErr
		}
		if closeErr != nil {
			return closeErr
		}
	}
	return syncDirectory(st.layout.dir)
}

func sparseSearchPHash(view *sparseView, dst []Result, query uint64, maxDistance uint8) []Result {
	if view == nil {
		return dst
	}
	base, overlay := view.base.records, view.overlay
	i, j := 0, 0
	for i < len(base) || j < len(overlay) {
		var rec sparseRecord
		switch {
		case j >= len(overlay) || (i < len(base) && base[i].id < overlay[j].id):
			rec = base[i]
			i++
		case i >= len(base) || overlay[j].id < base[i].id:
			rec = overlay[j]
			j++
		default:
			rec = overlay[j]
			i++
			j++
		}
		if rec.deleted {
			continue
		}
		if distance := DistancePHash(rec.record.PHash, query); distance <= maxDistance {
			dst = append(dst, Result{ImageID: rec.id, Distance: distance})
		}
	}
	return dst
}

func sparseSearchPDQ(view *sparseView, dst []Result, query *PDQHash, maxDistance uint8) []Result {
	if view == nil {
		return dst
	}
	base, overlay := view.base.records, view.overlay
	i, j := 0, 0
	for i < len(base) || j < len(overlay) {
		var rec sparseRecord
		switch {
		case j >= len(overlay) || (i < len(base) && base[i].id < overlay[j].id):
			rec = base[i]
			i++
		case i >= len(base) || overlay[j].id < base[i].id:
			rec = overlay[j]
			j++
		default:
			rec = overlay[j]
			i++
			j++
		}
		if rec.deleted {
			continue
		}
		if distance := DistancePDQ(&rec.record.PDQ, query); distance <= uint16(maxDistance) {
			dst = append(dst, Result{ImageID: rec.id, Distance: uint8(distance)})
		}
	}
	return dst
}
