package bitmapidx

import (
	"encoding/binary"
	"hash/crc32"
	"io"
	"os"
	"sort"
)

const (
	segmentMagic      uint32 = 0x424d5347 // BMSG
	segmentVersion    uint16 = 1
	segmentHeaderSize        = 64
	segmentEntrySize         = 64
	segmentAlignment         = 4_096
)

type BitmapDirectoryEntry struct {
	IndexType   IndexType
	Version     uint16
	Flags       Flags
	IndexID     uint64
	ValueID     uint64
	Cardinality uint64
	Offset      uint64
	Length      uint32
	Generation  uint64
}

type Segment struct {
	Generation uint64
	Entries    []BitmapDirectoryEntry
	Directory  *BitmapDirectory

	mapping []byte
	epoch   *Epoch
}

// WriteSegment writes all effective bitmap states into one ordered immutable
// segment. Payloads are page-aligned for direct mmap use by sroar.
func WriteSegment(path string, generation uint64, indexes []*BitmapIndex) error {
	ordered := append([]*BitmapIndex(nil), indexes...)
	for _, index := range ordered {
		if index == nil {
			return ErrInvalidIndex
		}
	}
	sort.Slice(ordered, func(i, j int) bool {
		a, b := ordered[i].view.Load(), ordered[j].view.Load()
		if a.IndexType != b.IndexType {
			return a.IndexType < b.IndexType
		}
		if a.ValueID != b.ValueID {
			return a.ValueID < b.ValueID
		}
		return a.IndexID < b.IndexID
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
	payloadStart := align(uint64(segmentHeaderSize+directorySize), segmentAlignment)
	if _, err = file.Write(make([]byte, payloadStart)); err != nil {
		return err
	}

	entries := make([]BitmapDirectoryEntry, len(ordered))
	checksums := make([]uint32, len(ordered))
	guards := enterEpochs(ordered)
	for n, index := range ordered {
		view := index.view.Load()
		effective := view.effectiveBitmap()
		payload := effective.MarshalBinary()
		offset, seekErr := file.Seek(0, io.SeekEnd)
		if seekErr != nil {
			exitGuards(guards)
			return seekErr
		}
		aligned := align(uint64(offset), segmentAlignment)
		if padding := int64(aligned) - offset; padding > 0 {
			if _, err = file.Write(make([]byte, padding)); err != nil {
				exitGuards(guards)
				return err
			}
		}
		if _, err = file.Write(payload); err != nil {
			exitGuards(guards)
			return err
		}
		entries[n] = BitmapDirectoryEntry{
			IndexType: view.IndexType, Version: view.Version, Flags: view.Flags,
			IndexID: view.IndexID, ValueID: view.ValueID,
			Cardinality: effective.Cardinality(),
			Offset:      aligned, Length: uint32(len(payload)), Generation: view.Generation,
		}
		checksums[n] = crc32.ChecksumIEEE(payload)
	}
	exitGuards(guards)

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

func OpenSegment(path string, config Config) (*Segment, error) {
	config, err := config.withDefaults()
	if err != nil {
		return nil, err
	}
	mapping, err := mmapFile(path)
	if err != nil {
		return nil, err
	}
	fail := func() (*Segment, error) {
		_ = unmapFile(mapping)
		return nil, ErrCorruptSegment
	}
	if len(mapping) < segmentHeaderSize {
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
	directoryEnd := directoryOffset + count*segmentEntrySize
	if directoryEnd < directoryOffset || directoryEnd > uint64(len(mapping)) {
		return fail()
	}
	directoryBytes := mapping[directoryOffset:directoryEnd]
	if binary.LittleEndian.Uint32(header[48:52]) != crc32.ChecksumIEEE(directoryBytes) {
		return fail()
	}
	indexes := make([]*BitmapIndex, 0, count)
	entries := make([]BitmapDirectoryEntry, 0, count)
	var previous BitmapKey
	for n := uint64(0); n < count; n++ {
		encoded := directoryBytes[n*segmentEntrySize : (n+1)*segmentEntrySize]
		entry, checksum := decodeSegmentEntry(encoded)
		end := entry.Offset + uint64(entry.Length)
		if entry.Version != BitmapFormatVersion || entry.IndexType == 0 || entry.Offset%segmentAlignment != 0 ||
			end < entry.Offset || end > uint64(len(mapping)) {
			return fail()
		}
		payload := mapping[entry.Offset:end]
		if crc32.ChecksumIEEE(payload) != checksum {
			return fail()
		}
		key := BitmapKey{Type: entry.IndexType, ValueID: entry.ValueID}
		if n != 0 && (key.Type < previous.Type || key.Type == previous.Type && key.ValueID < previous.ValueID) {
			return fail()
		}
		previous = key
		base := bitmapRefFromMappedBuffer(payload)
		if base.Cardinality() != entry.Cardinality {
			return fail()
		}
		index, indexErr := newBitmapIndexFromView(&BitmapIndexView{
			Version: entry.Version, IndexType: entry.IndexType, Flags: entry.Flags,
			IndexID: entry.IndexID, ValueID: entry.ValueID, Cardinality: entry.Cardinality,
			Generation: entry.Generation, Base: base,
		}, config)
		if indexErr != nil {
			return fail()
		}
		indexes = append(indexes, index)
		entries = append(entries, entry)
	}
	directory, err := NewBitmapDirectory(indexes...)
	if err != nil {
		_ = unmapFile(mapping)
		return nil, err
	}
	return &Segment{
		Generation: binary.LittleEndian.Uint64(header[8:16]),
		Entries:    entries, Directory: directory, mapping: mapping, epoch: config.Epoch,
	}, nil
}

func (s *Segment) Close() error {
	if s == nil || s.mapping == nil {
		return nil
	}
	for _, index := range s.Directory.Indexes() {
		index.writerMu.Lock()
		index.closed.Store(true)
		index.writerMu.Unlock()
		index.workers.Wait()
	}
	s.epoch.Synchronize()
	err := unmapFile(s.mapping)
	s.mapping = nil
	return err
}

func encodeSegmentEntry(dst []byte, e BitmapDirectoryEntry, checksum uint32) {
	clear(dst)
	binary.LittleEndian.PutUint16(dst[0:2], uint16(e.IndexType))
	binary.LittleEndian.PutUint16(dst[2:4], e.Version)
	binary.LittleEndian.PutUint32(dst[4:8], uint32(e.Flags))
	binary.LittleEndian.PutUint64(dst[8:16], e.IndexID)
	binary.LittleEndian.PutUint64(dst[16:24], e.ValueID)
	binary.LittleEndian.PutUint64(dst[24:32], e.Cardinality)
	binary.LittleEndian.PutUint64(dst[32:40], e.Offset)
	binary.LittleEndian.PutUint32(dst[40:44], e.Length)
	binary.LittleEndian.PutUint64(dst[48:56], e.Generation)
	binary.LittleEndian.PutUint32(dst[56:60], checksum)
}

func decodeSegmentEntry(src []byte) (BitmapDirectoryEntry, uint32) {
	return BitmapDirectoryEntry{
		IndexType: IndexType(binary.LittleEndian.Uint16(src[0:2])),
		Version:   binary.LittleEndian.Uint16(src[2:4]), Flags: Flags(binary.LittleEndian.Uint32(src[4:8])),
		IndexID: binary.LittleEndian.Uint64(src[8:16]), ValueID: binary.LittleEndian.Uint64(src[16:24]),
		Cardinality: binary.LittleEndian.Uint64(src[24:32]), Offset: binary.LittleEndian.Uint64(src[32:40]),
		Length: binary.LittleEndian.Uint32(src[40:44]), Generation: binary.LittleEndian.Uint64(src[48:56]),
	}, binary.LittleEndian.Uint32(src[56:60])
}

func align(value uint64, alignment uint64) uint64 {
	return (value + alignment - 1) &^ (alignment - 1)
}
