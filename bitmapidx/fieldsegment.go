package bitmapidx

import (
	"encoding/binary"
	"hash/crc32"
	"io"
	"os"
	"slices"

	"github.com/evanoberholster/imageIdx/internal/indexutil"
	"github.com/evanoberholster/imageIdx/internal/persist"
	"github.com/evanoberholster/imageIdx/roar"
)

const (
	fieldSegmentMagic      uint32 = 0x424d4653
	fieldSegmentHeaderSize        = 64
	fieldSegmentEntrySize         = 64
)

// Snapshot entry (64 bytes): Type u16, Slot u16, reserved u32, Hash u64,
// cardinality u64, bitmap offset/length u64, canonical offset/length u64,
// reserved u64. All offsets are absolute. Zero-cardinality dictionary values
// have no bitmap. Numeric/geo entries have no canonical bytes.
type fieldSnapshotEntry struct {
	key       FieldKey
	posting   *posting
	canonical []byte
}

func writeFieldSegment(path string, gen uint64, indexes []*FieldIndex, vacuum bool) error {
	registry := NewRegistry()
	var entries []fieldSnapshotEntry
	for _, index := range indexes {
		view := index.view.Load()
		if err := registry.Register(view.field); err != nil {
			return err
		}
		table := view.effectiveTable(view.frozen, view.active)
		keys := slices.Clone(table.keys)
		if !vacuum {
			for _, entry := range view.dict.merged() {
				keys = append(keys, entry.key)
			}
		}
		slices.SortFunc(keys, compareFieldKey)
		keys = slices.Compact(keys)
		for _, key := range keys {
			p, _ := table.get(key)
			entry := fieldSnapshotEntry{key: key, posting: p}
			if view.field.Kind != KindContinuous && view.field.Kind != KindGeo {
				e, ok := view.dict.entry(key)
				if !ok {
					return ErrCorruptSegment
				}
				entry.canonical = view.dict.bytes(e)
			}
			entries = append(entries, entry)
		}
	}
	defs, err := registry.MarshalBinary()
	if err != nil {
		return err
	}
	dirOffset := indexutil.Align(uint64(fieldSegmentHeaderSize+len(defs)), 8)
	directory := make([]byte, len(entries)*fieldSegmentEntrySize)
	payloadOffset := indexutil.Align(dirOffset+uint64(len(directory)), 4096)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := indexutil.WriteFull(f, make([]byte, payloadOffset)); err != nil {
		return err
	}
	position := payloadOffset
	for n, entry := range entries {
		record := directory[n*64 : (n+1)*64]
		binary.LittleEndian.PutUint16(record[0:2], uint16(entry.key.Type))
		binary.LittleEndian.PutUint16(record[2:4], entry.key.Slot)
		binary.LittleEndian.PutUint64(record[8:16], entry.key.Hash)
		if entry.posting != nil {
			payload, err := entry.posting.bitmap().MarshalBinary()
			if err != nil {
				return err
			}
			binary.LittleEndian.PutUint64(record[16:24], entry.posting.cardinality())
			binary.LittleEndian.PutUint64(record[24:32], position)
			binary.LittleEndian.PutUint64(record[32:40], uint64(len(payload)))
			if err := indexutil.WriteFull(f, payload); err != nil {
				return err
			}
			position += uint64(len(payload))
		}
		binary.LittleEndian.PutUint64(record[40:48], position)
		binary.LittleEndian.PutUint64(record[48:56], uint64(len(entry.canonical)))
		if err := indexutil.WriteFull(f, entry.canonical); err != nil {
			return err
		}
		position += uint64(len(entry.canonical))
		pad := indexutil.Align(position, 8) - position
		if err := indexutil.WriteFull(f, make([]byte, pad)); err != nil {
			return err
		}
		position += pad
	}
	if _, err := f.WriteAt(defs, 64); err != nil {
		return err
	}
	if _, err := f.WriteAt(directory, int64(dirOffset)); err != nil {
		return err
	}
	checksum := crc32.NewIEEE()
	if _, err := io.Copy(checksum, io.NewSectionReader(f, 64, int64(position)-64)); err != nil {
		return err
	}
	var header [64]byte
	binary.LittleEndian.PutUint32(header[0:4], fieldSegmentMagic)
	binary.LittleEndian.PutUint16(header[4:6], 1)
	binary.LittleEndian.PutUint16(header[6:8], 64)
	binary.LittleEndian.PutUint64(header[8:16], gen)
	binary.LittleEndian.PutUint64(header[16:24], uint64(len(indexes)))
	binary.LittleEndian.PutUint64(header[24:32], 64)
	binary.LittleEndian.PutUint64(header[32:40], uint64(len(defs)))
	binary.LittleEndian.PutUint64(header[40:48], uint64(len(entries)))
	binary.LittleEndian.PutUint64(header[48:56], dirOffset)
	binary.LittleEndian.PutUint32(header[56:60], checksum.Sum32())
	binary.LittleEndian.PutUint32(header[60:64], crc32.ChecksumIEEE(header[:60]))
	if _, err := f.WriteAt(header[:], 0); err != nil {
		return err
	}
	return f.Sync()
}

func openFieldSegment(path string, gen uint64, config FieldConfig) ([]*FieldIndex, []byte, error) {
	mapping, err := persist.MapFile(path, ErrCorruptSegment)
	if err != nil {
		return nil, nil, err
	}
	fail := func() ([]*FieldIndex, []byte, error) { persist.Unmap(mapping); return nil, nil, ErrCorruptSegment }
	if len(mapping) < 64 || len(mapping)%8 != 0 {
		return fail()
	}
	h := mapping[:64]
	if binary.LittleEndian.Uint32(h[0:4]) != fieldSegmentMagic || binary.LittleEndian.Uint16(h[4:6]) != 1 || binary.LittleEndian.Uint16(h[6:8]) != 64 ||
		binary.LittleEndian.Uint64(h[8:16]) != gen || crc32.ChecksumIEEE(h[:60]) != binary.LittleEndian.Uint32(h[60:64]) || crc32.ChecksumIEEE(mapping[64:]) != binary.LittleEndian.Uint32(h[56:60]) {
		return fail()
	}
	registryOffset := binary.LittleEndian.Uint64(h[24:32])
	registryLen := binary.LittleEndian.Uint64(h[32:40])
	registryEnd, ok := indexutil.CheckedEnd(registryOffset, registryLen, 1, uint64(len(mapping)))
	if !ok || registryOffset != 64 {
		return fail()
	}
	registry := NewRegistry()
	if err := registry.UnmarshalBinary(mapping[registryOffset:registryEnd]); err != nil {
		return fail()
	}
	if uint64(registry.Len()) != binary.LittleEndian.Uint64(h[16:24]) {
		return fail()
	}
	count := binary.LittleEndian.Uint64(h[40:48])
	dirOffset := binary.LittleEndian.Uint64(h[48:56])
	dirEnd, ok := indexutil.CheckedEnd(dirOffset, count, 64, uint64(len(mapping)))
	if !ok || dirOffset != indexutil.Align(registryEnd, 8) {
		return fail()
	}
	payloadStart := indexutil.Align(dirEnd, 4096)
	if payloadStart > uint64(len(mapping)) {
		return fail()
	}
	var indexes []*FieldIndex
	byType := make(map[IndexType]*FieldIndex)
	for _, field := range registry.Fields() {
		index, err := NewFieldIndex(field, config)
		if err != nil {
			return fail()
		}
		index.dict.chunks = [][]byte{mapping[:len(mapping):len(mapping)]}
		indexes = append(indexes, index)
		byType[field.Type] = index
	}
	position := payloadStart
	var previous FieldKey
	for n := uint64(0); n < count; n++ {
		record := mapping[dirOffset+n*64 : dirOffset+(n+1)*64]
		key := FieldKey{Type: IndexType(binary.LittleEndian.Uint16(record[0:2])), Slot: binary.LittleEndian.Uint16(record[2:4]), Hash: binary.LittleEndian.Uint64(record[8:16])}
		if n > 0 && compareFieldKey(previous, key) >= 0 {
			return fail()
		}
		previous = key
		index, ok := byType[key.Type]
		if !ok {
			return fail()
		}
		card := binary.LittleEndian.Uint64(record[16:24])
		bitmapOffset := binary.LittleEndian.Uint64(record[24:32])
		bitmapLen := binary.LittleEndian.Uint64(record[32:40])
		valueOffset := binary.LittleEndian.Uint64(record[40:48])
		valueLen := binary.LittleEndian.Uint64(record[48:56])
		if bitmapLen != 0 {
			end, ok := indexutil.CheckedEnd(bitmapOffset, bitmapLen, 1, uint64(len(mapping)))
			if !ok || bitmapOffset != position || bitmapOffset%8 != 0 {
				return fail()
			}
			bit, err := roar.Open(mapping[bitmapOffset:end])
			if err != nil || bit.Cardinality() != card || card == 0 {
				return fail()
			}
			v := index.view.Load()
			v.base.keys = append(v.base.keys, key)
			v.base.postings = append(v.base.postings, makePosting(bit))
			position = end
		} else if card != 0 || bitmapOffset != 0 {
			return fail()
		}
		end, ok := indexutil.CheckedEnd(valueOffset, valueLen, 1, uint64(len(mapping)))
		if !ok || valueOffset != position || valueLen > MaxValueBytes {
			return fail()
		}
		field := index.Field()
		if field.Kind == KindContinuous || field.Kind == KindGeo {
			if valueLen != 0 || key.Slot != 0 || card == 0 {
				return fail()
			}
			if field.Kind == KindGeo {
				if _, err := CellIDFromKey(key.Hash); err != nil {
					return fail()
				}
			}
		} else {
			value := mapping[valueOffset:end]
			if hashCanonical(field.Type, field.Kind, value).Hash != key.Hash {
				return fail()
			}
			// Offsets inside one chunk use u32. Split the arena into independent
			// mapped value chunks for files exceeding that addressable span.
			chunk := uint32(0)
			offset := uint32(valueOffset)
			if end > uint64(^uint32(0)) {
				chunk = uint32(len(index.dict.chunks))
				offset = 0
				index.dict.chunks = append(index.dict.chunks, mapping[valueOffset:end:end])
			}
			index.dict.base = append(index.dict.base, dictEntry{key: key, chunk: chunk, offset: offset, length: uint32(valueLen)})
		}
		position = indexutil.Align(end, 8)
		if position > uint64(len(mapping)) {
			return fail()
		}
	}
	if position != uint64(len(mapping)) {
		return fail()
	}
	for _, index := range indexes {
		index.dict.generation = 1
		v := index.view.Load()
		v.dict = index.dict.view()
		v.dictGen = 1
	}
	return indexes, mapping, nil
}
