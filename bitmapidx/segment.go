package bitmapidx

import (
	"encoding/binary"
	"hash/crc32"
	"io"
	"os"
	"sort"

	"github.com/evanoberholster/imageIdx/internal/indexutil"
	"github.com/evanoberholster/imageIdx/internal/persist"
	"github.com/evanoberholster/imageIdx/roar"
)

const (
	segmentMagic      uint32 = 0x424d5347 // BMSG
	segmentVersion    uint16 = 2
	segmentHeaderSize        = 64
	segmentEntrySize         = 64
	segmentAlignment         = 4_096
)

type segmentEntry struct {
	indexType   IndexType
	version     uint16
	flags       uint32
	indexID     uint64
	valueID     uint64
	cardinality uint64
	offset      uint64
	length      uint32
	generation  uint64
}

// segment is one immutable on-disk generation of the index collection: a
// mmap'd file holding a directory of fixed-width entries followed by
// page-aligned serialized bitmap payloads.
type segment struct {
	generation uint64
	directory  *Directory

	mapping []byte
	readers *indexutil.ReaderTracker
}

// writeSegment writes all effective bitmap states into one ordered immutable
// segment. Payloads are page-aligned for direct mmap use by roar.
func writeSegment(path string, generation uint64, indexes []*Index) error {
	ordered := append([]*Index(nil), indexes...)
	for _, index := range ordered {
		if index == nil {
			return ErrInvalidIndex
		}
	}
	sort.Slice(ordered, func(i, j int) bool {
		a, b := ordered[i].view.Load(), ordered[j].view.Load()
		if a.indexType != b.indexType {
			return a.indexType < b.indexType
		}
		if a.valueID != b.valueID {
			return a.valueID < b.valueID
		}
		return a.indexID < b.indexID
	})

	file, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	ok := false
	defer func() {
		_ = file.Close()
		if !ok {
			_ = os.Remove(path)
		}
	}()

	directorySize := len(ordered) * segmentEntrySize
	payloadStart := indexutil.Align(uint64(segmentHeaderSize+directorySize), segmentAlignment)
	if _, err = file.Write(make([]byte, payloadStart)); err != nil {
		return err
	}

	entries := make([]segmentEntry, len(ordered))
	checksums := make([]uint32, len(ordered))
	var tokenStack [8]readToken
	tokens := acquireReaders(tokenStack[:0], ordered)
	for n, index := range ordered {
		view := index.view.Load()
		effective := view.effectiveBitmap()
		payload, _ := effective.MarshalBinary()
		if uint64(len(payload)) > uint64(^uint32(0)) {
			releaseReaders(tokens)
			return ErrInvalidIndex
		}
		offset, seekErr := file.Seek(0, io.SeekEnd)
		if seekErr != nil {
			releaseReaders(tokens)
			return seekErr
		}
		aligned := indexutil.Align(uint64(offset), segmentAlignment)
		if padding := int64(aligned) - offset; padding > 0 {
			if _, err = file.Write(make([]byte, padding)); err != nil {
				releaseReaders(tokens)
				return err
			}
		}
		if _, err = file.Write(payload); err != nil {
			releaseReaders(tokens)
			return err
		}
		if padding := indexutil.Align(uint64(len(payload)), 8) - uint64(len(payload)); padding != 0 {
			if _, err = file.Write(make([]byte, padding)); err != nil {
				releaseReaders(tokens)
				return err
			}
		}
		entries[n] = segmentEntry{
			indexType: view.indexType, version: view.version, flags: view.flags,
			indexID: view.indexID, valueID: view.valueID,
			cardinality: effective.Cardinality(),
			offset:      aligned, length: uint32(len(payload)), generation: view.generation,
		}
		checksums[n] = crc32.ChecksumIEEE(payload)
	}
	releaseReaders(tokens)

	directoryBytes := make([]byte, directorySize)
	for n, entry := range entries {
		encodeSegmentEntry(directoryBytes[n*segmentEntrySize:(n+1)*segmentEntrySize], entry, checksums[n])
	}
	header := make([]byte, segmentHeaderSize)
	binary.LittleEndian.PutUint32(header[0:4], segmentMagic)
	binary.LittleEndian.PutUint16(header[4:6], segmentVersion)
	binary.LittleEndian.PutUint16(header[6:8], segmentHeaderSize)
	binary.LittleEndian.PutUint64(header[8:16], generation)
	binary.LittleEndian.PutUint64(header[16:24], uint64(len(entries)))
	binary.LittleEndian.PutUint64(header[24:32], segmentHeaderSize)
	binary.LittleEndian.PutUint32(header[32:36], segmentEntrySize)
	binary.LittleEndian.PutUint64(header[40:48], payloadStart)
	binary.LittleEndian.PutUint32(header[48:52], crc32.ChecksumIEEE(directoryBytes))
	binary.LittleEndian.PutUint32(header[60:64], crc32.ChecksumIEEE(header[:60]))
	if _, err = file.WriteAt(header, 0); err != nil {
		return err
	}
	if len(directoryBytes) != 0 {
		if _, err = file.WriteAt(directoryBytes, segmentHeaderSize); err != nil {
			return err
		}
	}
	if err = file.Sync(); err != nil {
		return err
	}
	if err = file.Close(); err != nil {
		return err
	}
	ok = true
	return nil
}

func openSegment(path string, config Config) (*segment, error) {
	config, err := config.withDefaults()
	if err != nil {
		return nil, err
	}
	mapping, err := persist.MapFile(path, ErrCorruptSegment)
	if err != nil {
		return nil, err
	}
	fail := func() (*segment, error) {
		_ = persist.Unmap(mapping)
		return nil, ErrCorruptSegment
	}
	if len(mapping) < segmentHeaderSize || len(mapping)%8 != 0 {
		return fail()
	}
	header := mapping[:segmentHeaderSize]
	if binary.LittleEndian.Uint32(header[0:4]) != segmentMagic ||
		binary.LittleEndian.Uint16(header[4:6]) != segmentVersion ||
		binary.LittleEndian.Uint16(header[6:8]) != segmentHeaderSize ||
		binary.LittleEndian.Uint32(header[32:36]) != segmentEntrySize ||
		binary.LittleEndian.Uint32(header[60:64]) != crc32.ChecksumIEEE(header[:60]) {
		return fail()
	}
	count := binary.LittleEndian.Uint64(header[16:24])
	directoryOffset := binary.LittleEndian.Uint64(header[24:32])
	directoryEnd, validEnd := indexutil.CheckedEnd(directoryOffset, count, segmentEntrySize, uint64(len(mapping)))
	if !validEnd || directoryOffset != segmentHeaderSize {
		return fail()
	}
	directoryBytes := mapping[directoryOffset:directoryEnd]
	if binary.LittleEndian.Uint32(header[48:52]) != crc32.ChecksumIEEE(directoryBytes) {
		return fail()
	}
	indexes := make([]*Index, 0, count)
	readers := &indexutil.ReaderTracker{}
	var previous Key
	for n := uint64(0); n < count; n++ {
		encoded := directoryBytes[n*segmentEntrySize : (n+1)*segmentEntrySize]
		entry, checksum := decodeSegmentEntry(encoded)
		end := entry.offset + uint64(entry.length)
		if entry.version != bitmapFormatVersion || entry.indexType == 0 || entry.offset%segmentAlignment != 0 ||
			end < entry.offset || end > uint64(len(mapping)) {
			return fail()
		}
		payload := mapping[entry.offset:end]
		if crc32.ChecksumIEEE(payload) != checksum {
			return fail()
		}
		key := Key{Type: entry.indexType, ValueID: entry.valueID}
		if n != 0 && (key.Type < previous.Type || key.Type == previous.Type && key.ValueID < previous.ValueID) {
			return fail()
		}
		previous = key
		base, bufferErr := roar.Open(payload)
		if bufferErr != nil {
			return fail()
		}
		if base.Cardinality() != entry.cardinality {
			return fail()
		}
		index, indexErr := newIndexFromView(&indexView{
			version: entry.version, indexType: entry.indexType, flags: entry.flags,
			indexID: entry.indexID, valueID: entry.valueID, cardinality: entry.cardinality,
			generation: entry.generation, base: base,
		}, config, readers)
		if indexErr != nil {
			return fail()
		}
		indexes = append(indexes, index)
	}
	directory, err := NewDirectory(indexes...)
	if err != nil {
		_ = persist.Unmap(mapping)
		return nil, err
	}
	return &segment{
		generation: binary.LittleEndian.Uint64(header[8:16]),
		directory:  directory, mapping: mapping, readers: readers,
	}, nil
}

func (s *segment) close() error {
	if s == nil || s.mapping == nil {
		return nil
	}
	for _, index := range s.directory.Indexes() {
		index.writerMu.Lock()
		index.closed.Store(true)
		index.writerMu.Unlock()
		index.workers.Wait()
	}
	s.readers.WaitForPriorReaders()
	err := persist.Unmap(s.mapping)
	s.mapping = nil
	return err
}

func encodeSegmentEntry(dst []byte, e segmentEntry, checksum uint32) {
	clear(dst)
	binary.LittleEndian.PutUint16(dst[0:2], uint16(e.indexType))
	binary.LittleEndian.PutUint16(dst[2:4], e.version)
	binary.LittleEndian.PutUint32(dst[4:8], e.flags)
	binary.LittleEndian.PutUint64(dst[8:16], e.indexID)
	binary.LittleEndian.PutUint64(dst[16:24], e.valueID)
	binary.LittleEndian.PutUint64(dst[24:32], e.cardinality)
	binary.LittleEndian.PutUint64(dst[32:40], e.offset)
	binary.LittleEndian.PutUint32(dst[40:44], e.length)
	binary.LittleEndian.PutUint64(dst[48:56], e.generation)
	binary.LittleEndian.PutUint32(dst[56:60], checksum)
}

func decodeSegmentEntry(src []byte) (segmentEntry, uint32) {
	return segmentEntry{
		indexType: IndexType(binary.LittleEndian.Uint16(src[0:2])),
		version:   binary.LittleEndian.Uint16(src[2:4]), flags: binary.LittleEndian.Uint32(src[4:8]),
		indexID: binary.LittleEndian.Uint64(src[8:16]), valueID: binary.LittleEndian.Uint64(src[16:24]),
		cardinality: binary.LittleEndian.Uint64(src[24:32]), offset: binary.LittleEndian.Uint64(src[32:40]),
		length: binary.LittleEndian.Uint32(src[40:44]), generation: binary.LittleEndian.Uint64(src[48:56]),
	}, binary.LittleEndian.Uint32(src[56:60])
}
