package bitmapidx

import (
	"encoding/binary"
	"hash/crc32"
	"io"
	"os"
	"slices"

	common "github.com/evanoberholster/imageIdx/common"
	"github.com/evanoberholster/imageIdx/internal/storage"
	"github.com/evanoberholster/imageIdx/roar"
)

const namespaceSegmentMagic uint32 = 0x424d4653

const (
	namespaceSegmentVersion    = 2
	namespaceSegmentHeaderSize = 80
	namespaceSegmentEntrySize  = 64
)

// Snapshot entry (64 bytes): Type u16, Slot u16, reserved u32, Hash u64,
// cardinality u64, bitmap offset/length u64, canonical offset/length u64,
// reserved u64. All offsets are absolute file offsets. Zero-cardinality
// dictionary values persist with no bitmap so the value survives
// checkpoints; numeric and geo entries persist with no canonical bytes.
// Entries sort strictly by NamespaceKey with no gaps in the payload.
type namespaceSnapshotEntry struct {
	key       NamespaceKey
	posting   *posting
	canonical []byte
}

// writeNamespaceSegment checkpoints effective postings and the dictionary to
// path with generation gen. Vacuum drops dead dictionary values; plain
// checkpoints preserve them. Layout: 80-byte header, registry, 64-byte
// directory, then 4K-aligned payload. Bitmaps marshal per posting (tiny
// postings materialize), canonical bytes follow each bitmap, each padded
// to 8. The header CRC covers the header prefix and the payload CRC covers
// everything after the header, so the fresh file re-reads once for
// integrity before Sync. Temporary files must be renamed by the caller.
func writeNamespaceSegment(path string, gen uint64, indexes []*Index, vacuum bool, storeID [16]byte) error {
	specs := make([]NamespaceSpec, 0, len(indexes))
	var entries []namespaceSnapshotEntry
	for _, index := range indexes {
		view := index.view.Load()
		specs = append(specs, view.namespace)
		table := view.effectiveTable(view.frozen, view.active)
		keys := slices.Clone(table.keys)
		if !vacuum {
			for _, entry := range view.dict.merged() {
				keys = append(keys, entry.key)
			}
		}
		slices.SortFunc(keys, compareNamespaceKey)
		keys = slices.Compact(keys)
		for _, key := range keys {
			p, _ := table.get(key)
			entry := namespaceSnapshotEntry{key: key, posting: p}
			if namespaceUsesDictionary(view.namespace.Kind) {
				e, ok := view.dict.entry(key)
				if !ok {
					return ErrCorruptSegment
				}
				entry.canonical = view.dict.bytes(e)
			}
			entries = append(entries, entry)
		}
	}
	defs := encodeNamespaceSpecs(specs)
	dirOffset := storage.Align(uint64(namespaceSegmentHeaderSize+len(defs)), 8)
	directory := make([]byte, len(entries)*namespaceSegmentEntrySize)
	payloadOffset := storage.Align(dirOffset+uint64(len(directory)), 4096)
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
		binary.LittleEndian.PutUint16(record[0:2], uint16(entry.key.Namespace))
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
			if err := storage.WriteFull(f, payload); err != nil {
				return err
			}
			position += uint64(len(payload))
		}
		binary.LittleEndian.PutUint64(record[40:48], position)
		binary.LittleEndian.PutUint64(record[48:56], uint64(len(entry.canonical)))
		if err := storage.WriteFull(f, entry.canonical); err != nil {
			return err
		}
		position += uint64(len(entry.canonical))
		pad := storage.Align(position, 8) - position
		if pad != 0 {
			var zeros [8]byte
			if err := storage.WriteFull(f, zeros[:pad]); err != nil {
				return err
			}
		}
		position += pad
	}
	if _, err := f.WriteAt(defs, namespaceSegmentHeaderSize); err != nil {
		return err
	}
	if _, err := f.WriteAt(directory, int64(dirOffset)); err != nil {
		return err
	}
	checksum := crc32.NewIEEE()
	if _, err := io.Copy(checksum, io.NewSectionReader(f, namespaceSegmentHeaderSize, int64(position)-namespaceSegmentHeaderSize)); err != nil {
		return err
	}
	var header [namespaceSegmentHeaderSize]byte
	binary.LittleEndian.PutUint32(header[0:4], namespaceSegmentMagic)
	binary.LittleEndian.PutUint16(header[4:6], namespaceSegmentVersion)
	binary.LittleEndian.PutUint16(header[6:8], namespaceSegmentHeaderSize)
	binary.LittleEndian.PutUint64(header[8:16], gen)
	binary.LittleEndian.PutUint64(header[16:24], uint64(len(indexes)))
	binary.LittleEndian.PutUint64(header[24:32], namespaceSegmentHeaderSize)
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
		if err := storage.WriteFull(f, zeros[:chunk]); err != nil {
			return err
		}
		n -= chunk
	}
	return nil
}

// openNamespaceSegment validates and memory-maps a checkpoint written by
// writeNamespaceSegment. It checks magic, version, generation, store identity,
// header and payload CRCs, registry shape, directory bounds and alignment,
// strict key ordering, contiguous 8-aligned payload offsets, cardinality
// agreement, hash agreement for hashed kinds, and geo key validity, with
// no trailing bytes. Bitmaps and canonical values alias the mapping for
// zero-copy reads; dictionary value offsets are 64-bit, so snapshots
// beyond 4 GiB need no extra chunks.
func openNamespaceSegment(path string, gen uint64, config NamespaceConfig, storeID [16]byte) ([]*Index, []byte, error) {
	mapping, err := storage.MapFile(path, ErrCorruptSegment)
	if err != nil {
		return nil, nil, err
	}
	fail := func() ([]*Index, []byte, error) { storage.Unmap(mapping); return nil, nil, ErrCorruptSegment }
	if len(mapping) < namespaceSegmentHeaderSize || len(mapping)%8 != 0 {
		return fail()
	}
	h := mapping[:namespaceSegmentHeaderSize]
	if binary.LittleEndian.Uint32(h[0:4]) != namespaceSegmentMagic || binary.LittleEndian.Uint16(h[4:6]) != namespaceSegmentVersion || binary.LittleEndian.Uint16(h[6:8]) != namespaceSegmentHeaderSize ||
		binary.LittleEndian.Uint64(h[8:16]) != gen || crc32.ChecksumIEEE(h[:76]) != binary.LittleEndian.Uint32(h[76:80]) || crc32.ChecksumIEEE(mapping[namespaceSegmentHeaderSize:]) != binary.LittleEndian.Uint32(h[56:60]) {
		return fail()
	}
	if [16]byte(h[60:76]) != storeID {
		return fail()
	}
	registryOffset := binary.LittleEndian.Uint64(h[24:32])
	registryLen := binary.LittleEndian.Uint64(h[32:40])
	registryEnd, ok := storage.CheckedEnd(registryOffset, registryLen, 1, uint64(len(mapping)))
	if !ok || registryOffset != namespaceSegmentHeaderSize {
		return fail()
	}
	specs, err := decodeNamespaceSpecs(mapping[registryOffset:registryEnd])
	if err != nil {
		return fail()
	}
	if uint64(len(specs)) != binary.LittleEndian.Uint64(h[16:24]) {
		return fail()
	}
	count := binary.LittleEndian.Uint64(h[40:48])
	dirOffset := binary.LittleEndian.Uint64(h[48:56])
	dirEnd, ok := storage.CheckedEnd(dirOffset, count, 64, uint64(len(mapping)))
	if !ok || dirOffset != storage.Align(registryEnd, 8) {
		return fail()
	}
	payloadStart := storage.Align(dirEnd, 4096)
	if payloadStart > uint64(len(mapping)) {
		return fail()
	}
	var indexes []*Index
	byType := make(map[common.Namespace]*Index)
	for _, spec := range specs {
		index, err := NewMemory(spec, config)
		if err != nil {
			return fail()
		}
		index.dict.chunks = [][]byte{mapping[:len(mapping):len(mapping)]}
		indexes = append(indexes, index)
		byType[spec.Namespace] = index
	}
	position := payloadStart
	var previous NamespaceKey
	for n := uint64(0); n < count; n++ {
		record := mapping[dirOffset+n*64 : dirOffset+(n+1)*64]
		key := NamespaceKey{Namespace: common.Namespace(binary.LittleEndian.Uint16(record[0:2])), Slot: binary.LittleEndian.Uint16(record[2:4]), Hash: binary.LittleEndian.Uint64(record[8:16])}
		if n > 0 && compareNamespaceKey(previous, key) >= 0 {
			return fail()
		}
		previous = key
		index, ok := byType[key.Namespace]
		if !ok {
			return fail()
		}
		card := binary.LittleEndian.Uint64(record[16:24])
		bitmapOffset := binary.LittleEndian.Uint64(record[24:32])
		bitmapLen := binary.LittleEndian.Uint64(record[32:40])
		valueOffset := binary.LittleEndian.Uint64(record[40:48])
		valueLen := binary.LittleEndian.Uint64(record[48:56])
		if bitmapLen != 0 {
			end, ok := storage.CheckedEnd(bitmapOffset, bitmapLen, 1, uint64(len(mapping)))
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
		end, ok := storage.CheckedEnd(valueOffset, valueLen, 1, uint64(len(mapping)))
		if !ok || valueOffset != position || valueLen > MaxValueBytes {
			return fail()
		}
		spec := index.spec()
		if !namespaceUsesDictionary(spec.Kind) {
			if valueLen != 0 || key.Slot != 0 || card == 0 {
				return fail()
			}
			if spec.Kind == common.KindBitmapBSI && key.Hash >= bsiMaxSlices {
				return fail()
			}
		} else {
			value := mapping[valueOffset:end]
			if hashCanonical(spec.Namespace, spec.Kind, value).Hash != key.Hash {
				return fail()
			}
			// Canonical values alias the single mapping chunk at a 64-bit
			// file offset, so snapshots beyond 4 GiB need no extra chunks.
			index.dict.base = append(index.dict.base, dictEntry{key: key, chunk: 0, offset: valueOffset, length: uint32(valueLen)})
		}
		position = storage.Align(end, 8)
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
