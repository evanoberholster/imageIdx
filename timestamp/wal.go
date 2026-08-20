package timestamp

import (
	"encoding/binary"
	"errors"
	"hash/crc32"
	"io"
	"os"
)

const (
	walMagic      uint32 = 0x5453574c // TSWL
	walVersion    uint16 = 1
	walHeaderSize        = 16
	walWriteSize         = 24
)

var ErrCorruptWAL = errors.New("timestamp: corrupt write-ahead log")

type timestampWAL struct {
	file   *os.File
	buffer []byte
}

func openTimestampWAL(path string, batchCapacity int) (*timestampWAL, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	if batchCapacity < 256 {
		batchCapacity = 256
	}
	return &timestampWAL{
		file:   file,
		buffer: make([]byte, walHeaderSize+batchCapacity*walWriteSize),
	}, nil
}

func (w *timestampWAL) appendBatch(writes []TimestampWrite, durability Durability) error {
	if len(writes) == 0 {
		return nil
	}
	if len(writes) > int(^uint16(0)) {
		return ErrBatchTooLarge
	}
	required := walHeaderSize + len(writes)*walWriteSize
	if required > len(w.buffer) {
		w.buffer = make([]byte, required)
	}
	record := w.buffer[:required]
	clear(record[:walHeaderSize])
	binary.LittleEndian.PutUint32(record[0:4], walMagic)
	binary.LittleEndian.PutUint16(record[4:6], walVersion)
	binary.LittleEndian.PutUint16(record[6:8], uint16(len(writes)))
	binary.LittleEndian.PutUint32(record[8:12], uint32(len(writes)*walWriteSize))
	for index, write := range writes {
		offset := walHeaderSize + index*walWriteSize
		binary.LittleEndian.PutUint64(record[offset:offset+8], write.Timestamp)
		binary.LittleEndian.PutUint64(record[offset+8:offset+16], uint64(write.ImageID))
		record[offset+16] = byte(write.Op)
		clear(record[offset+17 : offset+walWriteSize])
	}
	binary.LittleEndian.PutUint32(record[12:16], crc32.ChecksumIEEE(record[walHeaderSize:]))
	written, err := w.file.Write(record)
	if err != nil {
		return err
	}
	if written != len(record) {
		return io.ErrShortWrite
	}
	if durability == DurabilitySync {
		return w.file.Sync()
	}
	return nil
}

func (w *timestampWAL) offset() (int64, error) {
	return w.file.Seek(0, io.SeekEnd)
}

func (w *timestampWAL) close() error { return w.file.Close() }

func replayWAL(path string, offset int64) ([]TimestampWrite, error) {
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer file.Close()
	if _, err = file.Seek(offset, io.SeekStart); err != nil {
		return nil, err
	}

	var writes []TimestampWrite
	header := make([]byte, walHeaderSize)
	for {
		_, err = io.ReadFull(file, header)
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return writes, nil
		}
		if err != nil {
			return nil, err
		}
		if binary.LittleEndian.Uint32(header[0:4]) != walMagic ||
			binary.LittleEndian.Uint16(header[4:6]) != walVersion {
			return nil, ErrCorruptWAL
		}
		count := int(binary.LittleEndian.Uint16(header[6:8]))
		payloadSize := int(binary.LittleEndian.Uint32(header[8:12]))
		if payloadSize != count*walWriteSize {
			return nil, ErrCorruptWAL
		}
		payload := make([]byte, payloadSize)
		_, err = io.ReadFull(file, payload)
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return writes, nil
		}
		if err != nil {
			return nil, err
		}
		if crc32.ChecksumIEEE(payload) != binary.LittleEndian.Uint32(header[12:16]) {
			return nil, ErrCorruptWAL
		}
		for index := 0; index < count; index++ {
			position := index * walWriteSize
			op := WriteOp(payload[position+16])
			if op != WriteAdd && op != WriteDelete {
				return nil, ErrCorruptWAL
			}
			writes = append(writes, TimestampWrite{
				Timestamp: binary.LittleEndian.Uint64(payload[position : position+8]),
				ImageID:   ImageID(binary.LittleEndian.Uint64(payload[position+8 : position+16])),
				Op:        op,
			})
		}
	}
}
