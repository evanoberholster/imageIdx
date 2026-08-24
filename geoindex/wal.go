package geoindex

import (
	"encoding/binary"
	"errors"
	"hash/crc32"
	"io"
	"os"
	"slices"

	"github.com/evanoberholster/imageIdx/internal/indexutil"
)

const (
	geoWALMagic        uint32 = 0x4745574c // GEWL
	geoWALVersion      uint16 = 2
	geoWALHeaderSize          = 24
	geoWALMutationSize        = 24
	geoWALFilename            = "geo.wal"
)

type geoWAL struct{ file *os.File }

func openGeoWAL(path string) (*geoWAL, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	return &geoWAL{file: file}, nil
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
	payload := make([]byte, len(ordered)*geoWALMutationSize)
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
	start, err := w.offset()
	if err != nil {
		return err
	}
	rollback := func(writeErr error) error {
		_ = w.file.Truncate(start)
		_, _ = w.file.Seek(0, io.SeekEnd)
		return writeErr
	}
	if err := indexutil.WriteFull(w.file, header); err != nil {
		return rollback(err)
	}
	if err := indexutil.WriteFull(w.file, payload); err != nil {
		return rollback(err)
	}
	if durability == DurabilitySync {
		return w.file.Sync()
	}
	return nil
}

func (w *geoWAL) offset() (int64, error) { return w.file.Seek(0, io.SeekEnd) }
func (w *geoWAL) close() error           { return w.file.Close() }

func replayGeoWAL(path string, offset int64) ([][]Mutation, error) {
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
	header := make([]byte, geoWALHeaderSize)
	var batches [][]Mutation
	for {
		_, err = io.ReadFull(file, header)
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return batches, nil
		}
		if err != nil {
			return nil, err
		}
		if binary.LittleEndian.Uint32(header[0:4]) != geoWALMagic ||
			binary.LittleEndian.Uint16(header[4:6]) != geoWALVersion {
			return nil, ErrCorruptWAL
		}
		count := binary.LittleEndian.Uint32(header[8:12])
		payloadSize := binary.LittleEndian.Uint32(header[12:16])
		if uint64(payloadSize) != uint64(count)*geoWALMutationSize {
			return nil, ErrCorruptWAL
		}
		payload := make([]byte, payloadSize)
		_, err = io.ReadFull(file, payload)
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return batches, nil
		}
		if err != nil || crc32.ChecksumIEEE(payload) != binary.LittleEndian.Uint32(header[16:20]) {
			return nil, ErrCorruptWAL
		}
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
				return nil, ErrCorruptWAL
			}
			if _, duplicate := seen[mutation.ImageID]; duplicate {
				return nil, ErrCorruptWAL
			}
			seen[mutation.ImageID] = struct{}{}
			batch = append(batch, mutation)
		}
		batches = append(batches, batch)
	}
}
