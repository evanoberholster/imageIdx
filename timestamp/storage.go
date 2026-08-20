package timestamp

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
)

const (
	manifestMagic      uint32 = 0x54534d46 // TSMF
	manifestVersion    uint16 = 1
	manifestSize              = 72
	directoryEntrySize        = 64
	storageAlignment          = 4_096
	walFilename               = "timestamp.wal"
)

var ErrCorruptManifest = errors.New("timestamp: corrupt manifest")

type diskManifest struct {
	generation uint64
	count      uint64
	startTime  uint64
	endTime    uint64
	offsetTime uint64
	walOffset  int64
	blockCount uint64
	flags      uint32
}

func NewPersistent(storeDir string, records []Record, offset uint64, config Config) (*Index, error) {
	if err := os.MkdirAll(storeDir, 0o755); err != nil {
		return nil, err
	}
	index, err := New(records, offset, config)
	if err != nil {
		return nil, err
	}
	wal, err := openTimestampWAL(filepath.Join(storeDir, walFilename), config.DeltaCapacity)
	if err != nil {
		_ = index.Close()
		return nil, err
	}
	index.storeDir = storeDir
	index.wal = wal
	if err = index.Checkpoint(); err != nil {
		_ = index.Close()
		return nil, err
	}
	return index, nil
}

func OpenPersistent(storeDir string, config Config) (*Index, error) {
	config, err := config.withDefaults()
	if err != nil {
		return nil, err
	}
	manifest, err := readManifest(filepath.Join(storeDir, "manifest"))
	if err != nil {
		return nil, err
	}
	markersPath, idsPath, blocksPath := generationPaths(storeDir, manifest.generation)
	markersMapping, err := mmapFile(markersPath)
	if err != nil {
		return nil, err
	}
	idsMapping, err := mmapFile(idsPath)
	if err != nil {
		_ = unmapFile(markersMapping)
		return nil, err
	}
	directoryBytes, err := os.ReadFile(blocksPath)
	if err != nil {
		_ = unmapFile(markersMapping)
		_ = unmapFile(idsMapping)
		return nil, err
	}
	directory, err := decodeDirectory(directoryBytes, markersMapping, idsMapping, config)
	if err != nil {
		_ = unmapFile(markersMapping)
		_ = unmapFile(idsMapping)
		return nil, err
	}
	if uint64(len(directory.entries)) != manifest.blockCount {
		_ = unmapFile(markersMapping)
		_ = unmapFile(idsMapping)
		return nil, ErrCorruptManifest
	}
	var persistedCount uint64
	for _, entry := range directory.entries {
		if entry.meta.Generation != manifest.generation {
			_ = unmapFile(markersMapping)
			_ = unmapFile(idsMapping)
			return nil, ErrCorruptManifest
		}
		persistedCount += uint64(entry.meta.Count)
	}
	if persistedCount != manifest.count || directory.entries[0].startMarker != 0 ||
		directory.entries[len(directory.entries)-1].endMarker != markerSpaceEnd {
		_ = unmapFile(markersMapping)
		_ = unmapFile(idsMapping)
		return nil, ErrCorruptManifest
	}

	index := &Index{
		Header: TimestampIndexHeader{
			Magic:      TimestampIndexMagic,
			Version:    TimestampIndexVersion,
			Flags:      uint16(manifest.flags),
			Count:      manifest.count,
			StartTime:  manifest.startTime,
			EndTime:    manifest.endTime,
			OffsetTime: manifest.offsetTime,
			Shift:      TimestampShift,
			Generation: manifest.generation,
		},
		offset:        manifest.offsetTime,
		config:        config,
		writes:        make(chan *TimestampWriteBatch, config.WriteQueueCapacity),
		barriers:      make(chan chan barrierResult),
		compactions:   make(chan compactionTask, max(config.CompactionQueue, len(directory.entries))),
		compactDone:   make(chan compactionResult, max(config.CompactionQueue, len(directory.entries))),
		stop:          make(chan struct{}),
		writerNeeds:   make([]uint32, len(directory.entries)),
		writerTouched: make([]int, 0, 256),
		storeDir:      storeDir,
		mappings:      [][]byte{markersMapping, idsMapping},
	}
	index.directory.Store(directory)
	index.workers.Add(2)
	go index.writerLoop()
	go index.compactorLoop()

	recovered, err := replayWAL(filepath.Join(storeDir, walFilename), manifest.walOffset)
	if err != nil {
		_ = index.Close()
		return nil, err
	}
	if len(recovered) != 0 {
		batch := NewWriteBatch(min(len(recovered), config.DeltaCapacity))
		for start := 0; start < len(recovered); {
			end := min(start+config.DeltaCapacity, len(recovered))
			batch.Writes = append(batch.Writes[:0], recovered[start:end]...)
			for {
				err = index.Submit(batch)
				if err != ErrBackpressure {
					break
				}
			}
			if err != nil {
				_ = index.Close()
				return nil, err
			}
			start = end
		}
	}
	wal, err := openTimestampWAL(filepath.Join(storeDir, walFilename), config.DeltaCapacity)
	if err != nil {
		_ = index.Close()
		return nil, err
	}
	index.wal = wal
	return index, nil
}

func (i *Index) Checkpoint() error {
	i.lifecycle.RLock()
	defer i.lifecycle.RUnlock()
	if i.storeDir == "" || i.wal == nil {
		return errors.New("timestamp: index is not persistent")
	}
	if i.closed.Load() {
		return ErrClosed
	}
	i.persistenceMu.Lock()
	defer i.persistenceMu.Unlock()

	response := make(chan barrierResult, 1)
	select {
	case i.barriers <- response:
	case <-i.stop:
		return ErrClosed
	}
	barrier := <-response
	if barrier.err != nil {
		return barrier.err
	}
	generation := i.Header.Generation + 1
	markersPath, idsPath, blocksPath := generationPaths(i.storeDir, generation)
	markerTemp, idTemp, blockTemp := markersPath+".tmp", idsPath+".tmp", blocksPath+".tmp"
	markerFile, err := os.OpenFile(markerTemp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	idFile, err := os.OpenFile(idTemp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		_ = markerFile.Close()
		return err
	}

	epoch := i.epoch.enter()
	directory := i.directory.Load()
	diskEntries := make([]blockEntry, len(directory.entries))
	var totalCount uint64
	for index := range directory.entries {
		entry := &directory.entries[index]
		base := snapshotBase(entry.block.view.Load())
		markerOffset, alignErr := alignFile(markerFile)
		if alignErr != nil {
			err = alignErr
			break
		}
		idOffset, alignErr := alignFile(idFile)
		if alignErr != nil {
			err = alignErr
			break
		}
		if err = writeMarkers(markerFile, base.Markers); err != nil {
			break
		}
		if err = writeImageIDs(idFile, base.ImageIDs); err != nil {
			break
		}
		diskEntries[index] = *entry
		diskEntries[index].meta.MarkerOffset = uint64(markerOffset)
		diskEntries[index].meta.IDOffset = uint64(idOffset)
		diskEntries[index].meta.Count = uint32(base.Len())
		diskEntries[index].meta.Generation = generation
		totalCount += uint64(base.Len())
	}
	i.epoch.exit(epoch)
	if err == nil {
		err = markerFile.Sync()
	}
	if closeErr := markerFile.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = idFile.Sync()
	}
	if closeErr := idFile.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err = writeDirectory(blockTemp, diskEntries); err != nil {
		return err
	}
	if err = os.Rename(markerTemp, markersPath); err != nil {
		return err
	}
	if err = os.Rename(idTemp, idsPath); err != nil {
		return err
	}
	if err = os.Rename(blockTemp, blocksPath); err != nil {
		return err
	}
	manifest := diskManifest{
		generation: generation,
		count:      totalCount,
		startTime:  i.Header.StartTime,
		endTime:    i.Header.EndTime,
		offsetTime: i.offset,
		walOffset:  barrier.walOffset,
		blockCount: uint64(len(diskEntries)),
		flags:      uint32(i.Header.Flags),
	}
	if err = writeManifestAtomic(i.storeDir, manifest); err != nil {
		return err
	}
	i.Header.Generation = generation
	i.Header.Count = totalCount
	return nil
}

func (i *Index) Vacuum() error { return i.Checkpoint() }

func snapshotBase(view *TimestampBlockView) *TimestampBase {
	for {
		version := view.Active.stableVersion()
		base := view.Base
		if view.Frozen != nil && view.Frozen.Len() != 0 {
			base = compact(base, view.Frozen)
		}
		if view.Active.Len() != 0 {
			base = compact(base, view.Active)
		}
		if view.Active.version.Load() == version {
			return base
		}
	}
}

func generationPaths(dir string, generation uint64) (markers, ids, blocks string) {
	suffix := fmt.Sprintf("%020d", generation)
	return filepath.Join(dir, "markers."+suffix+".base"),
		filepath.Join(dir, "ids."+suffix+".base"),
		filepath.Join(dir, "blocks."+suffix+".idx")
}

func alignFile(file *os.File) (int64, error) {
	offset, err := file.Seek(0, io.SeekCurrent)
	if err != nil {
		return 0, err
	}
	aligned := (offset + storageAlignment - 1) &^ (storageAlignment - 1)
	if padding := aligned - offset; padding != 0 {
		zeros := make([]byte, padding)
		if _, err = file.Write(zeros); err != nil {
			return 0, err
		}
	}
	return aligned, nil
}

func writeMarkers(file *os.File, markers []Timemarker) error {
	buffer := make([]byte, 64*1024)
	for len(markers) != 0 {
		count := min(len(markers), len(buffer)/4)
		for index := 0; index < count; index++ {
			binary.LittleEndian.PutUint32(buffer[index*4:index*4+4], uint32(markers[index]))
		}
		if _, err := file.Write(buffer[:count*4]); err != nil {
			return err
		}
		markers = markers[count:]
	}
	return nil
}

func writeImageIDs(file *os.File, ids []ImageID) error {
	buffer := make([]byte, 64*1024)
	for len(ids) != 0 {
		count := min(len(ids), len(buffer)/8)
		for index := 0; index < count; index++ {
			binary.LittleEndian.PutUint64(buffer[index*8:index*8+8], uint64(ids[index]))
		}
		if _, err := file.Write(buffer[:count*8]); err != nil {
			return err
		}
		ids = ids[count:]
	}
	return nil
}

func writeDirectory(path string, entries []blockEntry) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	buffer := make([]byte, directoryEntrySize)
	for _, entry := range entries {
		binary.LittleEndian.PutUint64(buffer[0:8], entry.startMarker)
		binary.LittleEndian.PutUint64(buffer[8:16], entry.endMarker)
		binary.LittleEndian.PutUint64(buffer[16:24], entry.meta.StartTime)
		binary.LittleEndian.PutUint64(buffer[24:32], entry.meta.EndTime)
		binary.LittleEndian.PutUint64(buffer[32:40], entry.meta.MarkerOffset)
		binary.LittleEndian.PutUint64(buffer[40:48], entry.meta.IDOffset)
		binary.LittleEndian.PutUint32(buffer[48:52], entry.meta.Count)
		binary.LittleEndian.PutUint32(buffer[52:56], entry.meta.Flags)
		binary.LittleEndian.PutUint64(buffer[56:64], entry.meta.Generation)
		if _, err = file.Write(buffer); err != nil {
			_ = file.Close()
			return err
		}
	}
	if err = file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

func decodeDirectory(data, markerData, idData []byte, config Config) (*blockDirectory, error) {
	if len(data) == 0 || len(data)%directoryEntrySize != 0 {
		return nil, ErrCorruptManifest
	}
	entries := make([]blockEntry, len(data)/directoryEntrySize)
	for index := range entries {
		position := index * directoryEntrySize
		encoded := data[position : position+directoryEntrySize]
		meta := TimestampBlockMeta{
			StartTime:    binary.LittleEndian.Uint64(encoded[16:24]),
			EndTime:      binary.LittleEndian.Uint64(encoded[24:32]),
			MarkerOffset: binary.LittleEndian.Uint64(encoded[32:40]),
			IDOffset:     binary.LittleEndian.Uint64(encoded[40:48]),
			Count:        binary.LittleEndian.Uint32(encoded[48:52]),
			Flags:        binary.LittleEndian.Uint32(encoded[52:56]),
			Generation:   binary.LittleEndian.Uint64(encoded[56:64]),
		}
		markers, err := mappedMarkers(markerData, meta.MarkerOffset, meta.Count)
		if err != nil {
			return nil, err
		}
		ids, err := mappedImageIDs(idData, meta.IDOffset, meta.Count)
		if err != nil {
			return nil, err
		}
		base, err := NewTimestampBase(markers, ids)
		if err != nil {
			return nil, err
		}
		entries[index] = blockEntry{
			startMarker: binary.LittleEndian.Uint64(encoded[0:8]),
			endMarker:   binary.LittleEndian.Uint64(encoded[8:16]),
			meta:        meta,
			block:       newTimestampBlock(base, config.DeltaCapacity),
		}
		if entries[index].startMarker >= entries[index].endMarker ||
			(index != 0 && entries[index-1].endMarker != entries[index].startMarker) {
			return nil, ErrCorruptManifest
		}
	}
	return &blockDirectory{entries: entries}, nil
}

func writeManifestAtomic(dir string, manifest diskManifest) error {
	data := encodeManifest(manifest)
	temporary := filepath.Join(dir, "manifest.tmp")
	file, err := os.OpenFile(temporary, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err = file.Write(data); err == nil {
		err = file.Sync()
	}
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err = os.Rename(temporary, filepath.Join(dir, "manifest")); err != nil {
		return err
	}
	directory, err := os.Open(dir)
	if err != nil {
		return err
	}
	err = directory.Sync()
	if closeErr := directory.Close(); err == nil {
		err = closeErr
	}
	return err
}

func encodeManifest(manifest diskManifest) []byte {
	data := make([]byte, manifestSize)
	binary.LittleEndian.PutUint32(data[0:4], manifestMagic)
	binary.LittleEndian.PutUint16(data[4:6], manifestVersion)
	data[6] = TimestampShift
	binary.LittleEndian.PutUint64(data[8:16], manifest.generation)
	binary.LittleEndian.PutUint64(data[16:24], manifest.count)
	binary.LittleEndian.PutUint64(data[24:32], manifest.startTime)
	binary.LittleEndian.PutUint64(data[32:40], manifest.endTime)
	binary.LittleEndian.PutUint64(data[40:48], manifest.offsetTime)
	binary.LittleEndian.PutUint64(data[48:56], uint64(manifest.walOffset))
	binary.LittleEndian.PutUint64(data[56:64], manifest.blockCount)
	binary.LittleEndian.PutUint32(data[64:68], manifest.flags)
	binary.LittleEndian.PutUint32(data[68:72], crc32.ChecksumIEEE(data[:68]))
	return data
}

func readManifest(path string) (diskManifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return diskManifest{}, err
	}
	if len(data) != manifestSize || binary.LittleEndian.Uint32(data[0:4]) != manifestMagic ||
		binary.LittleEndian.Uint16(data[4:6]) != manifestVersion || data[6] != TimestampShift ||
		binary.LittleEndian.Uint32(data[68:72]) != crc32.ChecksumIEEE(data[:68]) {
		return diskManifest{}, ErrCorruptManifest
	}
	return diskManifest{
		generation: binary.LittleEndian.Uint64(data[8:16]),
		count:      binary.LittleEndian.Uint64(data[16:24]),
		startTime:  binary.LittleEndian.Uint64(data[24:32]),
		endTime:    binary.LittleEndian.Uint64(data[32:40]),
		offsetTime: binary.LittleEndian.Uint64(data[40:48]),
		walOffset:  int64(binary.LittleEndian.Uint64(data[48:56])),
		blockCount: binary.LittleEndian.Uint64(data[56:64]),
		flags:      binary.LittleEndian.Uint32(data[64:68]),
	}, nil
}
