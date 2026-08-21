package bitmapidx

import (
	"encoding/binary"
	"errors"
	"hash/crc32"
	"io"
	"os"
)

const (
	bitmapWALMagic      uint32 = 0x424d574c // BMWL
	bitmapWALVersion    uint16 = 1
	bitmapWALHeaderSize        = 48
)

type bitmapWAL struct {
	file *os.File
}

type walMutation struct {
	key      BitmapKey
	indexID  uint64
	mutation BitmapMutation
}

func openBitmapWAL(path string) (*bitmapWAL, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	return &bitmapWAL{file: file}, nil
}

func (w *bitmapWAL) append(key BitmapKey, indexID uint64, mutation BitmapMutation, durability Durability) error {
	if len(mutation.Additions) == 0 && len(mutation.Removals) == 0 {
		return nil
	}
	count := len(mutation.Additions) + len(mutation.Removals)
	if count > int(^uint32(0))/8 {
		return ErrInvalidIndex
	}
	payload := make([]byte, count*8)
	for n, id := range mutation.Additions {
		binary.LittleEndian.PutUint64(payload[n*8:(n+1)*8], id)
	}
	for n, id := range mutation.Removals {
		position := (len(mutation.Additions) + n) * 8
		binary.LittleEndian.PutUint64(payload[position:position+8], id)
	}
	header := make([]byte, bitmapWALHeaderSize)
	binary.LittleEndian.PutUint32(header[0:4], bitmapWALMagic)
	binary.LittleEndian.PutUint16(header[4:6], bitmapWALVersion)
	binary.LittleEndian.PutUint16(header[6:8], uint16(key.Type))
	binary.LittleEndian.PutUint32(header[12:16], uint32(len(payload)))
	binary.LittleEndian.PutUint32(header[16:20], crc32.ChecksumIEEE(payload))
	binary.LittleEndian.PutUint32(header[20:24], uint32(len(mutation.Additions)))
	binary.LittleEndian.PutUint32(header[24:28], uint32(len(mutation.Removals)))
	binary.LittleEndian.PutUint64(header[32:40], indexID)
	binary.LittleEndian.PutUint64(header[40:48], key.ValueID)
	if err := writeFull(w.file, header); err != nil {
		return err
	}
	if err := writeFull(w.file, payload); err != nil {
		return err
	}
	if durability == DurabilitySync {
		return w.file.Sync()
	}
	return nil
}

func (w *bitmapWAL) offset() (int64, error) { return w.file.Seek(0, io.SeekEnd) }
func (w *bitmapWAL) close() error           { return w.file.Close() }

func replayBitmapWAL(path string, offset int64) ([]walMutation, error) {
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
	var records []walMutation
	header := make([]byte, bitmapWALHeaderSize)
	for {
		_, err = io.ReadFull(file, header)
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return records, nil
		}
		if err != nil {
			return nil, err
		}
		if binary.LittleEndian.Uint32(header[0:4]) != bitmapWALMagic ||
			binary.LittleEndian.Uint16(header[4:6]) != bitmapWALVersion {
			return nil, ErrCorruptWAL
		}
		payloadSize := binary.LittleEndian.Uint32(header[12:16])
		adds := binary.LittleEndian.Uint32(header[20:24])
		removes := binary.LittleEndian.Uint32(header[24:28])
		if uint64(payloadSize) != uint64(adds+removes)*8 {
			return nil, ErrCorruptWAL
		}
		payload := make([]byte, payloadSize)
		_, err = io.ReadFull(file, payload)
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return records, nil
		}
		if err != nil || crc32.ChecksumIEEE(payload) != binary.LittleEndian.Uint32(header[16:20]) {
			return nil, ErrCorruptWAL
		}
		record := walMutation{
			key:      BitmapKey{Type: IndexType(binary.LittleEndian.Uint16(header[6:8])), ValueID: binary.LittleEndian.Uint64(header[40:48])},
			indexID:  binary.LittleEndian.Uint64(header[32:40]),
			mutation: BitmapMutation{Additions: make([]uint64, adds), Removals: make([]uint64, removes)},
		}
		for n := range record.mutation.Additions {
			record.mutation.Additions[n] = binary.LittleEndian.Uint64(payload[n*8 : (n+1)*8])
		}
		for n := range record.mutation.Removals {
			position := (int(adds) + n) * 8
			record.mutation.Removals[n] = binary.LittleEndian.Uint64(payload[position : position+8])
		}
		records = append(records, record)
	}
}

func writeFull(writer io.Writer, data []byte) error {
	for len(data) != 0 {
		n, err := writer.Write(data)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		data = data[n:]
	}
	return nil
}
