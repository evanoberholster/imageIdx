package bitmapidx

import (
	"encoding/binary"
	"hash/crc32"

	"github.com/evanoberholster/imageIdx/internal/persist"
)

const (
	bitmapWALMagic      uint32 = 0x424d574c // BMWL
	bitmapWALVersion    uint16 = 1
	bitmapWALHeaderSize        = 48
)

type bitmapWAL struct {
	*persist.WAL
}

type walMutation struct {
	key      Key
	indexID  uint64
	mutation Mutation
}

func openBitmapWAL(path string) (*bitmapWAL, error) {
	wal, err := persist.OpenWAL(path, 0)
	if err != nil {
		return nil, err
	}
	return &bitmapWAL{WAL: wal}, nil
}

func (w *bitmapWAL) append(key Key, indexID uint64, mutation Mutation, durability Durability) error {
	if len(mutation.Add) == 0 && len(mutation.Remove) == 0 {
		return nil
	}
	count := len(mutation.Add) + len(mutation.Remove)
	if count > (64<<20)/8 {
		return ErrInvalidIndex
	}
	payload := w.Scratch(count * 8)
	for n, id := range mutation.Add {
		binary.LittleEndian.PutUint64(payload[n*8:(n+1)*8], id)
	}
	for n, id := range mutation.Remove {
		position := (len(mutation.Add) + n) * 8
		binary.LittleEndian.PutUint64(payload[position:position+8], id)
	}
	header := make([]byte, bitmapWALHeaderSize)
	binary.LittleEndian.PutUint32(header[0:4], bitmapWALMagic)
	binary.LittleEndian.PutUint16(header[4:6], bitmapWALVersion)
	binary.LittleEndian.PutUint16(header[6:8], uint16(key.Type))
	binary.LittleEndian.PutUint32(header[12:16], uint32(len(payload)))
	binary.LittleEndian.PutUint32(header[16:20], crc32.ChecksumIEEE(payload))
	binary.LittleEndian.PutUint32(header[20:24], uint32(len(mutation.Add)))
	binary.LittleEndian.PutUint32(header[24:28], uint32(len(mutation.Remove)))
	binary.LittleEndian.PutUint64(header[32:40], indexID)
	binary.LittleEndian.PutUint64(header[40:48], key.ValueID)
	return w.AppendFrame(header, payload, durability == DurabilitySync)
}

func replayBitmapWAL(path string, offset int64) ([]walMutation, error) {
	records, _, err := replayBitmapWALEnd(path, offset)
	return records, err
}

func replayBitmapWALEnd(path string, offset int64) ([]walMutation, int64, error) {
	var records []walMutation
	end, err := persist.ReplayEnd(path, offset, persist.FrameSpec{
		HeaderSize: bitmapWALHeaderSize,
		Magic:      bitmapWALMagic,
		Version:    bitmapWALVersion,
		Corrupt:    ErrCorruptWAL,
		MaxPayload: 64 << 20,
	}, func(header []byte) (uint64, error) {
		payloadSize := binary.LittleEndian.Uint32(header[12:16])
		adds := binary.LittleEndian.Uint32(header[20:24])
		removes := binary.LittleEndian.Uint32(header[24:28])
		if uint64(payloadSize) != (uint64(adds)+uint64(removes))*8 {
			return 0, ErrCorruptWAL
		}
		return uint64(payloadSize), nil
	}, func(header, payload []byte) error {
		if crc32.ChecksumIEEE(payload) != binary.LittleEndian.Uint32(header[16:20]) {
			return ErrCorruptWAL
		}
		adds := binary.LittleEndian.Uint32(header[20:24])
		removes := binary.LittleEndian.Uint32(header[24:28])
		record := walMutation{
			key:      Key{Type: IndexType(binary.LittleEndian.Uint16(header[6:8])), ValueID: binary.LittleEndian.Uint64(header[40:48])},
			indexID:  binary.LittleEndian.Uint64(header[32:40]),
			mutation: Mutation{Add: make([]ImageID, adds), Remove: make([]ImageID, removes)},
		}
		for n := range record.mutation.Add {
			record.mutation.Add[n] = binary.LittleEndian.Uint64(payload[n*8 : (n+1)*8])
		}
		for n := range record.mutation.Remove {
			position := (int(adds) + n) * 8
			record.mutation.Remove[n] = binary.LittleEndian.Uint64(payload[position : position+8])
		}
		records = append(records, record)
		return nil
	})
	if err != nil {
		return nil, end, err
	}
	return records, end, nil
}
