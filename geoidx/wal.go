package geoidx

import (
	"encoding/binary"
	"hash/crc32"
	"slices"

	"github.com/evanoberholster/imageIdx/internal/persist"
)

const (
	geoWALMagic        uint32 = 0x4745574c // GEWL
	geoWALVersion      uint16 = 2
	geoWALHeaderSize          = 24
	geoWALMutationSize        = 24
	geoWALFilename            = "geo.wal"
)

type geoWAL struct{ *persist.WAL }

func openGeoWAL(path string) (*geoWAL, error) {
	wal, err := persist.OpenWAL(path, 0)
	if err != nil {
		return nil, err
	}
	return &geoWAL{WAL: wal}, nil
}

func (w *geoWAL) append(mutations []Mutation, durability Durability) error {
	if len(mutations) == 0 {
		return nil
	}
	if len(mutations) > int(^uint32(0))/geoWALMutationSize {
		return ErrInvalidConfig
	}
	ordered := append([]Mutation(nil), mutations...)
	slices.SortFunc(ordered, func(left, right Mutation) int {
		if left.ImageID < right.ImageID {
			return -1
		}
		if left.ImageID > right.ImageID {
			return 1
		}
		return 0
	})
	payload := w.Scratch(len(ordered) * geoWALMutationSize)
	for index, mutation := range ordered {
		position := index * geoWALMutationSize
		binary.LittleEndian.PutUint64(payload[position:position+8], mutation.ImageID)
		binary.LittleEndian.PutUint64(payload[position+8:position+16], uint64(mutation.From))
		binary.LittleEndian.PutUint64(payload[position+16:position+24], uint64(mutation.To))
	}
	header := make([]byte, geoWALHeaderSize)
	binary.LittleEndian.PutUint32(header[0:4], geoWALMagic)
	binary.LittleEndian.PutUint16(header[4:6], geoWALVersion)
	binary.LittleEndian.PutUint32(header[8:12], uint32(len(ordered)))
	binary.LittleEndian.PutUint32(header[12:16], uint32(len(payload)))
	binary.LittleEndian.PutUint32(header[16:20], crc32.ChecksumIEEE(payload))
	return w.AppendFrame(header, payload, durability == DurabilitySync)
}

func replayGeoWAL(path string, offset int64) ([][]Mutation, error) {
	var batches [][]Mutation
	err := persist.Replay(path, offset, persist.FrameSpec{
		HeaderSize: geoWALHeaderSize,
		Magic:      geoWALMagic,
		Version:    geoWALVersion,
		Corrupt:    ErrCorruptWAL,
	}, func(header []byte) (uint64, error) {
		count := binary.LittleEndian.Uint32(header[8:12])
		payloadSize := binary.LittleEndian.Uint32(header[12:16])
		if uint64(payloadSize) != uint64(count)*geoWALMutationSize {
			return 0, ErrCorruptWAL
		}
		return uint64(payloadSize), nil
	}, func(header, payload []byte) error {
		if crc32.ChecksumIEEE(payload) != binary.LittleEndian.Uint32(header[16:20]) {
			return ErrCorruptWAL
		}
		count := binary.LittleEndian.Uint32(header[8:12])
		batch := make([]Mutation, 0, count)
		seen := make(map[ImageID]struct{}, count)
		for index := uint32(0); index < count; index++ {
			position := index * geoWALMutationSize
			mutation := Mutation{
				ImageID: binary.LittleEndian.Uint64(payload[position : position+8]),
				From:    S2Key(binary.LittleEndian.Uint64(payload[position+8 : position+16])),
				To:      S2Key(binary.LittleEndian.Uint64(payload[position+16 : position+24])),
			}
			if validateMutation(mutation) != nil {
				return ErrCorruptWAL
			}
			if _, duplicate := seen[mutation.ImageID]; duplicate {
				return ErrCorruptWAL
			}
			seen[mutation.ImageID] = struct{}{}
			batch = append(batch, mutation)
		}
		batches = append(batches, batch)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return batches, nil
}
