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
	fieldSegmentVersion           = 2
	fieldSegmentHeaderSize        = 80
	fieldSegmentEntrySize         = 64
)

// Snapshot entry (64 bytes): Type u16, Slot u16, reserved u32, Hash u64,
// cardinality u64, bitmap offset/length u64, canonical offset/length u64,
// reserved u64. All offsets are absolute file offsets. Zero-cardinality
// dictionary values persist with no bitmap so the value survives
// checkpoints; numeric and geo entries persist with no canonical bytes.
// Entries sort strictly by FieldKey with no gaps in the payload.
type fieldSnapshotEntry struct {
	key       FieldKey
	posting   *posting
	canonical []byte
}

// writeFieldSegment checkpoints effective postings and the dictionary to
// path with generation gen. Vacuum drops dead dictionary values; plain
// checkpoints preserve them. Layout: 80-byte header, registry, 64-byte
// directory, then 4K-aligned payload. Bitmaps marshal per posting (tiny
// postings materialize), canonical bytes follow each bitmap, each padded
// to 8. The header CRC covers the header prefix and the payload CRC covers
// everything after the header, so the fresh file re-reads once for
// integrity before Sync. Temporary files must be renamed by the caller.
func writeFieldSegment(path string, gen uint64, indexes []*FieldIndex, vacuum bool, storeID [16]byte) error {
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
	if err := writeZeros(f, payloadOffset); err != nil {
		return err
	}
	position := payloadOffset
	for n, entry := range entries {
		record := directory[n*64 : (n+1)*64]
		binary.LittleEndian.PutUint16(record[0:2], uint16(entry.key.Type))
		binary.LittleEndian.PutUint16(record[2:4], entry.key.Slot)
		binary.LittleEndian.PutUint64(record[8:16], entry.key.Hash)
		if entry.posting != nil {
			// The posting's serialized bytes are already assembled, and the
			// checkpoint holds every writer lock, so write them directly
			// instead of cloning through MarshalBinary.
			payload := entry.posting.bitmap().Bytes()
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
		if pad != 0 {
			var zeros [8]byte
			if err := indexutil.WriteFull(f, zeros[:pad]); err != nil {
				return err
			}
		}
		position += pad
	}
	if _, err := f.WriteAt(defs, fieldSegmentHeaderSize); err != nil {
		return err
	}
	if _, err := f.WriteAt(directory, int64(dirOffset)); err != nil {
		return err
	}
	checksum := crc32.NewIEEE()
	if _, err := io.Copy(checksum, io.NewSectionReader(f, fieldSegmentHeaderSize, int64(position)-fieldSegmentHeaderSize)); err != nil {
		return err
	}
	var header [fieldSegmentHeaderSize]byte
	binary.LittleEndian.PutUint32(header[0:4], fieldSegmentMagic)
	binary.LittleEndian.PutUint16(header[4:6], fieldSegmentVersion)
	binary.LittleEndian.PutUint16(header[6:8], fieldSegmentHeaderSize)
	binary.LittleEndian.PutUint64(header[8:16], gen)
	binary.LittleEndian.PutUint64(header[16:24], uint64(len(indexes)))
	binary.LittleEndian.PutUint64(header[24:32], fieldSegmentHeaderSize)
	binary.LittleEndian.PutUint64(header[32:40], uint64(len(defs)))
	binary.LittleEndian.PutUint64(header[40:48], uint64(len(entries)))
	binary.LittleEndian.PutUint64(header[48:56], dirOffset)
	binary.LittleEndian.PutUint32(header[56:60], checksum.Sum32())
	copy(header[60:76], storeID[:])
	binary.LittleEndian.PutUint32(header[76:80], crc32.ChecksumIEEE(header[:76]))
	if _, err := f.WriteAt(header[:], 0); err != nil {
		return err
	}
	return f.Sync()
}

// writeZeros reserves n zero bytes with a small stack buffer instead of
// one huge allocation, so large headers and directories do not spike the
// heap on checkpoint.
func writeZeros(f *os.File, n uint64) error {
	var zeros [32 << 10]byte
	for n > 0 {
		chunk := uint64(len(zeros))
		if chunk > n {
			chunk = n
		}
		if err := indexutil.WriteFull(f, zeros[:chunk]); err != nil {
			return err
		}
		n -= chunk
	}
	return nil
}

// openFieldSegment validates and memory-maps a checkpoint written by
// writeFieldSegment. It checks magic, version, generation, store identity,
// header and payload CRCs, registry shape, directory bounds and alignment,
// strict key ordering, contiguous 8-aligned payload offsets, cardinality
// agreement, hash agreement for hashed kinds, and geo key validity, with
// no trailing bytes. Bitmaps and canonical values alias the mapping for
// zero-copy reads; dictionary value offsets are 64-bit, so snapshots
// beyond 4 GiB need no extra chunks.
func openFieldSegment(path string, gen uint64, config FieldConfig, storeID [16]byte) ([]*FieldIndex, []byte, error) {
	mapping, err := persist.MapFile(path, ErrCorruptSegment)
	if err != nil {
		return nil, nil, err
	}
	fail := func() ([]*FieldIndex, []byte, error) { persist.Unmap(mapping); return nil, nil, ErrCorruptSegment }
	if len(mapping) < fieldSegmentHeaderSize || len(mapping)%8 != 0 {
		return fail()
	}
	h := mapping[:fieldSegmentHeaderSize]
	if binary.LittleEndian.Uint32(h[0:4]) != fieldSegmentMagic || binary.LittleEndian.Uint16(h[4:6]) != fieldSegmentVersion || binary.LittleEndian.Uint16(h[6:8]) != fieldSegmentHeaderSize ||
		binary.LittleEndian.Uint64(h[8:16]) != gen || crc32.ChecksumIEEE(h[:76]) != binary.LittleEndian.Uint32(h[76:80]) || crc32.ChecksumIEEE(mapping[fieldSegmentHeaderSize:]) != binary.LittleEndian.Uint32(h[56:60]) {
		return fail()
	}
	if [16]byte(h[60:76]) != storeID {
		return fail()
	}
	registryOffset := binary.LittleEndian.Uint64(h[24:32])
	registryLen := binary.LittleEndian.Uint64(h[32:40])
	registryEnd, ok := indexutil.CheckedEnd(registryOffset, registryLen, 1, uint64(len(mapping)))
	if !ok || registryOffset != fieldSegmentHeaderSize {
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
			v.base.postings = append(v.base.postings, makeBasePosting(bit))
			position = end
		} else if card != 0 || bitmapOffset != 0 {
			return fail()
		}
		end, ok := indexutil.CheckedEnd(valueOffset, valueLen, 1, uint64(len(mapping)))
		if !ok || valueOffset != position || valueLen > MaxValueBytes {
			return fail()
		}
		field := index.field()
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
			// Canonical values alias the single mapping chunk at a 64-bit
			// file offset, so snapshots beyond 4 GiB need no extra chunks.
			index.dict.base = append(index.dict.base, dictEntry{key: key, chunk: 0, offset: valueOffset, length: uint32(valueLen)})
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
