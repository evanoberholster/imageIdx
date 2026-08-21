package geoindex

import (
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"os"
	"path/filepath"

	"github.com/evanoberholster/imageIdx/bitmapidx"
)

func writeGeoGeneration(path string, storageGeneration uint64, view *GeoIndexView) error {
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

	sparseKeysOffset := uint64(geoHeaderSize)
	sparseKeysLength := uint64(len(view.Sparse.keys)) * 8
	sparseValuesOffset := align(sparseKeysOffset+sparseKeysLength, 8)
	sparseValuesLength := uint64(len(view.Sparse.imageIDs)) * 8
	bitmapKeysOffset := align(sparseValuesOffset+sparseValuesLength, 8)
	bitmapKeysLength := uint64(len(view.Bitmap.keys)) * 8
	bitmapMetaOffset := align(bitmapKeysOffset+bitmapKeysLength, 8)
	bitmapMetaLength := uint64(len(view.Bitmap.keys)) * geoBitmapMetaSize
	bitmapDataOffset := align(bitmapMetaOffset+bitmapMetaLength, geoStorageAlignment)
	if err = file.Truncate(int64(bitmapDataOffset)); err != nil {
		return err
	}

	sparseKeysCRC, err := writeS2KeysAt(file, sparseKeysOffset, view.Sparse.keys)
	if err != nil {
		return err
	}
	sparseValuesCRC, err := writeUint64At(file, sparseValuesOffset, view.Sparse.imageIDs)
	if err != nil {
		return err
	}
	bitmapKeysCRC, err := writeS2KeysAt(file, bitmapKeysOffset, view.Bitmap.keys)
	if err != nil {
		return err
	}

	metas := make([]geoBitmapDiskMeta, len(view.Bitmap.bitmaps))
	dataHash := crc32.NewIEEE()
	dataLength := uint64(0)
	for index, bitmap := range view.Bitmap.bitmaps {
		aligned := align(dataLength, 8)
		if padding := aligned - dataLength; padding != 0 {
			zeros := make([]byte, padding)
			if _, err = file.WriteAt(zeros, int64(bitmapDataOffset+dataLength)); err != nil {
				return err
			}
			_, _ = dataHash.Write(zeros)
			dataLength = aligned
		}
		payload := bitmap.MarshalBinary()
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
	binary.LittleEndian.PutUint32(header[8:12], view.Flags)
	header[12] = GeoS2Level
	binary.LittleEndian.PutUint64(header[16:24], view.IndexID)
	binary.LittleEndian.PutUint64(header[24:32], view.Generation)
	binary.LittleEndian.PutUint64(header[32:40], storageGeneration)
	binary.LittleEndian.PutUint64(header[40:48], uint64(len(view.Sparse.keys)))
	binary.LittleEndian.PutUint64(header[48:56], uint64(len(view.Bitmap.keys)))
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

func readGeoGeneration(path string, config Config) (*GeoIndex, uint64, error) {
	mapping, err := mmapFile(path)
	if err != nil {
		return nil, 0, err
	}
	fail := func(err error) (*GeoIndex, uint64, error) {
		_ = unmapFile(mapping)
		return nil, 0, err
	}
	if len(mapping) < geoHeaderSize {
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
	sparseKeysEnd, ok := checkedEnd(sparseKeysOffset, sparseCount, 8, uint64(len(mapping)))
	if !ok {
		return fail(ErrCorruptStorage)
	}
	sparseValuesEnd, ok := checkedEnd(sparseValuesOffset, sparseCount, GeoSparseSlots*8, uint64(len(mapping)))
	if !ok {
		return fail(ErrCorruptStorage)
	}
	bitmapKeysEnd, ok := checkedEnd(bitmapKeysOffset, bitmapCount, 8, uint64(len(mapping)))
	if !ok {
		return fail(ErrCorruptStorage)
	}
	bitmapMetaEnd, ok := checkedEnd(bitmapMetaOffset, bitmapCount, geoBitmapMetaSize, uint64(len(mapping)))
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
	sparseKeys, err := mappedS2Keys(sparseKeyBytes)
	if err != nil {
		return fail(err)
	}
	sparseValues, err := mappedUint64(sparseValueBytes)
	if err != nil {
		return fail(err)
	}
	if err = validateSparse(sparseKeys, sparseValues); err != nil {
		return fail(err)
	}
	bitmapKeys, err := mappedS2Keys(bitmapKeyBytes)
	if err != nil {
		return fail(err)
	}
	bitmaps := make([]bitmapidx.BitmapRef, bitmapCount)
	for index := range bitmaps {
		encoded := bitmapMetaBytes[index*geoBitmapMetaSize : (index+1)*geoBitmapMetaSize]
		offset := binary.LittleEndian.Uint64(encoded[0:8])
		length := binary.LittleEndian.Uint32(encoded[8:12])
		cardinality := binary.LittleEndian.Uint64(encoded[16:24])
		end := offset + uint64(length)
		if end < offset || end > uint64(len(bitmapData)) || offset%8 != 0 || cardinality <= GeoSparseSlots {
			return fail(ErrCorruptStorage)
		}
		bitmap, bitmapErr := safeBitmapFromBuffer(bitmapData[offset:end])
		if bitmapErr != nil || bitmap.Cardinality() != cardinality {
			return fail(ErrCorruptStorage)
		}
		bitmaps[index] = bitmap
	}
	bitmapRef := GeoBitmapRef{keys: bitmapKeys, bitmaps: bitmaps, backing: mapping}
	if err = validateBitmapRef(bitmapRef); err != nil {
		return fail(err)
	}
	if partitionsOverlap(sparseKeys, bitmapKeys) {
		return fail(ErrCorruptStorage)
	}
	view := &GeoIndexView{
		Version: GeoFormatVersion, Flags: binary.LittleEndian.Uint32(header[8:12]),
		IndexID: binary.LittleEndian.Uint64(header[16:24]), Generation: binary.LittleEndian.Uint64(header[24:32]),
		Sparse: GeoSparseRef{keys: sparseKeys, imageIDs: sparseValues, backing: mapping}, Bitmap: bitmapRef,
	}
	index, err := newGeoIndexFromView(view, config, mapping)
	if err != nil {
		return fail(err)
	}
	expectedIDs := len(sparseKeys) * GeoSparseSlots
	for _, id := range sparseValues {
		if id == 0 {
			expectedIDs--
		}
	}
	for _, bitmap := range bitmaps {
		expectedIDs += int(bitmap.Cardinality())
	}
	if len(index.locations) != expectedIDs {
		_ = index.Close()
		return nil, 0, ErrCorruptStorage
	}
	return index, binary.LittleEndian.Uint64(header[32:40]), nil
}

func validateBitmapRef(ref GeoBitmapRef) error {
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

func safeBitmapFromBuffer(data []byte) (result bitmapidx.BitmapRef, err error) {
	defer func() {
		if recover() != nil {
			err = ErrCorruptStorage
		}
	}()
	result = bitmapidx.BitmapRefFromBuffer(data)
	_ = result.Cardinality()
	return result, nil
}

func checkedEnd(offset, count, stride, limit uint64) (uint64, bool) {
	if count != 0 && stride > (^uint64(0)-offset)/count {
		return 0, false
	}
	end := offset + count*stride
	return end, end >= offset && end <= limit
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

func align(value, alignment uint64) uint64 { return (value + alignment - 1) &^ (alignment - 1) }

func geoGenerationPath(dir string, generation uint64) string {
	return filepath.Join(dir, fmt.Sprintf("geo-%020d.base", generation))
}
