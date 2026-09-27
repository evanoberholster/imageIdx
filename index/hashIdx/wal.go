package hashidx

import (
	"encoding/binary"
	"hash/crc32"

	common "github.com/evanoberholster/imageIdx/common"
	"github.com/evanoberholster/imageIdx/internal/storage"
)

const (
	walMagic      uint32 = 0x4841574c // HAWL
	walVersion    uint16 = 1
	walHeaderSize        = 32
	walEntrySize         = 48 // ImageID8 + PHash64 8 + PDQ 32

	// maxWALFrameBytes bounds one framed WAL record. Past it the append
	// fails cleanly with ErrBatchTooLarge instead of growing scratch
	// toward an out-of-memory panic. It comfortably exceeds the
	// configured batch-entry caps below.
	maxWALFrameBytes = 64 << 20
)

// openHashWAL opens the framed append-batch WAL. Header layout
// (little-endian):
//
//	0:4  magic
//	4:6  version
//	8:12 record count
//	12:16 payload length (count * 48)
//	24:28 CRC32/IEEE of payload
func openHashWAL(path string) (*storage.WAL, error) {
	return storage.OpenWAL(path, walHeaderSize+4096*walEntrySize)
}

// appendHashWAL frames one Apply batch so it is atomically replayable.
// Group commits append every frame unsynced and fsync once for the whole
// group, so sync only requests the fsync; the frame always appends.
func appendHashWAL(w *storage.WAL, entries []Entry, sync bool) error {
	if len(entries) == 0 {
		return nil
	}
	if uint64(len(entries))*walEntrySize > maxWALFrameBytes {
		return ErrBatchTooLarge
	}
	record := w.Scratch(walHeaderSize + len(entries)*walEntrySize)
	clear(record[:walHeaderSize])
	binary.LittleEndian.PutUint32(record[0:4], walMagic)
	binary.LittleEndian.PutUint16(record[4:6], walVersion)
	binary.LittleEndian.PutUint32(record[8:12], uint32(len(entries)))
	binary.LittleEndian.PutUint32(record[12:16], uint32(len(entries)*walEntrySize))
	offset := walHeaderSize
	for k := range entries {
		entry := &entries[k]
		binary.LittleEndian.PutUint64(record[offset:offset+8], uint64(entry.ImageID))
		binary.LittleEndian.PutUint64(record[offset+8:offset+16], entry.PHash64)
		for word := 0; word < 4; word++ {
			binary.LittleEndian.PutUint64(record[offset+16+word*8:offset+24+word*8], entry.PDQ[word])
		}
		offset += walEntrySize
	}
	binary.LittleEndian.PutUint32(record[24:28], crc32.ChecksumIEEE(record[walHeaderSize:]))
	return w.WriteRecord(record, sync)
}

// replayHashWAL returns every complete valid batch after offset, flattened in
// append order. A missing file and a truncated trailing frame are clean stops;
// structural corruption returns ErrCorruptWAL.
func replayHashWAL(path string, offset int64) ([]Entry, error) {
	var entries []Entry
	err := storage.Replay(path, offset, storage.FrameSpec{
		HeaderSize: walHeaderSize,
		Magic:      walMagic,
		Version:    walVersion,
		Corrupt:    ErrCorruptWAL,
	}, func(header []byte) (uint64, error) {
		count := uint64(binary.LittleEndian.Uint32(header[8:12]))
		payloadLength := uint64(binary.LittleEndian.Uint32(header[12:16]))
		if count == 0 || count*walEntrySize != payloadLength {
			return 0, ErrCorruptWAL
		}
		return payloadLength, nil
	}, func(header, payload []byte) error {
		if crc32.ChecksumIEEE(payload) != binary.LittleEndian.Uint32(header[24:28]) {
			return ErrCorruptWAL
		}
		count := int(binary.LittleEndian.Uint32(header[8:12]))
		for k := 0; k < count; k++ {
			offset := k * walEntrySize
			var entry Entry
			entry.ImageID = common.ImageID(binary.LittleEndian.Uint64(payload[offset : offset+8]))
			entry.PHash64 = binary.LittleEndian.Uint64(payload[offset+8 : offset+16])
			for word := 0; word < 4; word++ {
				entry.PDQ[word] = binary.LittleEndian.Uint64(payload[offset+16+word*8 : offset+24+word*8])
			}
			if entry.ImageID == 0 {
				return ErrCorruptWAL
			}
			entries = append(entries, entry)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return entries, nil
}

const (
	exactWALMagic      uint32 = 0x4849574c // HIWL
	exactWALVersion    uint16 = 1
	exactWALHeaderSize        = 32
	exactWALOpSize            = 41 // Hash32 + ImageID8 + Op1

	exactWALOpAdd    uint8 = 1
	exactWALOpRemove uint8 = 2
)

type walRecord struct {
	hash    Hash
	imageID common.ImageID
	op      uint8
}

func openExactWAL(path string, maxBatch int) (*storage.WAL, error) {
	if maxBatch < 1 {
		maxBatch = 1
	}
	return storage.OpenWAL(path, exactWALHeaderSize+maxBatch*exactWALOpSize)
}

// appendExactWAL frames one add/remove batch with its sequence number.
func appendExactWAL(w *storage.WAL, adds, removes []ExactEntry, durability common.Durability, seq uint64) error {
	count := len(adds) + len(removes)
	if count == 0 {
		return nil
	}
	if uint64(count)*exactWALOpSize > maxWALFrameBytes {
		return ErrBatchTooLarge
	}
	required := exactWALHeaderSize + count*exactWALOpSize
	record := w.Scratch(required)
	clear(record)
	binary.LittleEndian.PutUint32(record[0:4], exactWALMagic)
	binary.LittleEndian.PutUint16(record[4:6], exactWALVersion)
	binary.LittleEndian.PutUint32(record[8:12], uint32(count))
	binary.LittleEndian.PutUint32(record[12:16], uint32(count*exactWALOpSize))
	binary.LittleEndian.PutUint64(record[16:24], seq)
	off := exactWALHeaderSize
	for _, e := range adds {
		copy(record[off:off+32], e.Hash[:])
		binary.LittleEndian.PutUint64(record[off+32:off+40], uint64(e.ImageID))
		record[off+40] = exactWALOpAdd
		off += exactWALOpSize
	}
	for _, e := range removes {
		copy(record[off:off+32], e.Hash[:])
		binary.LittleEndian.PutUint64(record[off+32:off+40], uint64(e.ImageID))
		record[off+40] = exactWALOpRemove
		off += exactWALOpSize
	}
	binary.LittleEndian.PutUint32(record[24:28], crc32.ChecksumIEEE(record[exactWALHeaderSize:]))
	return w.WriteRecord(record, durability == common.DurabilitySync)
}

// replayExactWAL replays batches from offset, returning per-batch records and
// the maximum sequence observed. A missing file and truncated tails are clean
// stops; structural corruption is ErrCorruptWAL.
func replayExactWAL(path string, offset int64) ([][]walRecord, uint64, error) {
	var batches [][]walRecord
	var maxSeq uint64
	err := storage.Replay(path, offset, storage.FrameSpec{
		HeaderSize: exactWALHeaderSize, Magic: exactWALMagic, Version: exactWALVersion, Corrupt: ErrCorruptWAL,
	}, func(header []byte) (uint64, error) {
		count := int(binary.LittleEndian.Uint32(header[8:12]))
		payloadSize := int(binary.LittleEndian.Uint32(header[12:16]))
		if count < 0 || payloadSize != count*exactWALOpSize {
			return 0, ErrCorruptWAL
		}
		seq := binary.LittleEndian.Uint64(header[16:24])
		if seq > maxSeq {
			maxSeq = seq
		}
		return uint64(payloadSize), nil
	}, func(header, payload []byte) error {
		if crc32.ChecksumIEEE(payload) != binary.LittleEndian.Uint32(header[24:28]) {
			return ErrCorruptWAL
		}
		count := int(binary.LittleEndian.Uint32(header[8:12]))
		batch := make([]walRecord, count)
		for i := 0; i < count; i++ {
			pos := i * exactWALOpSize
			var rec walRecord
			copy(rec.hash[:], payload[pos:pos+32])
			rec.imageID = common.ImageID(binary.LittleEndian.Uint64(payload[pos+32 : pos+40]))
			rec.op = payload[pos+40]
			if rec.op != exactWALOpAdd && rec.op != exactWALOpRemove {
				return ErrCorruptWAL
			}
			if rec.imageID == 0 {
				return ErrCorruptWAL
			}
			batch[i] = rec
		}
		batches = append(batches, batch)
		return nil
	})
	if err != nil {
		return nil, 0, err
	}
	return batches, maxSeq, nil
}
