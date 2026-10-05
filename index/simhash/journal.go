package simhash

import (
	"encoding/binary"

	"github.com/evanoberholster/imageIdx/internal/storage"
)

// The update journal is a redo log for in-place point updates. It reuses the
// shared framed WAL transport from internal/storage.
//
// Protocol (writer serialized by the store mutex):
//
//  1. append a journal record: ImageID + complete replacement tuple + CRC
//  2. fsync the journal when durability mode requires it
//  3. seqlock odd
//  4. update the canonical columns in place
//  5. seqlock even
//  6. advance/publish the generation
//  7. Checkpoint/ Sync makes the canonical columns and manifest durable
//  8. only then truncate the journal
//
// Recovery replays every valid record in order, rewriting the canonical tuple
// (idempotent). A record whose CRC fails, or a truncated frame, is corruption
// or an interrupted tail respectively; the tail is repaired away. Because the
// record is written before the canonical apply, a crash before/during apply is
// completed by replay; a crash before the record is written leaves the previous
// canonical state. This makes a permanently mixed pHash/PDQ/quality tuple
// impossible for any update whose journal record survived. Under relaxed
// (OS) durability a power loss can still lose both the record and the canonical
// bytes; DurabilitySync closes that window by fsyncing the record before apply.
const (
	journalMagic        uint32 = 0x534A524E // "SJRN"
	journalVersion      uint16 = 1
	journalHeaderBytes         = 16
	journalPayloadBytes        = 8 + 8 + 32 + 1 + 4 // id, pHash, PDQ, quality, crc32c
	journalCRCOffset           = 8 + 8 + 32 + 1
	journalFilename            = "updates.wal"
)

type journalRecord struct {
	imageID uint64
	phash   uint64
	pdq     PDQHash
	quality uint8
}

type journal struct {
	wal *storage.WAL
}

func openJournal(path string) (*journal, error) {
	w, err := storage.OpenWAL(path, journalHeaderBytes+journalPayloadBytes)
	if err != nil {
		return nil, err
	}
	return &journal{wal: w}, nil
}

func (j *journal) close() error {
	if j == nil || j.wal == nil {
		return nil
	}
	return j.wal.Close()
}

// reset truncates the journal to empty. The caller must have made the canonical
// columns durable first.
func (j *journal) reset() error { return j.wal.RepairTail(0) }

func (j *journal) appendRecord(r journalRecord, sync bool) error {
	var header [journalHeaderBytes]byte
	binary.LittleEndian.PutUint32(header[0:4], journalMagic)
	binary.LittleEndian.PutUint16(header[4:6], journalVersion)
	binary.LittleEndian.PutUint32(header[8:12], journalPayloadBytes)
	payload := j.wal.Scratch(journalPayloadBytes)
	binary.LittleEndian.PutUint64(payload[0:8], r.imageID)
	binary.LittleEndian.PutUint64(payload[8:16], r.phash)
	copy(payload[16:48], r.pdq[:])
	payload[48] = r.quality
	binary.LittleEndian.PutUint32(payload[journalCRCOffset:journalPayloadBytes], storage.Checksum(payload[:journalCRCOffset]))
	return j.wal.AppendFrame(header[:], payload, sync)
}

func decodeJournalPayload(src []byte) (journalRecord, bool) {
	if len(src) < journalPayloadBytes {
		return journalRecord{}, false
	}
	if binary.LittleEndian.Uint32(src[journalCRCOffset:journalPayloadBytes]) != storage.Checksum(src[:journalCRCOffset]) {
		return journalRecord{}, false
	}
	var r journalRecord
	r.imageID = binary.LittleEndian.Uint64(src[0:8])
	r.phash = binary.LittleEndian.Uint64(src[8:16])
	copy(r.pdq[:], src[16:48])
	r.quality = src[48]
	return r, true
}

func journalPayloadLen(header []byte) (uint64, error) {
	if len(header) < journalHeaderBytes {
		return 0, ErrCorrupt
	}
	return uint64(binary.LittleEndian.Uint32(header[8:12])), nil
}

// replayJournal visits every complete, well-formed record in order and returns
// the exclusive end offset of the last complete frame.
func replayJournal(path string, visit func(journalRecord) error) (int64, error) {
	return storage.ReplayEnd(path, 0, storage.FrameSpec{
		HeaderSize: journalHeaderBytes,
		Magic:      journalMagic,
		Version:    journalVersion,
		Corrupt:    ErrCorrupt,
		MaxPayload: journalPayloadBytes,
	}, journalPayloadLen, func(header, payload []byte) error {
		r, ok := decodeJournalPayload(payload)
		if !ok {
			return ErrCorrupt
		}
		return visit(r)
	})
}
