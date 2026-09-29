package timeidx

import (
	"encoding/binary"
	"errors"
	"hash/crc32"

	common "github.com/evanoberholster/imageIdx/common"
	"github.com/evanoberholster/imageIdx/internal/storage"
)

const (
	walMagic      uint32 = 0x5453574c // TSWL
	walVersion    uint16 = 1
	walHeaderSize        = 16
	walWriteSize         = 24
)

type timestampWAL struct {
	*storage.WAL
}

var ErrCorruptWAL = errors.New("timeidx: corrupt write-ahead log")

func openTimestampWAL(path string, batchCapacity int) (*timestampWAL, error) {
	if batchCapacity < 256 {
		batchCapacity = 256
	}
	wal, err := storage.OpenWAL(path, walHeaderSize+batchCapacity*walWriteSize)
	if err != nil {
		return nil, err
	}
	return &timestampWAL{WAL: wal}, nil
}

// appendBatch frames one mutation batch. Group commits append every frame
// unsynced and fsync once for the whole group, so sync only requests the
// fsync; the frame always appends.
func (w *timestampWAL) appendBatch(writes []write, sync bool) error {
	if len(writes) == 0 {
		return nil
	}
	if len(writes) > int(^uint16(0)) {
		return ErrBatchTooLarge
	}
	required := walHeaderSize + len(writes)*walWriteSize
	record := w.Scratch(required)
	clear(record[:walHeaderSize])
	binary.LittleEndian.PutUint32(record[0:4], walMagic)
	binary.LittleEndian.PutUint16(record[4:6], walVersion)
	binary.LittleEndian.PutUint16(record[6:8], uint16(len(writes)))
	binary.LittleEndian.PutUint32(record[8:12], uint32(len(writes)*walWriteSize))
	for index, write := range writes {
		offset := walHeaderSize + index*walWriteSize
		binary.LittleEndian.PutUint64(record[offset:offset+8], write.timestamp)
		binary.LittleEndian.PutUint64(record[offset+8:offset+16], uint64(write.imageID))
		record[offset+16] = byte(write.op)
		clear(record[offset+17 : offset+walWriteSize])
	}
	binary.LittleEndian.PutUint32(record[12:16], crc32.ChecksumIEEE(record[walHeaderSize:]))
	return w.WriteRecord(record, sync)
}

// validateWALFrame resolves one frame header to its payload size. Count
// and size arrive as uint16/uint32 fields compared in int arithmetic:
// both fit comfortably (at most 65535 writes), so no truncation can hide
// a malformed size the way a 32-bit multiply could. Shared by both replay
// entry points and the fuzz target so all three validate identically.
func validateWALFrame(header []byte) (uint64, error) {
	if len(header) < 12 {
		return 0, ErrCorruptWAL
	}
	count := int(binary.LittleEndian.Uint16(header[6:8]))
	payloadSize := int(binary.LittleEndian.Uint32(header[8:12]))
	if payloadSize != count*walWriteSize {
		return 0, ErrCorruptWAL
	}
	return uint64(payloadSize), nil
}

// replayWALEnd is replayWAL plus the exclusive end of the last complete
// frame, used by Vacuum to carry only complete frames into a fresh epoch.
func replayWALEnd(path string, offset int64) (int64, error) {
	return storage.ReplayEnd(path, offset, storage.FrameSpec{
		HeaderSize: walHeaderSize,
		Magic:      walMagic,
		Version:    walVersion,
		Corrupt:    ErrCorruptWAL,
	}, validateWALFrame, func(header, payload []byte) error {
		if crc32.ChecksumIEEE(payload) != binary.LittleEndian.Uint32(header[12:16]) {
			return ErrCorruptWAL
		}
		return nil
	})
}

func replayWAL(path string, offset int64) ([]write, error) {
	var writes []write
	err := storage.Replay(path, offset, storage.FrameSpec{
		HeaderSize: walHeaderSize,
		Magic:      walMagic,
		Version:    walVersion,
		Corrupt:    ErrCorruptWAL,
	}, func(header []byte) (uint64, error) {
		count := int(binary.LittleEndian.Uint16(header[6:8]))
		payloadSize := int(binary.LittleEndian.Uint32(header[8:12]))
		if payloadSize != count*walWriteSize {
			return 0, ErrCorruptWAL
		}
		return uint64(payloadSize), nil
	}, func(header, payload []byte) error {
		if crc32.ChecksumIEEE(payload) != binary.LittleEndian.Uint32(header[12:16]) {
			return ErrCorruptWAL
		}
		count := int(binary.LittleEndian.Uint16(header[6:8]))
		for index := 0; index < count; index++ {
			position := index * walWriteSize
			op := writeOp(payload[position+16])
			if op != writeAdd && op != writeDelete {
				return ErrCorruptWAL
			}
			writes = append(writes, write{
				timestamp: binary.LittleEndian.Uint64(payload[position : position+8]),
				imageID:   common.ValueID(binary.LittleEndian.Uint64(payload[position+8 : position+16])),
				op:        op,
			})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return writes, nil
}
