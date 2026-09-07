package geoidx

import (
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"os"
	"path/filepath"

	"github.com/evanoberholster/imageIdx/idset"
	"github.com/evanoberholster/imageIdx/internal/indexutil"
	"github.com/evanoberholster/imageIdx/internal/persist"
)

func writeGeoGeneration(path string, storageGeneration uint64, view *indexView) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_RDWR, 0o644)
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

	sparse, dense := flattenView(view)
	sparseKeysOffset := uint64(geoHeaderSize)
	sparseKeysLength := uint64(len(sparse.keys)) * 8
	sparseValuesOffset := indexutil.Align(sparseKeysOffset+sparseKeysLength, 8)
	sparseValuesLength := uint64(len(sparse.imageIDs)) * 8
	bitmapKeysOffset := indexutil.Align(sparseValuesOffset+sparseValuesLength, 8)
	bitmapKeysLength := uint64(len(dense.keys)) * 8
	bitmapMetaOffset := indexutil.Align(bitmapKeysOffset+bitmapKeysLength, 8)
	bitmapMetaLength := uint64(len(dense.keys)) * geoBitmapMetaSize
	bitmapDataOffset := indexutil.Align(bitmapMetaOffset+bitmapMetaLength, geoStorageAlignment)
	if err = file.Truncate(int64(bitmapDataOffset)); err != nil {
		return err
	}

	sparseKeysCRC, err := writeS2KeysAt(file, sparseKeysOffset, sparse.keys)
	if err != nil {
		return err
	}
	sparseValuesCRC, err := writeUint64At(file, sparseValuesOffset, sparse.imageIDs)
	if err != nil {
		return err
	}
	bitmapKeysCRC, err := writeS2KeysAt(file, bitmapKeysOffset, dense.keys)
	if err != nil {
		return err
	}

	metas := make([]geoBitmapDiskMeta, len(dense.bitmaps))
	dataHash := crc32.NewIEEE()
	dataLength := uint64(0)
	for index, bitmap := range dense.bitmaps {
		aligned := indexutil.Align(dataLength, 8)
		if padding := aligned - dataLength; padding != 0 {
			zeros := make([]byte, padding)
			if _, err = file.WriteAt(zeros, int64(bitmapDataOffset+dataLength)); err != nil {
				return err
			}
			_, _ = dataHash.Write(zeros)
			dataLength = aligned
		}
		payload := bitmap.Bytes()
		if len(payload) > int(^uint32(0)) {
			return ErrCorruptStorage
		}
		if _, err = file.WriteAt(payload, int64(bitmapDataOffset+dataLength)); err != nil {
			return err
		}
		_, _ = dataHash.Write(payload)
		metas[index] = geoBitmapDiskMeta{offset: dataLength, length: uint32(len(payload)), cardinality: bitmap.Cardinality()}
		dataLength += uint64(len(payload))
	}
	if aligned := indexutil.Align(dataLength, 8); aligned != dataLength {
		zeros := make([]byte, aligned-dataLength)
		if _, err = file.WriteAt(zeros, int64(bitmapDataOffset+dataLength)); err != nil {
			return err
		}
		_, _ = dataHash.Write(zeros)
		dataLength = aligned
	}
	metaBytes := make([]byte, len(metas)*geoBitmapMetaSize)
	for index, meta := range metas {
		encoded := metaBytes[index*geoBitmapMetaSize : (index+1)*geoBitmapMetaSize]
		binary.LittleEndian.PutUint64(encoded[0:8], meta.offset)
		binary.LittleEndian.PutUint32(encoded[8:12], meta.length)
		binary.LittleEndian.PutUint64(encoded[16:24], meta.cardinality)
	}
	if _, err = file.WriteAt(metaBytes, int64(bitmapMetaOffset)); err != nil {
		return err
	}

	header := make([]byte, geoHeaderSize)
	binary.LittleEndian.PutUint32(header[0:4], geoStorageMagic)
	binary.LittleEndian.PutUint16(header[4:6], geoStorageVersion)
	binary.LittleEndian.PutUint16(header[6:8], geoHeaderSize)
	binary.LittleEndian.PutUint32(header[8:12], view.flags)
	header[12] = GeoS2Level
	binary.LittleEndian.PutUint64(header[16:24], view.indexID)
	binary.LittleEndian.PutUint64(header[24:32], view.generation)
	binary.LittleEndian.PutUint64(header[32:40], storageGeneration)
	binary.LittleEndian.PutUint64(header[40:48], uint64(len(sparse.keys)))
	binary.LittleEndian.PutUint64(header[48:56], uint64(len(dense.keys)))
	binary.LittleEndian.PutUint64(header[56:64], sparseKeysOffset)
	binary.LittleEndian.PutUint64(header[64:72], sparseValuesOffset)
	binary.LittleEndian.PutUint64(header[72:80], bitmapKeysOffset)
	binary.LittleEndian.PutUint64(header[80:88], bitmapMetaOffset)
	binary.LittleEndian.PutUint64(header[88:96], bitmapDataOffset)
	binary.LittleEndian.PutUint64(header[96:104], dataLength)
	binary.LittleEndian.PutUint32(header[104:108], sparseKeysCRC)
	binary.LittleEndian.PutUint32(header[108:112], sparseValuesCRC)
	binary.LittleEndian.PutUint32(header[112:116], bitmapKeysCRC)
	binary.LittleEndian.PutUint32(header[116:120], crc32.ChecksumIEEE(metaBytes))
	binary.LittleEndian.PutUint32(header[120:124], dataHash.Sum32())
	binary.LittleEndian.PutUint32(header[124:128], crc32.ChecksumIEEE(header[:124]))
	if _, err = file.WriteAt(header, 0); err != nil {
		return err
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

func readGeoGeneration(path string, config Config) (*Index, uint64, error) {
	mapping, err := persist.MapFile(path, ErrCorruptStorage)
	if err != nil {
		return nil, 0, err
	}
	fail := func(err error) (*Index, uint64, error) {
		_ = persist.Unmap(mapping)
		return nil, 0, err
	}
	if len(mapping) < geoHeaderSize || len(mapping)%8 != 0 {
		return fail(ErrCorruptStorage)
	}
	header := mapping[:geoHeaderSize]
	if binary.LittleEndian.Uint32(header[0:4]) != geoStorageMagic ||
		binary.LittleEndian.Uint16(header[4:6]) != geoStorageVersion ||
		binary.LittleEndian.Uint16(header[6:8]) != geoHeaderSize || header[12] != GeoS2Level ||
		binary.LittleEndian.Uint32(header[124:128]) != crc32.ChecksumIEEE(header[:124]) {
		return fail(ErrCorruptStorage)
	}
	sparseCount := binary.LittleEndian.Uint64(header[40:48])
	bitmapCount := binary.LittleEndian.Uint64(header[48:56])
	sparseKeysOffset := binary.LittleEndian.Uint64(header[56:64])
	sparseValuesOffset := binary.LittleEndian.Uint64(header[64:72])
	bitmapKeysOffset := binary.LittleEndian.Uint64(header[72:80])
	bitmapMetaOffset := binary.LittleEndian.Uint64(header[80:88])
	bitmapDataOffset := binary.LittleEndian.Uint64(header[88:96])
	bitmapDataLength := binary.LittleEndian.Uint64(header[96:104])
	if sparseKeysOffset%8 != 0 || sparseValuesOffset%8 != 0 || bitmapKeysOffset%8 != 0 ||
		bitmapMetaOffset%8 != 0 || bitmapDataOffset%8 != 0 || bitmapDataLength%8 != 0 {
		return fail(ErrCorruptStorage)
	}
	sparseKeysEnd, ok := indexutil.CheckedEnd(sparseKeysOffset, sparseCount, 8, uint64(len(mapping)))
	if !ok {
		return fail(ErrCorruptStorage)
	}
	sparseValuesEnd, ok := indexutil.CheckedEnd(sparseValuesOffset, sparseCount, geoSparseSlots*8, uint64(len(mapping)))
	if !ok {
		return fail(ErrCorruptStorage)
	}
	bitmapKeysEnd, ok := indexutil.CheckedEnd(bitmapKeysOffset, bitmapCount, 8, uint64(len(mapping)))
	if !ok {
		return fail(ErrCorruptStorage)
	}
	bitmapMetaEnd, ok := indexutil.CheckedEnd(bitmapMetaOffset, bitmapCount, geoBitmapMetaSize, uint64(len(mapping)))
	if !ok || bitmapDataOffset+bitmapDataLength < bitmapDataOffset || bitmapDataOffset+bitmapDataLength > uint64(len(mapping)) {
		return fail(ErrCorruptStorage)
	}
	sparseKeyBytes := mapping[sparseKeysOffset:sparseKeysEnd]
	sparseValueBytes := mapping[sparseValuesOffset:sparseValuesEnd]
	bitmapKeyBytes := mapping[bitmapKeysOffset:bitmapKeysEnd]
	bitmapMetaBytes := mapping[bitmapMetaOffset:bitmapMetaEnd]
	bitmapData := mapping[bitmapDataOffset : bitmapDataOffset+bitmapDataLength]
	if crc32.ChecksumIEEE(sparseKeyBytes) != binary.LittleEndian.Uint32(header[104:108]) ||
		crc32.ChecksumIEEE(sparseValueBytes) != binary.LittleEndian.Uint32(header[108:112]) ||
		crc32.ChecksumIEEE(bitmapKeyBytes) != binary.LittleEndian.Uint32(header[112:116]) ||
		crc32.ChecksumIEEE(bitmapMetaBytes) != binary.LittleEndian.Uint32(header[116:120]) ||
		crc32.ChecksumIEEE(bitmapData) != binary.LittleEndian.Uint32(header[120:124]) {
		return fail(ErrCorruptStorage)
	}
	sparseKeys, err := persist.ViewFull[S2Key](sparseKeyBytes, ErrCorruptStorage, ErrUnsupportedEndian)
	if err != nil {
		return fail(err)
	}
	sparseValues, err := persist.ViewFull[uint64](sparseValueBytes, ErrCorruptStorage, ErrUnsupportedEndian)
	if err != nil {
		return fail(err)
	}
	if err = validateSparse(sparseKeys, sparseValues); err != nil {
		return fail(err)
	}
	bitmapKeys, err := persist.ViewFull[S2Key](bitmapKeyBytes, ErrCorruptStorage, ErrUnsupportedEndian)
	if err != nil {
		return fail(err)
	}
	bitmaps := make([]idset.Ref, bitmapCount)
	for index := range bitmaps {
		encoded := bitmapMetaBytes[index*geoBitmapMetaSize : (index+1)*geoBitmapMetaSize]
		offset := binary.LittleEndian.Uint64(encoded[0:8])
		length := binary.LittleEndian.Uint32(encoded[8:12])
		cardinality := binary.LittleEndian.Uint64(encoded[16:24])
		end := offset + uint64(length)
		if end < offset || end > uint64(len(bitmapData)) || offset%8 != 0 || cardinality <= geoSparseSlots {
			return fail(ErrCorruptStorage)
		}
		bitmap, bitmapErr := safeBitmapFromBuffer(bitmapData[offset:end])
		if bitmapErr != nil || bitmap.Cardinality() != cardinality {
			return fail(ErrCorruptStorage)
		}
		bitmaps[index] = bitmap
	}
	dense := bitmapRef{keys: bitmapKeys, bitmaps: bitmaps, backing: mapping}
	if err = validateBitmap(dense); err != nil {
		return fail(err)
	}
	if partitionsOverlap(sparseKeys, bitmapKeys) {
		return fail(ErrCorruptStorage)
	}
	sparse := sparseRef{keys: sparseKeys, imageIDs: sparseValues, backing: mapping}
	view := shardView(
		binary.LittleEndian.Uint64(header[16:24]),
		binary.LittleEndian.Uint32(header[8:12]),
		binary.LittleEndian.Uint64(header[24:32]),
		sparse,
		dense,
	)
	index, err := newIndexFromView(view, config, mapping)
	if err != nil {
		return fail(err)
	}
	return index, binary.LittleEndian.Uint64(header[32:40]), nil
}

func validateBitmap(ref bitmapRef) error {
	for index, key := range ref.keys {
		if index != 0 && key <= ref.keys[index-1] {
			return ErrCorruptStorage
		}
		if _, err := CellIDFromS2Key(key); err != nil {
			return err
		}
	}
	return nil
}

func flattenView(view *indexView) (sparseRef, bitmapRef) {
	sparseKeys := make([]S2Key, 0, view.sparseCellCount)
	sparseIDs := make([]uint64, 0, view.sparseCellCount*geoSparseSlots)
	denseKeys := make([]S2Key, 0, view.denseCellCount)
	denseBitmaps := make([]idset.Ref, 0, view.denseCellCount)
	for _, shard := range view.shards {
		if shard == nil {
			continue
		}
		sparseKeys = append(sparseKeys, shard.sparse.keys...)
		sparseIDs = append(sparseIDs, shard.sparse.imageIDs...)
		denseKeys = append(denseKeys, shard.dense.keys...)
		denseBitmaps = append(denseBitmaps, shard.dense.bitmaps...)
	}
	return sparseRef{keys: sparseKeys, imageIDs: sparseIDs}, bitmapRef{keys: denseKeys, bitmaps: denseBitmaps}
}

func shardView(indexID uint64, flags uint32, generation uint64, sparse sparseRef, dense bitmapRef) *indexView {
	view := &indexView{
		version: geoFormatVersion, flags: flags, indexID: indexID, generation: generation,
		sparseCellCount: uint64(len(sparse.keys)), denseCellCount: uint64(len(dense.keys)),
	}
	sparseStart, denseStart := 0, 0
	for shardIndex := range view.shards {
		sparseEnd := sparseStart
		for sparseEnd < len(sparse.keys) && shardForKey(sparse.keys[sparseEnd]) == shardIndex {
			sparseEnd++
		}
		denseEnd := denseStart
		for denseEnd < len(dense.keys) && shardForKey(dense.keys[denseEnd]) == shardIndex {
			denseEnd++
		}
		if sparseStart != sparseEnd || denseStart != denseEnd {
			view.shards[shardIndex] = &geoShard{
				sparse: sparseRef{
					keys:     sparse.keys[sparseStart:sparseEnd],
					imageIDs: sparse.imageIDs[sparseStart*geoSparseSlots : sparseEnd*geoSparseSlots],
					backing:  sparse.backing,
				},
				dense: bitmapRef{
					keys:    dense.keys[denseStart:denseEnd],
					bitmaps: dense.bitmaps[denseStart:denseEnd],
					backing: dense.backing,
				},
			}
		}
		sparseStart, denseStart = sparseEnd, denseEnd
	}
	return view
}

func partitionsOverlap(a, b []S2Key) bool {
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		if a[i] == b[j] {
			return true
		}
		if a[i] < b[j] {
			i++
		} else {
			j++
		}
	}
	return false
}

func safeBitmapFromBuffer(data []byte) (idset.Ref, error) {
	result, err := idset.FromBuffer(data)
	if err != nil {
		return idset.Ref{}, ErrCorruptStorage
	}
	return result, nil
}

func writeS2KeysAt(file *os.File, offset uint64, keys []S2Key) (uint32, error) {
	hasher := crc32.NewIEEE()
	buffer := make([]byte, 64*1024)
	written := uint64(0)
	for len(keys) != 0 {
		count := min(len(keys), len(buffer)/8)
		for index := 0; index < count; index++ {
			binary.LittleEndian.PutUint64(buffer[index*8:(index+1)*8], uint64(keys[index]))
		}
		chunk := buffer[:count*8]
		if _, err := file.WriteAt(chunk, int64(offset+written)); err != nil {
			return 0, err
		}
		_, _ = hasher.Write(chunk)
		written += uint64(len(chunk))
		keys = keys[count:]
	}
	return hasher.Sum32(), nil
}

func writeUint64At(file *os.File, offset uint64, values []uint64) (uint32, error) {
	hasher := crc32.NewIEEE()
	buffer := make([]byte, 64*1024)
	written := uint64(0)
	for len(values) != 0 {
		count := min(len(values), len(buffer)/8)
		for index := 0; index < count; index++ {
			binary.LittleEndian.PutUint64(buffer[index*8:(index+1)*8], values[index])
		}
		chunk := buffer[:count*8]
		if _, err := file.WriteAt(chunk, int64(offset+written)); err != nil {
			return 0, err
		}
		_, _ = hasher.Write(chunk)
		written += uint64(len(chunk))
		values = values[count:]
	}
	return hasher.Sum32(), nil
}

func geoGenerationPath(dir string, generation uint64) string {
	return filepath.Join(dir, fmt.Sprintf("geo-%020d.base", generation))
}
