package timeidx

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sync"

	common "github.com/evanoberholster/imageIdx/common"
	"github.com/evanoberholster/imageIdx/internal/storage"
)

const (
	manifestMagic      uint32 = 0x54534d46 // TSMF
	manifestVersion    uint16 = 3
	manifestSize              = 88
	directoryEntrySize        = 64
	storageAlignment          = 4_096
	formatAlignment           = 8
	walPattern                = "timestamp-%020d.wal"
	walGlob                   = "timestamp-*.wal"
	storeLockName             = ".timeidx.lock"
)

var ErrCorruptManifest = errors.New("timeidx: corrupt manifest")

// walPath names the WAL for one epoch. Epochs make WAL rotation crash-safe:
// the manifest references (epoch, offset), the old file is removed only
// after the new root is durable, and a crash on either side of publication
// leaves a replayable root behind.
func walPath(dir string, epoch uint64) string {
	return filepath.Join(dir, fmt.Sprintf(walPattern, epoch))
}

type diskManifest struct {
	generation uint64
	// namespace occupies the field that held the old uint64 IndexID. It is
	// persisted zero-extended in the same 8 bytes, so no format version bump
	// is needed; decode rejects values above math.MaxUint16 as corruption.
	namespace  common.Namespace
	count      uint64
	startTime  uint64
	endTime    uint64
	offsetTime uint64
	walOffset  int64
	blockCount uint64
	flags      uint32
	walEpoch   uint64
}

type persistentState struct {
	mu         sync.Mutex
	dir        string
	durability Durability
	wal        *timestampWAL
	walEpoch   uint64
	mappings   [][]byte
}

// NewPersistent creates and locks a store directory, then creates a
// persistent index and publishes its initial checkpoint. The directory
// must be empty; a held lock reports ErrStoreBusy.
func NewPersistent(storeDir string, spec Spec, config Config, durability Durability) (*Index, error) {
	if !durability.Valid() {
		return nil, ErrInvalidConfig
	}
	lock, err := storage.AcquireLock(storeDir, storeLockName, true)
	if err != nil {
		return nil, err
	}
	locked := true
	defer func() {
		if locked {
			_ = storage.ReleaseLock(lock)
		}
	}()
	index, err := New(spec, config)
	if err != nil {
		return nil, err
	}
	wal, err := openTimestampWAL(walPath(storeDir, 1), config.DeltaCapacity)
	if err != nil {
		_ = index.Close()
		return nil, err
	}
	index.persistence = &persistentState{dir: storeDir, durability: durability, wal: wal, walEpoch: 1}
	if err = index.Checkpoint(); err != nil {
		_ = index.Close()
		return nil, err
	}
	index.lock = lock
	locked = false
	return index, nil
}

// OpenPersistent maps the published base generation and replays the WAL tail.
// Block sizing is open-time policy, not persisted state: the files record
// offsets and counts only, so a different BlockSize or DeltaCapacity than
// the writer used changes memory use and compaction cadence, never the
// logical contents.
func OpenPersistent(storeDir string, config Config, durability Durability) (*Index, error) {
	if !durability.Valid() {
		return nil, ErrInvalidConfig
	}
	config, err := config.withDefaults()
	if err != nil {
		return nil, err
	}
	lock, err := storage.AcquireLock(storeDir, storeLockName, false)
	if err != nil {
		return nil, err
	}
	ok := false
	defer func() {
		if !ok {
			_ = storage.ReleaseLock(lock)
		}
	}()
	manifest, err := readManifest(filepath.Join(storeDir, "manifest"))
	if err != nil {
		return nil, err
	}
	markersPath, idsPath, blocksPath := generationPaths(storeDir, manifest.generation)
	markersMapping, err := storage.MapFile(markersPath, nil)
	if err != nil {
		return nil, err
	}
	idsMapping, err := storage.MapFile(idsPath, nil)
	if err != nil {
		_ = storage.Unmap(markersMapping)
		return nil, err
	}
	if len(markersMapping)%formatAlignment != 0 || len(idsMapping)%formatAlignment != 0 {
		_ = storage.Unmap(markersMapping)
		_ = storage.Unmap(idsMapping)
		return nil, ErrCorruptManifest
	}
	directoryBytes, err := os.ReadFile(blocksPath)
	if err != nil {
		_ = storage.Unmap(markersMapping)
		_ = storage.Unmap(idsMapping)
		return nil, err
	}
	if len(directoryBytes)%formatAlignment != 0 {
		_ = storage.Unmap(markersMapping)
		_ = storage.Unmap(idsMapping)
		return nil, ErrCorruptManifest
	}
	directory, err := decodeDirectory(directoryBytes, markersMapping, idsMapping, config)
	if err != nil {
		_ = storage.Unmap(markersMapping)
		_ = storage.Unmap(idsMapping)
		return nil, err
	}
	if uint64(len(directory.entries)) != manifest.blockCount {
		_ = storage.Unmap(markersMapping)
		_ = storage.Unmap(idsMapping)
		return nil, ErrCorruptManifest
	}
	var persistedCount uint64
	for _, entry := range directory.entries {
		if entry.meta.Generation != manifest.generation {
			_ = storage.Unmap(markersMapping)
			_ = storage.Unmap(idsMapping)
			return nil, ErrCorruptManifest
		}
		persistedCount += uint64(entry.meta.Count)
	}
	if persistedCount != manifest.count || directory.entries[0].startMarker != 0 ||
		directory.entries[len(directory.entries)-1].endMarker != markerSpaceEnd {
		_ = storage.Unmap(markersMapping)
		_ = storage.Unmap(idsMapping)
		return nil, ErrCorruptManifest
	}

	index := &Index{
		namespace:     manifest.namespace,
		flags:         manifest.flags,
		offset:        manifest.offsetTime,
		startTime:     manifest.startTime,
		endTime:       manifest.endTime,
		config:        config,
		writes:        make(chan *Batch, config.WriteQueueCapacity),
		barriers:      make(chan chan barrierResult),
		rotate:        make(chan rotateRequest),
		compactions:   make(chan compactionTask, max(config.CompactionQueue, len(directory.entries))),
		compactDone:   make(chan compactionResult, max(config.CompactionQueue, len(directory.entries))),
		stop:          make(chan struct{}),
		writerNeeds:   make([]uint32, len(directory.entries)),
		writerTouched: make([]int, 0, 256),
		persistence: &persistentState{
			dir: storeDir, durability: durability,
			walEpoch: manifest.walEpoch,
			mappings: [][]byte{markersMapping, idsMapping},
		},
	}
	index.generation.Store(manifest.generation)
	index.baseCount.Store(manifest.count)
	index.directory.Store(directory)
	index.workers.Add(2)
	go index.writerLoop()
	go index.compactorLoop()

	recovered, err := replayWAL(walPath(storeDir, manifest.walEpoch), manifest.walOffset)
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
	wal, err := openTimestampWAL(walPath(storeDir, manifest.walEpoch), config.DeltaCapacity)
	if err != nil {
		_ = index.Close()
		return nil, err
	}
	index.persistence.wal = wal
	// Ownership transfers: Close now releases the lock, so disarm the
	// error-path unlock above.
	index.lock = lock
	ok = true
	return index, nil
}

// Verify validates a store directory offline without opening it: the
// manifest, generation headers, block directory ordering, page CRCs and
// alignment, and the referenced WAL file's presence. Mappings are
// read-only and released before return; the WAL is never replayed and no
// handles outlive the call. Best run on a closed store: concurrent
// maintenance may move files underfoot and surface transient errors.
func Verify(dir string) error {
	manifest, err := readManifest(filepath.Join(dir, "manifest"))
	if err != nil {
		return err
	}
	markersPath, idsPath, blocksPath := generationPaths(dir, manifest.generation)
	markersMapping, err := storage.MapFile(markersPath, nil)
	if err != nil {
		return err
	}
	defer func() { _ = storage.Unmap(markersMapping) }()
	idsMapping, err := storage.MapFile(idsPath, nil)
	if err != nil {
		return err
	}
	defer func() { _ = storage.Unmap(idsMapping) }()
	if len(markersMapping)%formatAlignment != 0 || len(idsMapping)%formatAlignment != 0 {
		return ErrCorruptManifest
	}
	directoryBytes, err := os.ReadFile(blocksPath)
	if err != nil {
		return err
	}
	if len(directoryBytes)%formatAlignment != 0 {
		return ErrCorruptManifest
	}
	// Sizing is runtime policy only: persisted state is sizing-agnostic,
	// so verification uses defaults exactly as a fresh open would.
	cfg, err := Config{}.withDefaults()
	if err != nil {
		return err
	}
	directory, err := decodeDirectory(directoryBytes, markersMapping, idsMapping, cfg)
	if err != nil {
		return err
	}
	if uint64(len(directory.entries)) != manifest.blockCount {
		return ErrCorruptManifest
	}
	var persistedCount uint64
	for _, entry := range directory.entries {
		if entry.meta.Generation != manifest.generation {
			return ErrCorruptManifest
		}
		persistedCount += uint64(entry.meta.Count)
	}
	if persistedCount != manifest.count || directory.entries[0].startMarker != 0 ||
		directory.entries[len(directory.entries)-1].endMarker != markerSpaceEnd {
		return ErrCorruptManifest
	}
	if _, err := os.Stat(walPath(dir, manifest.walEpoch)); err != nil {
		return err
	}
	return nil
}

// Checkpoint writes and atomically publishes a new base generation. A
// latched WAL failure fails fast with ErrRecoveryRequired.
//
// Checkpoint takes only a read lock, so batches published concurrently
// land both in the snapshot and in the recorded WAL tail, and open
// replays them a second time. Replay stays correct by collapse: the
// per-ID delta state tables always resolve to the latest operation, so a
// doubled insert or delete reads identically (at the cost of transient
// delta space until compaction). Checkpoint-then-reopen is therefore
// idempotent at read time by construction, not by deduplication.
func (i *Index) Checkpoint() error {
	i.lifecycle.RLock()
	defer i.lifecycle.RUnlock()
	if i.persistence == nil || i.persistence.wal == nil {
		return ErrNotPersistent
	}
	if i.closed.Load() {
		return ErrClosed
	}
	if err := i.failedErr(); err != nil {
		return err
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
	markerFile, err := os.OpenFile(markerTemp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	idFile, err := os.OpenFile(idTemp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
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
		namespace:  i.namespace,
		count:      totalCount,
		startTime:  i.startTime,
		endTime:    i.endTime,
		offsetTime: i.offset,
		walOffset:  barrier.walOffset,
		blockCount: uint64(len(diskEntries)),
		flags:      i.flags,
		walEpoch:   i.persistence.walEpoch,
	}
	if err = writeManifestAtomic(i.persistence.dir, manifest); err != nil {
		return err
	}
	i.generation.Store(generation)
	i.baseCount.Store(totalCount)
	return nil
}

// Vacuum publishes a checkpoint, rolls the WAL to a fresh epoch carrying
// over the post-checkpoint tail, and removes obsolete generations and WAL
// epochs. It does not rewrite or filter base data: unlike the other index
// packages, timeidx Vacuum compacts nothing and drops no records —
// per-block deltas keep folding in the background regardless. Writers
// stall briefly on the lifecycle lock; readers never block. Mappings are
// left alone: live views alias them, so only Close releases them.
// Generation retention beyond the published generation remains a
// storage-owner policy exercised by calling Vacuum.
func (i *Index) Vacuum() error {
	if i.persistence == nil || i.persistence.wal == nil {
		return ErrNotPersistent
	}
	if i.closed.Load() {
		return ErrClosed
	}
	if err := i.Checkpoint(); err != nil {
		return err
	}
	// Exclusive lock blocks new Apply/Checkpoint calls. Batches already
	// queued in the writer loop still drain, which is safe: the rotation
	// request travels through the writerLoop itself, so batches handled
	// before the swap land in the copied tail and batches after it append
	// to the new epoch.
	i.lifecycle.Lock()
	defer i.lifecycle.Unlock()
	if i.closed.Load() {
		return ErrClosed
	}
	if err := i.failedErr(); err != nil {
		return err
	}
	i.persistence.mu.Lock()
	manifest, err := readManifest(filepath.Join(i.persistence.dir, "manifest"))
	if err != nil {
		i.persistence.mu.Unlock()
		return err
	}
	epoch := manifest.walEpoch + 1
	i.persistence.mu.Unlock()

	result := make(chan rotateResult, 1)
	select {
	case i.rotate <- rotateRequest{oldEpoch: manifest.walEpoch, epoch: epoch, fromOffset: manifest.walOffset, result: result}:
	case <-i.stop:
		return ErrClosed
	}
	if res := <-result; res.err != nil {
		_ = os.Remove(walPath(i.persistence.dir, epoch))
		return i.fail(res.err)
	}

	i.persistence.mu.Lock()
	defer i.persistence.mu.Unlock()
	manifest.walOffset = 0
	manifest.walEpoch = epoch
	if err := writeManifestAtomic(i.persistence.dir, manifest); err != nil {
		return err
	}
	i.persistence.walEpoch = epoch
	markersPath, idsPath, blocksPath := generationPaths(i.persistence.dir, manifest.generation)
	if err := storage.RemoveObsolete(i.persistence.dir, "markers.*.base", markersPath); err != nil {
		return err
	}
	if err := storage.RemoveObsolete(i.persistence.dir, "ids.*.base", idsPath); err != nil {
		return err
	}
	if err := storage.RemoveObsolete(i.persistence.dir, "blocks.*.idx", blocksPath); err != nil {
		return err
	}
	if err := storage.RemoveObsolete(i.persistence.dir, walGlob, walPath(i.persistence.dir, epoch)); err != nil {
		return err
	}
	matches, _ := filepath.Glob(filepath.Join(i.persistence.dir, "*.tmp"))
	for _, path := range matches {
		_ = os.Remove(path)
	}
	return nil
}

// rotateWAL runs on the writerLoop goroutine, the sole WAL appender: it
// copies the complete tail frames at and after fromOffset of the old epoch
// into a fresh epoch file and swaps the live WAL handle. Callers publish a
// manifest referencing (epoch, 0) only after this returns success; a crash
// before that leaves the old manifest and old WAL authoritative, a crash
// after it leaves a trivially consistent new epoch.
func (i *Index) rotateWAL(req rotateRequest) rotateResult {
	oldPath := walPath(i.persistence.dir, req.oldEpoch)
	newPath := walPath(i.persistence.dir, req.epoch)
	end, err := replayWALEnd(oldPath, req.fromOffset)
	if err != nil {
		return rotateResult{err: err}
	}
	data, err := os.ReadFile(oldPath)
	if err != nil {
		return rotateResult{err: err}
	}
	if req.fromOffset < 0 || end < req.fromOffset || end > int64(len(data)) {
		return rotateResult{err: ErrCorruptWAL}
	}
	file, err := os.OpenFile(newPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return rotateResult{err: err}
	}
	if _, err = file.Write(data[req.fromOffset:end]); err != nil {
		_ = file.Close()
		return rotateResult{err: err}
	}
	if err = file.Sync(); err != nil {
		_ = file.Close()
		return rotateResult{err: err}
	}
	if err = file.Close(); err != nil {
		return rotateResult{err: err}
	}
	wal, err := openTimestampWAL(newPath, i.config.DeltaCapacity)
	if err != nil {
		return rotateResult{err: err}
	}
	old := i.persistence.wal
	i.persistence.wal = wal
	_ = old.Close()
	return rotateResult{}
}

// OnDiskBytes reports the bytes held by generation files, WAL files, and
// the manifest.
func (i *Index) OnDiskBytes() uint64 {
	if i.persistence == nil {
		return 0
	}
	return storage.DirSize(i.persistence.dir, "markers.*.base", "ids.*.base", "blocks.*.idx", walGlob, "manifest")
}

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
	aligned := int64(storage.Align(uint64(offset), alignment))
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
	file, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
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
		markers, err := storage.View[timemarker](markerData, meta.MarkerOffset, uint64(meta.Count), ErrInvalidBase, ErrUnsupportedEndian)
		if err != nil {
			return nil, err
		}
		ids, err := storage.View[ImageID](idData, meta.IDOffset, uint64(meta.Count), ErrInvalidBase, ErrUnsupportedEndian)
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
	return storage.WriteManifest(dir, data)
}

func encodeManifest(manifest diskManifest) []byte {
	return storage.BuildManifest(manifestSize, manifestMagic, manifestVersion, 84, 84, func(data []byte) {
		data[6] = TimestampShift
		binary.LittleEndian.PutUint64(data[8:16], manifest.generation)
		binary.LittleEndian.PutUint64(data[16:24], uint64(manifest.namespace))
		binary.LittleEndian.PutUint64(data[24:32], manifest.count)
		binary.LittleEndian.PutUint64(data[32:40], manifest.startTime)
		binary.LittleEndian.PutUint64(data[40:48], manifest.endTime)
		binary.LittleEndian.PutUint64(data[48:56], manifest.offsetTime)
		binary.LittleEndian.PutUint64(data[56:64], uint64(manifest.walOffset))
		binary.LittleEndian.PutUint64(data[64:72], manifest.blockCount)
		binary.LittleEndian.PutUint32(data[72:76], manifest.flags)
		binary.LittleEndian.PutUint64(data[76:84], manifest.walEpoch)
	})
}

func readManifest(path string) (diskManifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return diskManifest{}, err
	}
	return decodeManifest(data)
}

// decodeManifest validates manifest bytes without touching the
// filesystem, so fuzz targets and offline verification share the open
// path exactly.
func decodeManifest(data []byte) (diskManifest, error) {
	// A recognized magic with an unknown version is a newer format, not
	// corruption: fail open with ErrUnsupported so callers never misread
	// it as same-version bit rot.
	if magic, version, ok := storage.PeekHeader(data); ok && magic == manifestMagic && version != manifestVersion {
		return diskManifest{}, ErrUnsupported
	}
	if err := storage.VerifyManifest(data, manifestSize, manifestMagic, manifestVersion, 84, 84); err != nil {
		return diskManifest{}, ErrCorruptManifest
	}
	rawNamespace := binary.LittleEndian.Uint64(data[16:24])
	if rawNamespace > math.MaxUint16 {
		return diskManifest{}, ErrCorruptManifest
	}
	manifest := diskManifest{
		generation: binary.LittleEndian.Uint64(data[8:16]),
		namespace:  common.Namespace(rawNamespace),
		count:      binary.LittleEndian.Uint64(data[24:32]),
		startTime:  binary.LittleEndian.Uint64(data[32:40]),
		endTime:    binary.LittleEndian.Uint64(data[40:48]),
		offsetTime: binary.LittleEndian.Uint64(data[48:56]),
		walOffset:  int64(binary.LittleEndian.Uint64(data[56:64])),
		blockCount: binary.LittleEndian.Uint64(data[64:72]),
		flags:      binary.LittleEndian.Uint32(data[72:76]),
		walEpoch:   binary.LittleEndian.Uint64(data[76:84]),
	}
	if manifest.generation == 0 || manifest.walOffset < 0 || manifest.walEpoch == 0 {
		return diskManifest{}, ErrCorruptManifest
	}
	return manifest, nil
}
