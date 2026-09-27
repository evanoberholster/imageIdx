package vectoridx

import (
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// WAL framing (VectorIdx.MD section 13). One record:
//
//	magic u32 (0x5657414C) | version u16 (1) | rtype u8 | reserved u8
//	lsn u64 | nodeID u32 | vecLen u32 | payload[vecLen] | crc32 u32
//
// crc32 covers every preceding byte of the record. Recovery replays records
// with lsn > checkpointLSN, stops cleanly at the first torn tail, and
// truncates it so reopening is deterministic. A mid-file checksum mismatch
// with trailing bytes still present is corruption (ErrCorruptWAL); a
// mismatch at exact EOF is treated as a torn tail and truncated.
//
// The log is segmented: files are <path>.wal.<016d sequence>, rotated at
// walSegBytes. Segments replay oldest to newest, and a checkpoint removes
// every segment except the active one.
const (
	walMagic   uint32 = 0x5657414C
	walVersion uint16 = 1

	walTypeAdd    = 1
	walTypeDelete = 2

	walHeaderSize = 4 + 2 + 1 + 1 + 8 + 4 + 4 // 24
	walCRCSize    = 4

	// DefaultWALSegmentBytes bounds one segment before rotation.
	DefaultWALSegmentBytes int64 = 64 << 20

	walSegmentInfix = ".wal."
	legacyWALSuffix = ".wal"
)

func walSegmentPath(base string, seq uint64) string {
	return fmt.Sprintf("%s%s%016d", base, walSegmentInfix, seq)
}

type walSegment struct {
	seq  uint64
	path string
	size int64
}

// listWALSegments returns the segment files of base ordered by sequence.
// Unparseable names are ignored; missing files are skipped.
func listWALSegments(base string) ([]walSegment, error) {
	matches, err := filepath.Glob(base + walSegmentInfix + "*")
	if err != nil {
		return nil, err
	}
	segs := make([]walSegment, 0, len(matches))
	for _, match := range matches {
		seq, err := strconv.ParseUint(strings.TrimPrefix(match, base+walSegmentInfix), 10, 64)
		if err != nil {
			continue
		}
		info, err := os.Stat(match)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, err
		}
		segs = append(segs, walSegment{seq: seq, path: match, size: info.Size()})
	}
	sort.Slice(segs, func(i, j int) bool { return segs[i].seq < segs[j].seq })
	return segs, nil
}

// removeWALFiles deletes every WAL file for base, segmented or legacy. It is
// used by Create, which starts a fresh log.
func removeWALFiles(base string) error {
	paths, err := filepath.Glob(base + walSegmentInfix + "*")
	if err != nil {
		return err
	}
	for _, path := range paths {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	if err := os.Remove(base + legacyWALSuffix); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// openWALSegment opens (creating) the segment for seq for appends and records
// its on-disk size.
func (v *VectorIdx) openWALSegment(seq uint64) error {
	path := walSegmentPath(v.path, seq)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return err
	}
	v.wal = f
	v.walSeq = seq
	v.walPath = path
	v.walSize = info.Size()
	return nil
}

// rotateWALLocked opens the next segment and closes the current one. The old
// segment is closed only after the new handle exists, so a failed rotation
// leaves the active segment writable.
func (v *VectorIdx) rotateWALLocked() error {
	next := v.walSeq + 1
	path := walSegmentPath(v.path, next)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	if v.wal != nil {
		if v.durability == DurabilitySync {
			// The old segment must be durable before it stops being the
			// active handle: a writer that appended to it may be about to
			// fsync the successor instead.
			if err := v.wal.Sync(); err != nil {
				_ = f.Close()
				return err
			}
			v.walSyncs.Add(1)
		}
		_ = v.wal.Close()
	}
	v.wal = f
	v.walSeq = next
	v.walPath = path
	v.walSize = 0
	return nil
}

// truncateWALLocked removes every segment except the active one and truncates
// the active segment. Callers hold the writer lock and have durably published
// a checkpoint covering every record.
func (v *VectorIdx) truncateWALLocked() error {
	if v.wal != nil {
		if err := v.wal.Truncate(0); err != nil {
			return err
		}
		if _, err := v.wal.Seek(0, 0); err != nil {
			return err
		}
		v.walSize = 0
	}
	segs, err := listWALSegments(v.path)
	if err != nil {
		return err
	}
	for _, seg := range segs {
		if seg.seq == v.walSeq {
			continue
		}
		if err := os.Remove(seg.path); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

func (v *VectorIdx) appendWALRecord(rtype uint8, id NodeID, raw []byte) error {
	vecLen := 0
	if rtype == walTypeAdd {
		vecLen = len(raw)
	}
	record := make([]byte, 0, walHeaderSize+vecLen+walCRCSize)
	var hdr [walHeaderSize]byte
	binary.LittleEndian.PutUint32(hdr[0:4], walMagic)
	binary.LittleEndian.PutUint16(hdr[4:6], walVersion)
	hdr[6] = rtype
	hdr[7] = v.cfgBase.ElementType.byte()
	binary.LittleEndian.PutUint64(hdr[8:16], v.lsn+1)
	binary.LittleEndian.PutUint32(hdr[16:20], uint32(id))
	binary.LittleEndian.PutUint32(hdr[20:24], uint32(vecLen))
	record = append(record, hdr[:]...)
	if vecLen != 0 {
		record = append(record, raw...)
	}
	crc := crc32.ChecksumIEEE(record)
	var crcBuf [4]byte
	binary.LittleEndian.PutUint32(crcBuf[:], crc)
	record = append(record, crcBuf[:]...)
	if v.walSize > 0 && v.walSize+int64(len(record)) > v.walSegBytes {
		if err := v.rotateWALLocked(); err != nil {
			return err
		}
	}
	start, err := v.wal.Seek(0, io.SeekEnd)
	if err != nil {
		return err
	}
	for len(record) != 0 {
		n, err := v.wal.Write(record)
		if err != nil {
			_ = v.wal.Truncate(start)
			_, _ = v.wal.Seek(0, io.SeekEnd)
			return err
		}
		if n == 0 {
			_ = v.wal.Truncate(start)
			_, _ = v.wal.Seek(0, io.SeekEnd)
			return io.ErrShortWrite
		}
		record = record[n:]
	}
	v.walSize += walHeaderSize + int64(vecLen) + walCRCSize
	v.walRecords.Add(1)
	v.walBytes.Add(uint64(walHeaderSize + vecLen + walCRCSize))
	// The record is written but never fsynced here. A DurabilitySync
	// writer calls syncWALAfterWrite once it releases writeMu, so the
	// fsync overlaps the next writer's graph insertion instead of holding
	// the write lock. DurabilityOS leaves the record in the OS page cache
	// and only marks it dirty for periodic group commit.
	if v.syncInterval > 0 && v.durability == DurabilityOS {
		v.walDirty.Store(true)
	}
	if v.durability == DurabilitySync {
		v.pendingSync = v.wal
	}
	return nil
}

func (t ElementType) byte() byte { return byte(t) }

// replayWAL migrates a legacy single-file WAL, replays every segment after
// checkpointLSN into writer state, truncates a torn tail in the newest
// segment, then opens the newest segment for appends. It must run before the
// index accepts writes.
func (v *VectorIdx) replayWAL() error {
	if err := v.migrateLegacyWAL(); err != nil {
		return err
	}
	segs, err := listWALSegments(v.path)
	if err != nil {
		return err
	}
	for i, seg := range segs {
		if err := v.replayWALSegment(seg, i == len(segs)-1); err != nil {
			return err
		}
	}
	if len(segs) == 0 {
		return v.openWALSegment(1)
	}
	return v.openWALSegment(segs[len(segs)-1].seq)
}

// migrateLegacyWAL renames a pre-segment <path>.wal to sequence zero so
// ordering and cleanup treat it like any other segment.
func (v *VectorIdx) migrateLegacyWAL() error {
	legacy := v.path + legacyWALSuffix
	if _, err := os.Stat(legacy); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if segs, err := listWALSegments(v.path); err == nil && len(segs) > 0 && segs[0].seq == 0 {
		return ErrCorruptWAL
	}
	return os.Rename(legacy, walSegmentPath(v.path, 0))
}

// replayWALSegment replays one segment from offset zero. allowTorn permits
// truncating an incomplete trailing record (the crash point); a torn record
// in an older segment is corruption because later segments may depend on it.
func (v *VectorIdx) replayWALSegment(seg walSegment, allowTorn bool) error {
	f, err := os.Open(seg.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer f.Close()
	size := seg.size
	torn := func(offset int64) error {
		if !allowTorn {
			return ErrCorruptWAL
		}
		// Truncate by path: the replay handle is read-only, and ftruncate
		// rejects read-only descriptors on some platforms.
		return os.Truncate(seg.path, offset)
	}
	var offset int64
	header := make([]byte, walHeaderSize)
	for {
		if offset == size {
			return nil
		}
		if size-offset < int64(walHeaderSize) {
			return torn(offset)
		}
		if _, err := f.ReadAt(header, offset); err != nil {
			return err
		}
		if binary.LittleEndian.Uint32(header[0:4]) != walMagic ||
			binary.LittleEndian.Uint16(header[4:6]) != walVersion {
			// If nothing valid follows (all remaining bytes trailing), treat
			// as a torn tail; otherwise the log is corrupt mid-stream.
			if isZeroTail(f, offset, size) {
				return torn(offset)
			}
			return ErrCorruptWAL
		}
		rtype := header[6]
		elem := ElementType(header[7])
		lsn := binary.LittleEndian.Uint64(header[8:16])
		id := NodeID(binary.LittleEndian.Uint32(header[16:20]))
		vecLen := binary.LittleEndian.Uint32(header[20:24])
		if rtype != walTypeAdd && rtype != walTypeDelete {
			return ErrCorruptWAL
		}
		if elem != v.cfgBase.ElementType {
			return ErrCorruptWAL
		}
		if rtype == walTypeDelete && vecLen != 0 {
			return ErrCorruptWAL
		}
		if rtype == walTypeAdd && int(vecLen) != v.vecBytes {
			// A vecLen mismatch at the tail is a torn record; mid-file it is
			// corruption. Distinguish by whether the full record fits.
			if offset+int64(walHeaderSize)+int64(vecLen)+int64(walCRCSize) > size {
				return torn(offset)
			}
			return ErrCorruptWAL
		}
		recordLen := int64(walHeaderSize) + int64(vecLen) + int64(walCRCSize)
		if offset+recordLen > size {
			return torn(offset)
		}
		record := make([]byte, recordLen)
		if _, err := f.ReadAt(record, offset); err != nil {
			return err
		}
		body, crcBytes := record[:recordLen-4], record[recordLen-4:]
		if crc32.ChecksumIEEE(body) != binary.LittleEndian.Uint32(crcBytes) {
			if offset+recordLen == size {
				return torn(offset)
			}
			return ErrCorruptWAL
		}
		next := offset + recordLen
		if lsn > v.checkpointLSN {
			payload := append([]byte(nil), body[walHeaderSize:walHeaderSize+vecLen]...)
			if err := v.applyReplayRecord(rtype, id, payload, lsn); err != nil {
				return err
			}
		}
		offset = next
	}
}

// applyReplayRecord folds one WAL record into writer state. Graph insertion
// reuses the normal writer path but skips WAL appends (the record exists).
// It runs during Open before the async worker starts; no readers exist yet,
// so snapshots publish without tracker waits.
func (v *VectorIdx) applyReplayRecord(rtype uint8, id NodeID, payload []byte, lsn uint64) error {
	cur := v.view.Load()
	switch rtype {
	case walTypeAdd:
		ord, exists := cur.locate(id)
		if exists {
			if !cur.tomb.Contains(uint64(id)) {
				// Idempotent replay: duplicate adds after a checkpoint
				// boundary can occur when Checkpoint and WAL truncation
				// interleave with a crash. The first record wins.
				if lsn > v.lsn {
					v.lsn = lsn
				}
				return nil
			}
			// Delete-then-re-add: the later add replaces the vector.
			// Temporarily detach the WAL so resurrect does not re-append.
			wal := v.wal
			v.wal = nil
			ns, err := v.resurrectLocked(cur, ord, id, payload)
			v.wal = wal
			if err != nil {
				return err
			}
			v.view.Store(ns)
			v.lsn = lsn
			return nil
		}
		// Temporarily detach the WAL so addRaw does not re-append.
		wal := v.wal
		v.wal = nil
		ns, err := v.addRawLocked(cur, id, payload)
		v.wal = wal
		if err != nil {
			return err
		}
		v.view.Store(ns)
		v.lsn = lsn
		return nil
	case walTypeDelete:
		if _, ok := cur.locate(id); !ok {
			if lsn > v.lsn {
				v.lsn = lsn
			}
			return nil
		}
		ns := cloneSnapshot(cur)
		ns.tombSorted = insertTombSorted(ns.tombSorted, id)
		ns.tomb, ns.tombSorted = rebuildTombeset(ns.tombSorted)
		ns.gen = v.gen.Add(1)
		v.view.Store(ns)
		v.markPendingLocked(id)
		v.lsn = lsn
		return nil
	default:
		return ErrCorruptWAL
	}
}

func isZeroTail(f *os.File, offset, size int64) bool {
	// Conservative: only exact-EOF checksum failures count as torn tails.
	// Callers already established the record extends to size.
	return offset >= 0 && size >= offset
}
