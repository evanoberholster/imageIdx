package timestamp

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"sync"

	"github.com/evanoberholster/imageIdx/internal/indexutil"
)

const (
	manifestMagic      uint32 = 0x54534d46 // TSMF
	manifestVersion    uint16 = 2
	manifestSize              = 80
	directoryEntrySize        = 64
	storageAlignment          = 4_096
	formatAlignment           = 8
	walFilename               = "timestamp.wal"
)

var ErrCorruptManifest = errors.New("timestamp: corrupt manifest")

type diskManifest struct {
	generation uint64
	indexID    uint64
	count      uint64
	startTime  uint64
	endTime    uint64
	offsetTime uint64
	walOffset  int64
	blockCount uint64
	flags      uint32
}

type persistentState struct {
	mu         sync.Mutex
	dir        string
	durability Durability
	wal        *timestampWAL
	mappings   [][]byte
}

// NewPersistent creates a persistent index and publishes its initial
// checkpoint.
func NewPersistent(storeDir string, spec Spec, config Config, durability Durability) (*Index, error) {
	if durability > DurabilitySync {
		return nil, ErrInvalidConfig
	}
	if err := os.MkdirAll(storeDir, 0o755); err != nil {
		return nil, err
	}
	index, err := New(spec, config)
	if err != nil {
		return nil, err
	}
	wal, err := openTimestampWAL(filepath.Join(storeDir, walFilename), config.DeltaCapacity)
	if err != nil {
		_ = index.Close()
		return nil, err
	}
	index.persistence = &persistentState{dir: storeDir, durability: durability, wal: wal}
	if err = index.Checkpoint(); err != nil {
		_ = index.Close()
		return nil, err
	}
	return index, nil
}

// OpenPersistent maps the published base generation and replays the WAL tail.
func OpenPersistent(storeDir string, config Config, durability Durability) (*Index, error) {
	if durability > DurabilitySync {
		return nil, ErrInvalidConfig
	}
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
	if len(markersMapping)%formatAlignment != 0 || len(idsMapping)%formatAlignment != 0 {
		_ = unmapFile(markersMapping)
		_ = unmapFile(idsMapping)
		return nil, ErrCorruptManifest
	}
	directoryBytes, err := os.ReadFile(blocksPath)
	if err != nil {
		_ = unmapFile(markersMapping)
		_ = unmapFile(idsMapping)
		return nil, err
	}
	if len(directoryBytes)%formatAlignment != 0 {
		_ = unmapFile(markersMapping)
		_ = unmapFile(idsMapping)
		return nil, ErrCorruptManifest
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
		indexID:       manifest.indexID,
		flags:         manifest.flags,
		offset:        manifest.offsetTime,
		startTime:     manifest.startTime,
		endTime:       manifest.endTime,
		config:        config,
		writes:        make(chan *Batch, config.WriteQueueCapacity),
		barriers:      make(chan chan barrierResult),
		compactions:   make(chan compactionTask, max(config.CompactionQueue, len(directory.entries))),
		compactDone:   make(chan compactionResult, max(config.CompactionQueue, len(directory.entries))),
		stop:          make(chan struct{}),
		writerNeeds:   make([]uint32, len(directory.entries)),
		writerTouched: make([]int, 0, 256),
		persistence: &persistentState{
			dir: storeDir, durability: durability,
			mappings: [][]byte{markersMapping, idsMapping},
		},
	}
	index.generation.Store(manifest.generation)
	index.baseCount.Store(manifest.count)
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
		batch := NewBatch(min(len(recovered), config.DeltaCapacity))
		for start := 0; start < len(recovered); {
			end := min(start+config.DeltaCapacity, len(recovered))
			batch.writes = append(batch.writes[:0], recovered[start:end]...)
			batch.mutations = end - start
			for {
				err = index.Apply(batch)
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
	index.persistence.wal = wal
	return index, nil
}

// Checkpoint writes and atomically publishes a new base generation.
func (i *Index) Checkpoint() error {
	i.lifecycle.RLock()
	defer i.lifecycle.RUnlock()
	if i.persistence == nil || i.persistence.wal == nil {
		return ErrNotPersistent
	}
	if i.closed.Load() {
		return ErrClosed
	}
	i.persistence.mu.Lock()
	defer i.persistence.mu.Unlock()

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
	generation := i.generation.Load() + 1
	markersPath, idsPath, blocksPath := generationPaths(i.persistence.dir, generation)
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

	token := i.readers.Acquire()
	directory := i.directory.Load()
	diskEntries := make([]blockEntry, len(directory.entries))
	var totalCount uint64
	for index := range directory.entries {
		entry := &directory.entries[index]
		base := snapshotBase(entry.block.view.Load())
		markerOffset, alignErr := alignFile(markerFile, storageAlignment)
		if alignErr != nil {
			err = alignErr
			break
		}
		idOffset, alignErr := alignFile(idFile, storageAlignment)
		if alignErr != nil {
			err = alignErr
			break
		}
		if err = writeMarkers(markerFile, base.markers); err != nil {
			break
		}
		if err = writeImageIDs(idFile, base.imageIDs); err != nil {
			break
		}
		if _, err = alignFile(markerFile, formatAlignment); err != nil {
			break
		}
		if _, err = alignFile(idFile, formatAlignment); err != nil {
			break
		}
		diskEntries[index] = *entry
		diskEntries[index].meta.MarkerOffset = uint64(markerOffset)
		diskEntries[index].meta.IDOffset = uint64(idOffset)
		diskEntries[index].meta.Count = uint32(base.len())
		diskEntries[index].meta.Generation = generation
		totalCount += uint64(base.len())
	}
	i.readers.Release(token)
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
		indexID:    i.indexID,
		count:      totalCount,
		startTime:  i.startTime,
		endTime:    i.endTime,
		offsetTime: i.offset,
		walOffset:  barrier.walOffset,
		blockCount: uint64(len(diskEntries)),
		flags:      i.flags,
	}
	if err = writeManifestAtomic(i.persistence.dir, manifest); err != nil {
		return err
	}
	i.generation.Store(generation)
	i.baseCount.Store(totalCount)
	return nil
}

// Vacuum currently publishes a checkpoint; generation retention remains a
// storage-owner policy.
func (i *Index) Vacuum() error { return i.Checkpoint() }

func snapshotBase(view *blockView) *base {
	for {
		version := view.active.stableVersion()
		base := view.base
		if view.frozen != nil && view.frozen.len() != 0 {
			base = compact(base, view.frozen)
		}
		if view.active.len() != 0 {
			base = compact(base, view.active)
		}
		if view.active.version.Load() == version {
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

func alignFile(file *os.File, alignment uint64) (int64, error) {
	offset, err := file.Seek(0, io.SeekCurrent)
	if err != nil {
		return 0, err
	}
	aligned := int64(indexutil.Align(uint64(offset), alignment))
	if padding := aligned - offset; padding != 0 {
		zeros := make([]byte, padding)
		if _, err = file.Write(zeros); err != nil {
			return 0, err
		}
	}
	return aligned, nil
}

func writeMarkers(file *os.File, markers []timemarker) error {
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
		meta := blockMetadata{
			StartTime:    binary.LittleEndian.Uint64(encoded[16:24]),
			EndTime:      binary.LittleEndian.Uint64(encoded[24:32]),
			MarkerOffset: binary.LittleEndian.Uint64(encoded[32:40]),
			IDOffset:     binary.LittleEndian.Uint64(encoded[40:48]),
			Count:        binary.LittleEndian.Uint32(encoded[48:52]),
			Flags:        binary.LittleEndian.Uint32(encoded[52:56]),
			Generation:   binary.LittleEndian.Uint64(encoded[56:64]),
		}
		if meta.MarkerOffset%formatAlignment != 0 || meta.IDOffset%formatAlignment != 0 {
			return nil, ErrCorruptManifest
		}
		markers, err := mappedMarkers(markerData, meta.MarkerOffset, meta.Count)
		if err != nil {
			return nil, err
		}
		ids, err := mappedImageIDs(idData, meta.IDOffset, meta.Count)
		if err != nil {
			return nil, err
		}
		base, err := newBase(markers, ids)
		if err != nil {
			return nil, err
		}
		entries[index] = blockEntry{
			startMarker: binary.LittleEndian.Uint64(encoded[0:8]),
			endMarker:   binary.LittleEndian.Uint64(encoded[8:16]),
			meta:        meta,
			block:       newBlock(base, config.DeltaCapacity),
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
	return indexutil.WriteFileAtomicOnce(dir, "manifest.tmp", "manifest", data, 0o644)
}

func encodeManifest(manifest diskManifest) []byte {
	data := make([]byte, manifestSize)
	binary.LittleEndian.PutUint32(data[0:4], manifestMagic)
	binary.LittleEndian.PutUint16(data[4:6], manifestVersion)
	data[6] = TimestampShift
	binary.LittleEndian.PutUint64(data[8:16], manifest.generation)
	binary.LittleEndian.PutUint64(data[16:24], manifest.indexID)
	binary.LittleEndian.PutUint64(data[24:32], manifest.count)
	binary.LittleEndian.PutUint64(data[32:40], manifest.startTime)
	binary.LittleEndian.PutUint64(data[40:48], manifest.endTime)
	binary.LittleEndian.PutUint64(data[48:56], manifest.offsetTime)
	binary.LittleEndian.PutUint64(data[56:64], uint64(manifest.walOffset))
	binary.LittleEndian.PutUint64(data[64:72], manifest.blockCount)
	binary.LittleEndian.PutUint32(data[72:76], manifest.flags)
	binary.LittleEndian.PutUint32(data[76:80], crc32.ChecksumIEEE(data[:76]))
	return data
}

func readManifest(path string) (diskManifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return diskManifest{}, err
	}
	if len(data) != manifestSize || binary.LittleEndian.Uint32(data[0:4]) != manifestMagic ||
		binary.LittleEndian.Uint16(data[4:6]) != manifestVersion || data[6] != TimestampShift ||
		binary.LittleEndian.Uint32(data[76:80]) != crc32.ChecksumIEEE(data[:76]) {
		return diskManifest{}, ErrCorruptManifest
	}
	return diskManifest{
		generation: binary.LittleEndian.Uint64(data[8:16]),
		indexID:    binary.LittleEndian.Uint64(data[16:24]),
		count:      binary.LittleEndian.Uint64(data[24:32]),
		startTime:  binary.LittleEndian.Uint64(data[32:40]),
		endTime:    binary.LittleEndian.Uint64(data[40:48]),
		offsetTime: binary.LittleEndian.Uint64(data[48:56]),
		walOffset:  int64(binary.LittleEndian.Uint64(data[56:64])),
		blockCount: binary.LittleEndian.Uint64(data[64:72]),
		flags:      binary.LittleEndian.Uint32(data[72:76]),
	}, nil
}
