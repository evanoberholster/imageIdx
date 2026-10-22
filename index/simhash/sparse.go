package simhash

import (
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"os"
	"sort"

	schema "github.com/evanoberholster/imageIdx/schema"
)

const (
	sparseMagic       uint32 = 0x53535052 // "SSPR"
	sparseVersion     uint16 = 1
	sparseHeaderBytes        = 8
	sparseFrameBytes         = 8 + 8 + 32 + 1 + 1 + 4
)

// sparseRecord is a live external-ID record kept outside the dense hash-page
// columns. Sparse records are sorted by external ImageID.
type sparseRecord struct {
	id     schema.ImageID
	record Record
}

func openSparse(l layout) ([]sparseRecord, int64, *os.File, error) {
	f, err := os.OpenFile(l.sparsePath(), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, 0, nil, err
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, 0, nil, err
	}
	if info.Size() == 0 {
		header := make([]byte, sparseHeaderBytes)
		binary.LittleEndian.PutUint32(header[0:4], sparseMagic)
		binary.LittleEndian.PutUint16(header[4:6], sparseVersion)
		if err := writeAt(f, header, 0); err != nil {
			_ = f.Close()
			return nil, 0, nil, err
		}
		return nil, sparseHeaderBytes, f, nil
	}
	if info.Size() < sparseHeaderBytes {
		_ = f.Close()
		return nil, 0, nil, fmt.Errorf("%w: short sparse header", ErrCorrupt)
	}
	header := make([]byte, sparseHeaderBytes)
	if _, err := f.ReadAt(header, 0); err != nil {
		_ = f.Close()
		return nil, 0, nil, err
	}
	if binary.LittleEndian.Uint32(header[0:4]) != sparseMagic || binary.LittleEndian.Uint16(header[4:6]) != sparseVersion {
		_ = f.Close()
		return nil, 0, nil, fmt.Errorf("%w: sparse header", ErrCorrupt)
	}
	latest := make(map[schema.ImageID]Record)
	offset := int64(sparseHeaderBytes)
	frame := make([]byte, sparseFrameBytes)
	for offset+sparseFrameBytes <= info.Size() {
		if _, err := f.ReadAt(frame, offset); err != nil {
			_ = f.Close()
			return nil, 0, nil, err
		}
		body := frame[:sparseFrameBytes-4]
		if binary.LittleEndian.Uint32(frame[sparseFrameBytes-4:]) != crc32.ChecksumIEEE(body) {
			_ = f.Close()
			return nil, 0, nil, fmt.Errorf("%w: sparse crc", ErrCorrupt)
		}
		id := binary.LittleEndian.Uint64(body[0:8])
		flags := body[49]
		if id == 0 || flags > 1 {
			_ = f.Close()
			return nil, 0, nil, fmt.Errorf("%w: sparse record", ErrCorrupt)
		}
		if flags == 1 {
			delete(latest, schema.ImageID(id))
		} else {
			var pdq PDQHash
			copy(pdq[:], body[16:48])
			latest[schema.ImageID(id)] = Record{
				PHash:   binary.LittleEndian.Uint64(body[8:16]),
				PDQ:     pdq,
				Quality: body[48],
			}
		}
		offset += sparseFrameBytes
	}
	if offset < info.Size() {
		// A crash can leave a partial last frame. It was never acknowledged as a
		// durable sparse commit; truncate it before future appends.
		if err := f.Truncate(offset); err != nil {
			_ = f.Close()
			return nil, 0, nil, err
		}
	}
	records := make([]sparseRecord, 0, len(latest))
	for id, rec := range latest {
		records = append(records, sparseRecord{id: id, record: rec})
	}
	sort.Slice(records, func(i, j int) bool { return records[i].id < records[j].id })
	return records, offset, f, nil
}

func writeAt(f *os.File, data []byte, offset int64) error {
	for len(data) != 0 {
		n, err := f.WriteAt(data, offset)
		if err != nil {
			return err
		}
		if n == 0 {
			return fmt.Errorf("%w: short sparse write", ErrCorrupt)
		}
		data = data[n:]
		offset += int64(n)
	}
	return nil
}

// ApplyUpdates accepts arbitrary nonzero external ImageIDs. Existing dense
// records are updated through the dense journal path until a sparse record is
// introduced; after that, all updates use the sparse overlay. This preserves
// the dense append fast path for legacy image IDs while removing the Len()+1
// constraint.
func (st *Store) ApplyUpdates(updates []Update) error {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.closed.Load() {
		return ErrClosed
	}
	if len(updates) == 0 {
		return nil
	}
	if len(updates) > maxBatchRecords {
		return ErrBatchTooLarge
	}
	for i := range updates {
		if updates[i].ImageID == 0 {
			return ErrInvalidID
		}
	}
	if !st.sparseUsed {
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
	return st.appendSparseLocked(updates)
}

func (st *Store) appendSparseLocked(updates []Update) error {
	oldOffset := st.sparseOff
	frames := make([]byte, 0, len(updates)*sparseFrameBytes)
	for _, update := range updates {
		if update.ImageID == 0 {
			return ErrInvalidID
		}
		start := len(frames)
		frames = append(frames, make([]byte, sparseFrameBytes)...)
		frame := frames[start:]
		binary.LittleEndian.PutUint64(frame[0:8], uint64(update.ImageID))
		binary.LittleEndian.PutUint64(frame[8:16], update.Record.PHash)
		copy(frame[16:48], update.Record.PDQ[:])
		frame[48] = update.Record.Quality
		frame[49] = 0
		binary.LittleEndian.PutUint32(frame[50:54], crc32.ChecksumIEEE(frame[:50]))
	}
	if err := writeAt(st.sparseFile, frames, oldOffset); err != nil {
		_ = st.sparseFile.Truncate(oldOffset)
		return err
	}
	if st.config.Durability == schema.DurabilitySync {
		if err := st.sparseFile.Sync(); err != nil {
			_ = st.sparseFile.Truncate(oldOffset)
			return err
		}
	}
	st.sparseOff = oldOffset + int64(len(frames))
	next := append([]sparseRecord(nil), st.sparseRecords...)
	for _, update := range updates {
		index := sparseIndex(next, update.ImageID)
		if index < len(next) && next[index].id == update.ImageID {
			next[index].record = update.Record
			continue
		}
		next = append(next, sparseRecord{})
		copy(next[index+1:], next[index:])
		next[index] = sparseRecord{id: update.ImageID, record: update.Record}
	}
	st.sparseRecords = next
	st.sparseView.Store(&next)
	st.sparseUsed = true
	return nil
}

func (st *Store) sparseSnapshot() []sparseRecord {
	p := st.sparseView.Load()
	if p == nil {
		return nil
	}
	return *p
}

func (st *Store) sparseGet(id schema.ImageID) (Record, bool) {
	records := st.sparseSnapshot()
	index := sparseIndex(records, id)
	if index < len(records) && records[index].id == id {
		return records[index].record, true
	}
	return Record{}, false
}

func sparseHasID(records []sparseRecord, id schema.ImageID) bool {
	index := sparseIndex(records, id)
	return index < len(records) && records[index].id == id
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
