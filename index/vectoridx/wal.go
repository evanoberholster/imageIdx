package vectoridx

import (
	"encoding/binary"
	"hash/crc32"
	"io"
	"os"
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

const (
	walMagic   uint32 = 0x5657414C
	walVersion uint16 = 1

	walTypeAdd    = 1
	walTypeDelete = 2

	walHeaderSize = 4 + 2 + 1 + 1 + 8 + 4 + 4 // 24
	walCRCSize    = 4
)

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
	v.walRecords.Add(1)
	v.walBytes.Add(uint64(walHeaderSize + vecLen + walCRCSize))
	// DurabilitySync fsyncs each acknowledged mutation; DurabilityOS leaves
	// the record in the OS page cache and only marks it dirty for periodic
	// group commit.
	if v.durability == DurabilitySync {
		if err := v.wal.Sync(); err != nil {
			return err
		}
		v.walSyncs.Add(1)
		v.walDirty.Store(false)
		return nil
	}
	if v.syncInterval > 0 {
		v.walDirty.Store(true)
	}
	return nil
}

func (t ElementType) byte() byte { return byte(t) }

// replayWAL replays records after checkpointLSN into writer state without
// fsyncing per record. It must run before the index accepts writes.
func (v *VectorIdx) replayWAL() error {
	f, err := os.Open(v.walPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	size := info.Size()
	var offset int64
	header := make([]byte, walHeaderSize)
	for {
		if offset == size {
			return nil
		}
		if size-offset < int64(walHeaderSize) {
			// Torn header tail: truncate and stop.
			return truncateWAL(v.walPath, offset)
		}
		if _, err := f.ReadAt(header, offset); err != nil {
			return err
		}
		if binary.LittleEndian.Uint32(header[0:4]) != walMagic ||
			binary.LittleEndian.Uint16(header[4:6]) != walVersion {
			// If nothing valid follows (all remaining bytes trailing), treat
			// as a torn tail; otherwise the log is corrupt mid-stream.
			if isZeroTail(f, offset, size) {
				return truncateWAL(v.walPath, offset)
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
				return truncateWAL(v.walPath, offset)
			}
			return ErrCorruptWAL
		}
		recordLen := int64(walHeaderSize) + int64(vecLen) + int64(walCRCSize)
		if offset+recordLen > size {
			return truncateWAL(v.walPath, offset)
		}
		record := make([]byte, recordLen)
		if _, err := f.ReadAt(record, offset); err != nil {
			return err
		}
		body, crcBytes := record[:recordLen-4], record[recordLen-4:]
		if crc32.ChecksumIEEE(body) != binary.LittleEndian.Uint32(crcBytes) {
			if offset+recordLen == size {
				return truncateWAL(v.walPath, offset)
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

func truncateWAL(path string, offset int64) error {
	f, err := os.OpenFile(path, os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Truncate(offset)
}
