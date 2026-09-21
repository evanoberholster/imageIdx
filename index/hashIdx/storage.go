package hashidx

import (
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"

	"github.com/evanoberholster/imageIdx/internal/indexutil"
	"github.com/evanoberholster/imageIdx/internal/persist"
)

const (
	manifestMagic   uint32 = 0x48494d46 // HIMF
	manifestVersion uint16 = 1
	manifestSize           = 64

	genMagic      uint32 = 0x48494458 // HIDX
	genVersion    uint16 = 1
	genHeaderSize        = 64

	pageMagic      uint32 = 0x48504745 // HPGE
	pageVersion    uint16 = 1
	pageHeaderSize        = 64

	pageRefSize = 16

	// recordSize is one ordinal's fixed width: ImageID8 + pHash64 8 + PDQ 32.
	recordSize = 48

	genPattern = "hashidx-%020d.data"
	walPattern = "hashidx-%020d.wal"
	genGlob    = "hashidx-*.data"
	walGlob    = "hashidx-*.wal"
)

// diskManifest is the atomically published root.
// Layout (little-endian): magic 0:4, version 4:6, generation 8:16,
// walOffset 16:24, recordCount 24:32, pageCount 32:40,
// recordsPerPage 40:44, flags 44:48, walEpoch 48:56, CRC32 60:64 over
// [0:60).
type diskManifest struct {
	generation     uint64
	walOffset      int64
	recordCount    uint64
	pageCount      uint64
	recordsPerPage uint32
	flags          uint32
	walEpoch       uint64
}

// genHeader is the fixed 64-byte generation header.
// Layout: magic 0:4, version 4:6, generation 8:16, flags 16:20,
// recordsPerPage 20:24, pageCount 24:32, recordCount 32:40,
// directoryOffset 40:48, dataOffset 48:56, directoryCRC 56:60,
// CRC32 60:64 over [0:60).
type genHeader struct {
	generation      uint64
	flags           uint32
	recordsPerPage  uint32
	pageCount       uint64
	recordCount     uint64
	directoryOffset uint64
	dataOffset      uint64
	directoryCRC    uint32
}

// pageRef locates one immutable page: offset 0:8, count 8:12, flags 12:16.
type pageRef struct {
	offset uint64
	count  uint32
	flags  uint32
}

func generationPath(dir string, generation uint64) string {
	return filepath.Join(dir, fmt.Sprintf(genPattern, generation))
}

// walPath names the WAL for one epoch. Vacuum starts a fresh epoch instead of
// truncating in place, which keeps crash recovery free of offset resets.
func walPath(dir string, epoch uint64) string {
	return filepath.Join(dir, fmt.Sprintf(walPattern, epoch))
}

func validRecordsPerPage(value uint32) bool {
	return value >= minRecordsPerPage && value <= maxRecordsPerPage && value%8 == 0
}

// newIndex creates a persistent index in dir and publishes its initial
// checkpoint and manifest. An optional reader tracker lets Store share one
// tracker across its indexes.
func newIndex(dir string, cfg Config, durability Durability, readers ...*indexutil.ReaderTracker) (*Index, error) {
	if !durability.Valid() {
		return nil, ErrInvalidConfig
	}
	cfg, err := cfg.withDefaults()
	if err != nil {
		return nil, err
	}
	if err = os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	wal, err := openHashWAL(walPath(dir, 1))
	if err != nil {
		return nil, err
	}
	idx := &Index{config: cfg, readers: chooseReaders(readers)}
	idx.view.Store(&indexView{generation: 1})
	idx.persistence = persistedState{dir: dir, durability: durability, wal: wal, walEpoch: 1}
	if err = idx.Checkpoint(); err != nil {
		_ = idx.Close()
		return nil, err
	}
	return idx, nil
}

// openIndex maps the published generation and replays the WAL tail. A non-zero
// cfg.RecordsPerPage must match the persisted page size; a zero value adopts
// the persisted size. An optional reader tracker lets Store share one tracker
// across its indexes.
func openIndex(dir string, cfg Config, durability Durability, readers ...*indexutil.ReaderTracker) (*Index, error) {
	if !durability.Valid() {
		return nil, ErrInvalidConfig
	}
	manifest, err := readManifest(filepath.Join(dir, "manifest"))
	if err != nil {
		return nil, err
	}
	if cfg.RecordsPerPage != 0 {
		if !validRecordsPerPage(cfg.RecordsPerPage) {
			return nil, ErrInvalidConfig
		}
		if cfg.RecordsPerPage != manifest.recordsPerPage {
			return nil, ErrInvalidConfig
		}
	}
	// RecordsPerPage is adopted from the manifest; MaxBatchEntries stays a
	// caller runtime policy and is never persisted.
	maxBatch := cfg.MaxBatchEntries
	cfg = Config{RecordsPerPage: manifest.recordsPerPage, MaxBatchEntries: maxBatch}
	if cfg.MaxBatchEntries == 0 {
		cfg.MaxBatchEntries = DefaultMaxApplyEntries
	}
	mapping, err := persist.MapFile(generationPath(dir, manifest.generation), ErrCorrupt)
	if err != nil {
		return nil, err
	}
	view, header, err := decodeGeneration(mapping, cfg)
	if err != nil {
		_ = persist.Unmap(mapping)
		return nil, err
	}
	if header.generation != manifest.generation || view.count != manifest.recordCount || header.pageCount != manifest.pageCount {
		_ = persist.Unmap(mapping)
		return nil, ErrCorrupt
	}
	idx := &Index{
		config:  cfg,
		readers: chooseReaders(readers),
		persistence: persistedState{
			dir: dir, durability: durability, mappings: [][]byte{mapping},
			persistedGen: manifest.generation, walEpoch: manifest.walEpoch,
		},
	}
	idx.view.Store(view)
	entries, err := replayHashWAL(walPath(dir, manifest.walEpoch), manifest.walOffset)
	if err != nil {
		_ = persist.Unmap(mapping)
		return nil, err
	}
	idx.appendEntriesLocked(entries)
	wal, err := openHashWAL(walPath(dir, manifest.walEpoch))
	if err != nil {
		_ = persist.Unmap(mapping)
		return nil, err
	}
	idx.persistence.wal = wal
	return idx, nil
}

// Checkpoint writes the current view as a new generation and atomically
// publishes a manifest pointing at it. The WAL offset in the manifest covers
// every appended record, so replay after a crash is exact.
func (i *Index) Checkpoint() error {
	i.writerMu.Lock()
	defer i.writerMu.Unlock()
	return i.checkpointLocked()
}

func (i *Index) checkpointLocked() error {
	if i.closed.Load() {
		return ErrClosed
	}
	if i.failed != nil {
		return i.failed
	}
	i.persistence.mu.Lock()
	defer i.persistence.mu.Unlock()
	view := i.view.Load()
	if view.generation == i.persistence.persistedGen {
		return nil
	}
	walOffset, err := i.persistence.wal.Offset()
	if err != nil {
		return err
	}
	manifest := diskManifest{
		generation:     view.generation,
		walOffset:      walOffset,
		walEpoch:       i.persistence.walEpoch,
		recordCount:    view.count,
		pageCount:      view.logicalPageCount(),
		recordsPerPage: i.config.RecordsPerPage,
	}
	if err := writeGenerationFile(i.persistence.dir, view, i.config.RecordsPerPage); err != nil {
		return err
	}
	if err := writeManifest(i.persistence.dir, manifest); err != nil {
		return err
	}
	i.persistence.persistedGen = view.generation
	return nil
}

// Vacuum rewrites only records admitted by live into a dense new generation,
// repacks pages, starts a fresh WAL epoch, and retires obsolete generations
// after prior readers finish. A nil live set keeps every physical record.
// Ordinals change across Vacuum.
func (i *Index) Vacuum(live CandidateSet) error {
	i.writerMu.Lock()
	defer i.writerMu.Unlock()
	if i.closed.Load() {
		return ErrClosed
	}
	if i.failed != nil {
		return i.failed
	}
	next := buildDenseView(i.view.Load(), int(i.config.RecordsPerPage), live)
	i.persistence.mu.Lock()
	defer i.persistence.mu.Unlock()
	// Write the dense generation, then create the next WAL epoch and publish
	// a manifest that references it with a zero offset. A crash before the
	// manifest leaves the old manifest and old WAL authoritative; a crash
	// after it leaves a trivially consistent empty WAL. The old WAL file is
	// removed only after the new root is durable.
	if err := writeGenerationFile(i.persistence.dir, next, i.config.RecordsPerPage); err != nil {
		return err
	}
	epoch := i.persistence.walEpoch + 1
	newWAL, err := openHashWAL(walPath(i.persistence.dir, epoch))
	if err != nil {
		return err
	}
	manifest := diskManifest{
		generation:     next.generation,
		walOffset:      0,
		walEpoch:       epoch,
		recordCount:    next.count,
		pageCount:      next.logicalPageCount(),
		recordsPerPage: i.config.RecordsPerPage,
	}
	if err = writeManifest(i.persistence.dir, manifest); err != nil {
		_ = newWAL.Close()
		return err
	}
	oldWAL := i.persistence.wal
	i.persistence.wal = newWAL
	i.persistence.walEpoch = epoch
	i.persistence.persistedGen = next.generation
	_ = oldWAL.Close()
	mapping, err := persist.MapFile(generationPath(i.persistence.dir, next.generation), ErrCorrupt)
	if err != nil {
		return err
	}
	mapped, _, err := decodeGeneration(mapping, i.config)
	if err != nil {
		_ = persist.Unmap(mapping)
		return err
	}
	if mapped.count != next.count {
		_ = persist.Unmap(mapping)
		return ErrCorrupt
	}
	i.view.Store(mapped)
	i.readers.WaitForPriorReaders()
	obsolete := i.persistence.mappings
	i.persistence.mappings = [][]byte{mapping}
	_ = persist.RemoveObsolete(i.persistence.dir, genGlob, generationPath(i.persistence.dir, next.generation))
	_ = persist.RemoveObsolete(i.persistence.dir, walGlob, walPath(i.persistence.dir, epoch))
	for _, old := range obsolete {
		_ = persist.Unmap(old)
	}
	return nil
}

// OnDiskBytes reports the bytes held by generation files, WAL files, and the
// manifest.
func (i *Index) OnDiskBytes() uint64 {
	return indexutil.DirSize(i.persistence.dir, genGlob, walGlob, "manifest")
}

// buildDenseView copies records admitted by live into fresh page planes.
// The page slice is presized from the current page count so repacking
// never regrows it; liveness still probes per record.
func buildDenseView(current *indexView, recordsPerPage int, live CandidateSet) *indexView {
	pages := make([]pageView, 0, len(current.pages)+1)
	var active *activePage
	activeCount := 0
	var count uint64
	appendRecord := func(entry Entry) {
		if active == nil {
			active = newActivePage(recordsPerPage)
			activeCount = 0
		}
		active.imageIDs[activeCount] = entry.ImageID
		active.phash64[activeCount] = entry.PHash64
		active.pdq[0][activeCount] = entry.PDQ[0]
		active.pdq[1][activeCount] = entry.PDQ[1]
		active.pdq[2][activeCount] = entry.PDQ[2]
		active.pdq[3][activeCount] = entry.PDQ[3]
		activeCount++
		count++
		if activeCount == recordsPerPage {
			pages = append(pages, pageView{imageIDs: active.imageIDs, phash64: active.phash64, pdq: active.pdq})
			active = nil
			activeCount = 0
		}
	}
	for pageIndex := range current.pages {
		page := &current.pages[pageIndex]
		for k := 0; k < page.count(); k++ {
			entry := entryAtPage(page, k)
			if live != nil && !live.Contains(entry.ImageID) {
				continue
			}
			appendRecord(entry)
		}
	}
	if current.active != nil {
		for k := 0; k < current.activeCount; k++ {
			entry := entryAtActive(current.active, k)
			if live != nil && !live.Contains(entry.ImageID) {
				continue
			}
			appendRecord(entry)
		}
	}
	return &indexView{
		generation:  current.generation + 1,
		count:       count,
		pages:       pages,
		active:      active,
		activeCount: activeCount,
	}
}

// writeGenerationFile streams a complete generation to a temporary file,
// fsyncs it, and renames it into place.
func writeGenerationFile(dir string, view *indexView, recordsPerPage uint32) error {
	pageCount := view.logicalPageCount()
	directoryBytes := pageCount * pageRefSize
	dataOffset := uint64(genHeaderSize) + directoryBytes
	directory := make([]byte, directoryBytes)
	offset := dataOffset
	for pageIndex := range view.pages {
		page := &view.pages[pageIndex]
		writePageRef(directory[uint64(pageIndex)*pageRefSize:], offset, uint32(page.count()))
		offset += pageHeaderSize + uint64(page.count())*recordSize
	}
	if view.activeCount > 0 {
		writePageRef(directory[uint64(len(view.pages))*pageRefSize:], offset, uint32(view.activeCount))
		offset += pageHeaderSize + uint64(view.activeCount)*recordSize
	}
	header := genHeader{
		generation:      view.generation,
		recordsPerPage:  recordsPerPage,
		pageCount:       pageCount,
		recordCount:     view.count,
		directoryOffset: genHeaderSize,
		dataOffset:      dataOffset,
		directoryCRC:    crc32.ChecksumIEEE(directory),
	}
	headerBytes := make([]byte, genHeaderSize)
	encodeGenHeader(headerBytes, header)
	buffer := make([]byte, 64*1024)
	return writeAtomicFile(generationPath(dir, view.generation), func(file *os.File) error {
		if _, err := file.Write(headerBytes); err != nil {
			return err
		}
		if _, err := file.Write(directory); err != nil {
			return err
		}
		for pageIndex := range view.pages {
			page := &view.pages[pageIndex]
			if err := writePage(file, buffer, page.imageIDs, page.phash64, page.pdq); err != nil {
				return err
			}
		}
		if view.activeCount > 0 {
			active := view.active
			count := view.activeCount
			return writePage(file, buffer,
				active.imageIDs[:count], active.phash64[:count],
				[4][]uint64{
					active.pdq[0][:count], active.pdq[1][:count],
					active.pdq[2][:count], active.pdq[3][:count],
				})
		}
		return nil
	})
}

func writePageRef(dst []byte, offset uint64, count uint32) {
	binary.LittleEndian.PutUint64(dst[0:8], offset)
	binary.LittleEndian.PutUint32(dst[8:12], count)
	binary.LittleEndian.PutUint32(dst[12:16], 0)
}

// writePage encodes one page: a 64-byte header followed by the six planes.
// Planes are written with explicit little-endian encoding; the CRC covers the
// concatenated payload.
func writePage(file *os.File, buffer []byte, imageIDs []ImageID, phash64 []uint64, pdq [4][]uint64) error {
	crc := crc32.ChecksumIEEE(nil)
	crc = crcPlane(crc, buffer, imageIDs)
	crc = crcPlane(crc, buffer, phash64)
	for word := 0; word < 4; word++ {
		crc = crcPlane(crc, buffer, pdq[word])
	}
	var headerBuf [pageHeaderSize]byte
	header := headerBuf[:]
	encodePageHeader(header, uint32(len(imageIDs)), crc)
	if _, err := file.Write(header); err != nil {
		return err
	}
	if err := writePlane(file, buffer, imageIDs); err != nil {
		return err
	}
	if err := writePlane(file, buffer, phash64); err != nil {
		return err
	}
	for word := 0; word < 4; word++ {
		if err := writePlane(file, buffer, pdq[word]); err != nil {
			return err
		}
	}
	return nil
}

// emitPlaneChunks encodes values into buffer in explicit little-endian uint64
// chunks, updating *crc and writing to w when they are non-nil. Both plane
// checksumming and plane file writes share this one traversal.
func emitPlaneChunks(values []uint64, buffer []byte, crc *uint32, w io.Writer) error {
	perPass := len(buffer) / 8
	for len(values) > 0 {
		n := min(len(values), perPass)
		for k := 0; k < n; k++ {
			binary.LittleEndian.PutUint64(buffer[k*8:k*8+8], values[k])
		}
		chunk := buffer[:n*8]
		if crc != nil {
			*crc = crc32.Update(*crc, crc32.IEEETable, chunk)
		}
		if w != nil {
			if _, err := w.Write(chunk); err != nil {
				return err
			}
		}
		values = values[n:]
	}
	return nil
}

func crcPlane(crc uint32, buffer []byte, values []uint64) uint32 {
	_ = emitPlaneChunks(values, buffer, &crc, nil)
	return crc
}

func writePlane(file *os.File, buffer []byte, values []uint64) error {
	return emitPlaneChunks(values, buffer, nil, file)
}

func encodePageHeader(dst []byte, count uint32, crc uint32) {
	binary.LittleEndian.PutUint32(dst[0:4], pageMagic)
	binary.LittleEndian.PutUint16(dst[4:6], pageVersion)
	binary.LittleEndian.PutUint32(dst[8:12], count)
	binary.LittleEndian.PutUint32(dst[12:16], 0)
	binary.LittleEndian.PutUint64(dst[16:24], uint64(count)*recordSize)
	binary.LittleEndian.PutUint32(dst[24:28], crc)
	clear(dst[28:pageHeaderSize])
}

// encodeGenHeader writes a 64-byte generation header with a trailing CRC.
func encodeGenHeader(dst []byte, header genHeader) {
	binary.LittleEndian.PutUint32(dst[0:4], genMagic)
	binary.LittleEndian.PutUint16(dst[4:6], genVersion)
	binary.LittleEndian.PutUint64(dst[8:16], header.generation)
	binary.LittleEndian.PutUint32(dst[16:20], header.flags)
	binary.LittleEndian.PutUint32(dst[20:24], header.recordsPerPage)
	binary.LittleEndian.PutUint64(dst[24:32], header.pageCount)
	binary.LittleEndian.PutUint64(dst[32:40], header.recordCount)
	binary.LittleEndian.PutUint64(dst[40:48], header.directoryOffset)
	binary.LittleEndian.PutUint64(dst[48:56], header.dataOffset)
	binary.LittleEndian.PutUint32(dst[56:60], header.directoryCRC)
	clear(dst[6:8])
	clear(dst[60:64])
	binary.LittleEndian.PutUint32(dst[60:64], crc32.ChecksumIEEE(dst[:60]))
}

// decodeGenHeader validates the fixed generation header.
func decodeGenHeader(data []byte) (genHeader, error) {
	if len(data) < genHeaderSize {
		return genHeader{}, ErrCorrupt
	}
	if binary.LittleEndian.Uint32(data[0:4]) != genMagic {
		return genHeader{}, ErrCorrupt
	}
	if binary.LittleEndian.Uint16(data[4:6]) != genVersion {
		return genHeader{}, ErrUnsupported
	}
	if crc32.ChecksumIEEE(data[:60]) != binary.LittleEndian.Uint32(data[60:64]) {
		return genHeader{}, ErrCorrupt
	}
	return genHeader{
		generation:      binary.LittleEndian.Uint64(data[8:16]),
		flags:           binary.LittleEndian.Uint32(data[16:20]),
		recordsPerPage:  binary.LittleEndian.Uint32(data[20:24]),
		pageCount:       binary.LittleEndian.Uint64(data[24:32]),
		recordCount:     binary.LittleEndian.Uint64(data[32:40]),
		directoryOffset: binary.LittleEndian.Uint64(data[40:48]),
		dataOffset:      binary.LittleEndian.Uint64(data[48:56]),
		directoryCRC:    binary.LittleEndian.Uint32(data[56:60]),
	}, nil
}

// decodePageHeader validates one page image and returns its record count and
// payload slice. data spans from the page start to the end of the mapping.
func decodePageHeader(data []byte) (int, []byte, error) {
	if len(data) < pageHeaderSize {
		return 0, nil, ErrCorrupt
	}
	if binary.LittleEndian.Uint32(data[0:4]) != pageMagic {
		return 0, nil, ErrCorrupt
	}
	if binary.LittleEndian.Uint16(data[4:6]) != pageVersion {
		return 0, nil, ErrUnsupported
	}
	count := int(binary.LittleEndian.Uint32(data[8:12]))
	payloadLength := binary.LittleEndian.Uint64(data[16:24])
	if count <= 0 || payloadLength != uint64(count)*recordSize {
		return 0, nil, ErrCorrupt
	}
	if payloadLength > uint64(len(data)-pageHeaderSize) {
		return 0, nil, ErrCorrupt
	}
	payload := data[pageHeaderSize : pageHeaderSize+payloadLength]
	if crc32.ChecksumIEEE(payload) != binary.LittleEndian.Uint32(data[24:28]) {
		return 0, nil, ErrCorrupt
	}
	return count, payload, nil
}

// decodeGeneration validates a mapped generation and builds a view whose
// planes reference the mapping. A final partial page becomes the active page
// so later appends continue the same ordinal space.
func decodeGeneration(mapping []byte, cfg Config) (*indexView, genHeader, error) {
	header, err := decodeGenHeader(mapping)
	if err != nil {
		return nil, genHeader{}, err
	}
	if !validRecordsPerPage(header.recordsPerPage) || header.recordsPerPage != cfg.RecordsPerPage {
		return nil, genHeader{}, ErrInvalidConfig
	}
	// Bound the page count by the file size before any arithmetic or
	// allocation, so a corrupt header cannot provoke overflow or a huge
	// allocation.
	if header.pageCount > uint64(len(mapping))/pageRefSize {
		return nil, genHeader{}, ErrCorrupt
	}
	directoryBytes := header.pageCount * pageRefSize
	if header.directoryOffset != genHeaderSize || header.dataOffset != uint64(genHeaderSize)+directoryBytes {
		return nil, genHeader{}, ErrCorrupt
	}
	if header.dataOffset > uint64(len(mapping)) {
		return nil, genHeader{}, ErrCorrupt
	}
	directory := mapping[genHeaderSize:header.dataOffset]
	if crc32.ChecksumIEEE(directory) != header.directoryCRC {
		return nil, genHeader{}, ErrCorrupt
	}
	recordsPerPage := uint64(header.recordsPerPage)
	offset := header.dataOffset
	var recordCount uint64
	pages := make([]pageView, 0, header.pageCount)
	var active *activePage
	activeCount := 0
	for index := uint64(0); index < header.pageCount; index++ {
		ref := directory[index*pageRefSize : (index+1)*pageRefSize]
		pageOffset := binary.LittleEndian.Uint64(ref[0:8])
		count := int(binary.LittleEndian.Uint32(ref[8:12]))
		if pageOffset != offset {
			return nil, genHeader{}, ErrCorrupt
		}
		if count <= 0 || uint64(count) > recordsPerPage {
			return nil, genHeader{}, ErrCorrupt
		}
		if index+1 < header.pageCount && uint64(count) != recordsPerPage {
			return nil, genHeader{}, ErrCorrupt
		}
		decoded, payload, err := decodePageHeader(mapping[offset:])
		if err != nil {
			return nil, genHeader{}, err
		}
		if decoded != count || len(payload) != count*recordSize {
			return nil, genHeader{}, ErrCorrupt
		}
		planeWidth := uint64(count) * 8
		planeOffset := offset + pageHeaderSize
		imageIDs, err := persist.View[ImageID](mapping, planeOffset, uint64(count), ErrCorrupt, ErrUnsupported)
		if err != nil {
			return nil, genHeader{}, err
		}
		phash64, err := persist.View[uint64](mapping, planeOffset+planeWidth, uint64(count), ErrCorrupt, ErrUnsupported)
		if err != nil {
			return nil, genHeader{}, err
		}
		var pdq [4][]uint64
		for word := 0; word < 4; word++ {
			pdq[word], err = persist.View[uint64](mapping, planeOffset+planeWidth*uint64(2+word), uint64(count), ErrCorrupt, ErrUnsupported)
			if err != nil {
				return nil, genHeader{}, err
			}
		}
		if index+1 == header.pageCount && uint64(count) < recordsPerPage {
			active = newActivePage(int(recordsPerPage))
			copy(active.imageIDs, imageIDs)
			copy(active.phash64, phash64)
			for word := 0; word < 4; word++ {
				copy(active.pdq[word], pdq[word])
			}
			activeCount = count
		} else {
			pages = append(pages, pageView{imageIDs: imageIDs, phash64: phash64, pdq: pdq})
		}
		offset = planeOffset + planeWidth*6
		recordCount += uint64(count)
	}
	if offset != uint64(len(mapping)) {
		return nil, genHeader{}, ErrCorrupt
	}
	if recordCount != header.recordCount {
		return nil, genHeader{}, ErrCorrupt
	}
	return &indexView{
		generation:  header.generation,
		count:       recordCount,
		pages:       pages,
		active:      active,
		activeCount: activeCount,
	}, header, nil
}

func writeManifest(dir string, manifest diskManifest) error {
	data := persist.BuildManifest(manifestSize, manifestMagic, manifestVersion, 60, 60, func(data []byte) {
		binary.LittleEndian.PutUint64(data[8:16], manifest.generation)
		binary.LittleEndian.PutUint64(data[16:24], uint64(manifest.walOffset))
		binary.LittleEndian.PutUint64(data[24:32], manifest.recordCount)
		binary.LittleEndian.PutUint64(data[32:40], manifest.pageCount)
		binary.LittleEndian.PutUint32(data[40:44], manifest.recordsPerPage)
		binary.LittleEndian.PutUint32(data[44:48], manifest.flags)
		binary.LittleEndian.PutUint64(data[48:56], manifest.walEpoch)
	})
	return persist.WriteManifest(dir, data)
}

func readManifest(path string) (diskManifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return diskManifest{}, err
	}
	// A recognized magic with an unknown version is a newer format, not
	// corruption: fail open with ErrUnsupported so callers never misread
	// it as a same-version bit rot.
	if magic, version, ok := persist.PeekHeader(data); ok && magic == manifestMagic && version != manifestVersion {
		return diskManifest{}, ErrUnsupported
	}
	if err = persist.VerifyManifest(data, manifestSize, manifestMagic, manifestVersion, 60, 60); err != nil {
		return diskManifest{}, ErrCorrupt
	}
	manifest := diskManifest{
		generation:     binary.LittleEndian.Uint64(data[8:16]),
		walOffset:      int64(binary.LittleEndian.Uint64(data[16:24])),
		recordCount:    binary.LittleEndian.Uint64(data[24:32]),
		pageCount:      binary.LittleEndian.Uint64(data[32:40]),
		recordsPerPage: binary.LittleEndian.Uint32(data[40:44]),
		flags:          binary.LittleEndian.Uint32(data[44:48]),
		walEpoch:       binary.LittleEndian.Uint64(data[48:56]),
	}
	if !validRecordsPerPage(manifest.recordsPerPage) || manifest.generation == 0 ||
		manifest.walOffset < 0 || manifest.walEpoch == 0 {
		return diskManifest{}, ErrCorrupt
	}
	return manifest, nil
}

const (
	exactManifestMagic   uint32 = 0x48494d46 // HIMF
	exactManifestVersion uint16 = 2
	exactManifestSize           = 64

	exactGenMagic      uint32 = 0x48494458 // HIDX
	exactGenVersion    uint16 = 1
	exactGenHeaderSize        = 128

	exactWALPattern = "exact-%020d.wal"
	exactWALGlob    = "exact-*.wal"
	exactGenPattern = "exact-%020d.idx"
	exactGenGlob    = "exact-*.idx"
)

// exactManifest is the atomically published root.
// Layout (little-endian): magic 0:4, version 4:6, algorithm 6, targetFillPct
// 7, generation 8:16, indexID 16:24, liveCount 24:32, blockCount 32:40,
// walOffset 40:48, flags 48:52, blockSize 52:56, walEpoch 56:60, CRC32 60:64
// over [0:60).
type exactManifest struct {
	generation    uint64
	indexID       uint64
	liveCount     uint64
	blockCount    uint64
	walOffset     int64
	flags         uint32
	blockSize     uint32
	algorithm     Algorithm
	targetFillPct uint8
	walEpoch      uint32
}

func encodeManifest(m exactManifest) []byte {
	return persist.BuildManifest(exactManifestSize, exactManifestMagic, exactManifestVersion, 60, 60, func(data []byte) {
		data[6] = byte(m.algorithm)
		data[7] = m.targetFillPct
		binary.LittleEndian.PutUint64(data[8:16], m.generation)
		binary.LittleEndian.PutUint64(data[16:24], m.indexID)
		binary.LittleEndian.PutUint64(data[24:32], m.liveCount)
		binary.LittleEndian.PutUint64(data[32:40], m.blockCount)
		binary.LittleEndian.PutUint64(data[40:48], uint64(m.walOffset))
		binary.LittleEndian.PutUint32(data[48:52], m.flags)
		binary.LittleEndian.PutUint32(data[52:56], m.blockSize)
		binary.LittleEndian.PutUint32(data[56:60], m.walEpoch)
	})
}

func decodeManifest(data []byte) (exactManifest, error) {
	// Recognized magic with an unknown version is a newer format, not
	// corruption: fail open with ErrUnsupported (see readManifest).
	if magic, version, ok := persist.PeekHeader(data); ok && magic == exactManifestMagic && version != exactManifestVersion {
		return exactManifest{}, ErrUnsupported
	}
	if err := persist.VerifyManifest(data, exactManifestSize, exactManifestMagic, exactManifestVersion, 60, 60); err != nil {
		return exactManifest{}, ErrCorrupt
	}
	algo := Algorithm(data[6])
	if !algo.Valid() {
		return exactManifest{}, ErrCorrupt
	}
	m := exactManifest{
		algorithm:     algo,
		targetFillPct: data[7],
		generation:    binary.LittleEndian.Uint64(data[8:16]),
		indexID:       binary.LittleEndian.Uint64(data[16:24]),
		liveCount:     binary.LittleEndian.Uint64(data[24:32]),
		blockCount:    binary.LittleEndian.Uint64(data[32:40]),
		walOffset:     int64(binary.LittleEndian.Uint64(data[40:48])),
		flags:         binary.LittleEndian.Uint32(data[48:52]),
		blockSize:     binary.LittleEndian.Uint32(data[52:56]),
		walEpoch:      binary.LittleEndian.Uint32(data[56:60]),
	}
	if m.generation == 0 || m.walOffset < 0 || m.walEpoch == 0 {
		return exactManifest{}, ErrCorrupt
	}
	return m, nil
}

func readDiskManifest(path string) (exactManifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return exactManifest{}, err
	}
	return decodeManifest(data)
}

// encodeExactGenHeader writes a 128-byte generation header. Layout (LE):
// 0:4 magic, 4:6 version, 6 algorithm u8, 7 targetFillPct u8,
// 8:16 generation, 16:24 indexID, 24:28 flags, 28:32 blockCount,
// 32:40 liveCount, 40:48 dirOffset, 48:56 dirLen,
// 56:64 walOffset, 64:68 blockSize, 68:124 reserved, 124:128 crc of [0:124).
func encodeExactGenHeader(dst []byte, m exactManifest) {
	binary.LittleEndian.PutUint32(dst[0:4], exactGenMagic)
	binary.LittleEndian.PutUint16(dst[4:6], exactGenVersion)
	dst[6] = byte(m.algorithm)
	dst[7] = m.targetFillPct
	binary.LittleEndian.PutUint64(dst[8:16], m.generation)
	binary.LittleEndian.PutUint64(dst[16:24], m.indexID)
	binary.LittleEndian.PutUint32(dst[24:28], m.flags)
	binary.LittleEndian.PutUint32(dst[28:32], uint32(m.blockCount))
	binary.LittleEndian.PutUint64(dst[32:40], m.liveCount)
	binary.LittleEndian.PutUint64(dst[40:48], exactGenHeaderSize)
	binary.LittleEndian.PutUint64(dst[48:56], m.blockCount*dirEntrySize)
	binary.LittleEndian.PutUint64(dst[56:64], uint64(m.walOffset))
	binary.LittleEndian.PutUint32(dst[64:68], m.blockSize)
	clear(dst[68:124])
	binary.LittleEndian.PutUint32(dst[124:128], crc32.ChecksumIEEE(dst[:124]))
}

func decodeExactGenHeader(data []byte) (exactManifest, error) {
	if len(data) < exactGenHeaderSize {
		return exactManifest{}, ErrCorrupt
	}
	if binary.LittleEndian.Uint32(data[0:4]) != exactGenMagic {
		return exactManifest{}, ErrCorrupt
	}
	if binary.LittleEndian.Uint16(data[4:6]) != exactGenVersion {
		return exactManifest{}, ErrUnsupported
	}
	if crc32.ChecksumIEEE(data[:124]) != binary.LittleEndian.Uint32(data[124:128]) {
		return exactManifest{}, ErrCorrupt
	}
	algo := Algorithm(data[6])
	if !algo.Valid() {
		return exactManifest{}, ErrCorrupt
	}
	m := exactManifest{
		algorithm:     algo,
		targetFillPct: data[7],
		generation:    binary.LittleEndian.Uint64(data[8:16]),
		indexID:       binary.LittleEndian.Uint64(data[16:24]),
		flags:         binary.LittleEndian.Uint32(data[24:28]),
		blockCount:    uint64(binary.LittleEndian.Uint32(data[28:32])),
		liveCount:     binary.LittleEndian.Uint64(data[32:40]),
		walOffset:     int64(binary.LittleEndian.Uint64(data[56:64])),
		blockSize:     binary.LittleEndian.Uint32(data[64:68]),
	}
	dirOffset := binary.LittleEndian.Uint64(data[40:48])
	dirLen := binary.LittleEndian.Uint64(data[48:56])
	if dirOffset != exactGenHeaderSize || dirLen != m.blockCount*dirEntrySize {
		return exactManifest{}, ErrCorrupt
	}
	if m.blockSize < minBlockSize || m.blockSize > maxBlockSize {
		return exactManifest{}, ErrCorrupt
	}
	return m, nil
}

func exactGenerationPath(dir string, gen uint64) string {
	return filepath.Join(dir, fmt.Sprintf(exactGenPattern, gen))
}

// exactWALPath names the exact WAL for one epoch. Vacuum starts a fresh epoch
// instead of truncating in place, so reclaiming log space never requires an
// in-place offset reset.
func exactWALPath(dir string, epoch uint64) string {
	return filepath.Join(dir, fmt.Sprintf(exactWALPattern, epoch))
}

// newExact creates a persistent index in dir and publishes its initial
// checkpoint. An optional reader tracker lets Store share one tracker across
// its indexes.
func newExact(dir string, spec ExactSpec, cfg ExactConfig, durability Durability, readers ...*indexutil.ReaderTracker) (*ExactIndex, error) {
	if !durability.Valid() {
		return nil, ErrInvalidConfig
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	idx, err := newExactIndex(spec, cfg, readers...)
	if err != nil {
		return nil, err
	}
	wal, err := openExactWAL(exactWALPath(dir, 1), int(idx.config.MaxBatchEntries))
	if err != nil {
		_ = idx.Close()
		return nil, err
	}
	idx.persistence = persistedState{dir: dir, durability: durability, wal: wal, walEpoch: 1}
	idx.seq.Store(1)
	if err = idx.Checkpoint(); err != nil {
		_ = idx.Close()
		return nil, err
	}
	return idx, nil
}

// openExact maps the published generation and replays the WAL tail. Zero
// BlockSize and TargetFillPct values adopt the persisted generation's sizing
// so reopening a store keeps its write policy. An optional reader tracker lets
// Store share one tracker across its indexes.
func openExact(dir string, cfg ExactConfig, durability Durability, readers ...*indexutil.ReaderTracker) (*ExactIndex, error) {
	if !durability.Valid() {
		return nil, ErrInvalidConfig
	}
	raw := cfg
	manifest, err := readDiskManifest(filepath.Join(dir, "manifest"))
	if err != nil {
		return nil, err
	}
	genPath := exactGenerationPath(dir, manifest.generation)
	mapping, err := persist.MapFile(genPath, ErrCorrupt)
	if err != nil {
		return nil, err
	}
	view, header, err := decodeExactGeneration(mapping)
	if err != nil {
		_ = persist.Unmap(mapping)
		return nil, err
	}
	if header.generation != manifest.generation || header.indexID != manifest.indexID ||
		header.liveCount != manifest.liveCount || header.blockCount != manifest.blockCount ||
		header.algorithm != manifest.algorithm {
		_ = persist.Unmap(mapping)
		return nil, ErrCorrupt
	}
	if view.liveCount != manifest.liveCount || uint64(len(view.blocks)) != manifest.blockCount {
		_ = persist.Unmap(mapping)
		return nil, ErrCorrupt
	}
	if raw.BlockSize == 0 {
		cfg.BlockSize = header.blockSize
	}
	if raw.TargetFillPct == 0 {
		cfg.TargetFillPct = header.targetFillPct
	}
	cfg, err = cfg.withDefaults()
	if err != nil {
		_ = persist.Unmap(mapping)
		return nil, err
	}
	idx := &ExactIndex{
		config:    cfg,
		readers:   chooseReaders(readers),
		writes:    make(chan *exactBatch, exactWriteQueue),
		barriers:  make(chan chan barrierResult),
		stop:      make(chan struct{}),
		indexID:   manifest.indexID,
		flags:     manifest.flags,
		algorithm: manifest.algorithm,
		persistence: persistedState{
			dir: dir, durability: durability, mappings: [][]byte{mapping},
			persistedGen: manifest.generation, walEpoch: uint64(manifest.walEpoch),
		},
	}
	idx.view.Store(view)
	idx.generation.Store(view.generation)
	batches, maxSeq, err := replayExactWAL(exactWALPath(dir, uint64(manifest.walEpoch)), manifest.walOffset)
	if err != nil {
		_ = persist.Unmap(mapping)
		return nil, err
	}
	for _, batch := range batches {
		var adds, removes []ExactEntry
		for _, rec := range batch {
			e := ExactEntry{Hash: rec.hash, ImageID: rec.imageID}
			if rec.op == exactWALOpAdd {
				adds = append(adds, e)
			} else {
				removes = append(removes, e)
			}
		}
		idx.applyWithoutWAL(adds, removes)
	}
	idx.seq.Store(maxSeq + 1)
	wal, err := openExactWAL(exactWALPath(dir, uint64(manifest.walEpoch)), int(cfg.MaxBatchEntries))
	if err != nil {
		_ = persist.Unmap(mapping)
		return nil, err
	}
	idx.persistence.wal = wal
	idx.startWriterLoop()
	return idx, nil
}

// decodeExactGeneration validates a mapped generation file and builds a view whose
// record slices reference the mapping.
func decodeExactGeneration(mapping []byte) (*exactIndexView, exactManifest, error) {
	header, err := decodeExactGenHeader(mapping)
	if err != nil {
		return nil, exactManifest{}, err
	}
	if uint64(len(mapping)) < exactGenHeaderSize+header.blockCount*dirEntrySize {
		return nil, exactManifest{}, ErrCorrupt
	}
	dirBase := uint64(exactGenHeaderSize)
	blocks := make([]blockHandle, 0, header.blockCount)
	offset := dirBase + header.blockCount*dirEntrySize
	var live uint64
	var prevFirst *Hash
	for i := uint64(0); i < header.blockCount; i++ {
		entry := mapping[dirBase+i*dirEntrySize : dirBase+(i+1)*dirEntrySize]
		var first Hash
		copy(first[:], entry[:32])
		blockOffset := binary.LittleEndian.Uint64(entry[32:40])
		count := binary.LittleEndian.Uint32(entry[40:44])
		if blockOffset < offset || blockOffset+blockHeaderSize > uint64(len(mapping)) {
			return nil, exactManifest{}, ErrCorrupt
		}
		if blockOffset != offset {
			return nil, exactManifest{}, ErrCorrupt
		}
		blockImage := mapping[blockOffset:]
		got, err := decodeBlockHeader(blockImage)
		if err != nil {
			return nil, exactManifest{}, err
		}
		if got != count {
			return nil, exactManifest{}, ErrCorrupt
		}
		payload := blockImage[blockHeaderSize : blockHeaderSize+uint64(count)*RecordSize]
		if len(payload) > 0 {
			var actualFirst Hash
			copy(actualFirst[:], payload[:32])
			if actualFirst != first {
				return nil, exactManifest{}, ErrCorrupt
			}
		} else if count != 0 {
			return nil, exactManifest{}, ErrCorrupt
		}
		if prevFirst != nil && compareHash(*prevFirst, first) > 0 {
			return nil, exactManifest{}, ErrCorrupt
		}
		h := first
		prevFirst = &h
		blocks = append(blocks, blockHandle{first: first, records: payload})
		live += uint64(count)
		offset = blockOffset + blockHeaderSize + uint64(count)*RecordSize
	}
	if live != header.liveCount {
		return nil, exactManifest{}, ErrCorrupt
	}
	if offset > uint64(len(mapping)) {
		return nil, exactManifest{}, ErrCorrupt
	}
	return &exactIndexView{generation: header.generation, liveCount: live, blocks: blocks}, header, nil
}

// Checkpoint persists the current view into a new generation file and
// atomically publishes a manifest pointing at it.
func (i *ExactIndex) Checkpoint() error {
	i.lifecycle.RLock()
	defer i.lifecycle.RUnlock()
	return i.checkpointLocked()
}

func (i *ExactIndex) checkpointLocked() error {
	if i.closed.Load() {
		return ErrClosed
	}
	if err := i.failedErr(); err != nil {
		return err
	}
	i.persistence.mu.Lock()
	defer i.persistence.mu.Unlock()
	view := i.view.Load()
	if view.generation == i.persistence.persistedGen {
		return nil
	}
	// Checkpoint persists effective state: staged overlay sets fold into
	// a file-only block set (the live view keeps its overlay). Staged
	// sets are canonical against base by construction, so the fold merges
	// straight in. The WAL offset below covers the staged batches, so
	// reopen replays nothing twice and loses nothing.
	fileView := view
	if view.overlay.ids() > 0 {
		folded := applyEffective(baseOnlyView(view), view.overlay.adds, view.overlay.removes, i.config)
		folded.generation = view.generation
		fileView = folded
	}
	walOffset, err := i.persistence.wal.Offset()
	if err != nil {
		return err
	}
	manifest := exactManifest{
		generation: view.generation, indexID: i.indexID, liveCount: fileView.liveCount,
		blockCount: uint64(len(fileView.blocks)), walOffset: walOffset,
		flags: i.flags, blockSize: i.config.BlockSize,
		algorithm: i.algorithm, targetFillPct: i.config.TargetFillPct,
		walEpoch: uint32(i.persistence.walEpoch),
	}
	if err := writeExactGenerationFile(i.persistence.dir, manifest, fileView); err != nil {
		return err
	}
	if err := persist.WriteManifest(i.persistence.dir, encodeManifest(manifest)); err != nil {
		return err
	}
	i.persistence.persistedGen = view.generation
	return nil
}

func writeExactGenerationFile(dir string, manifest exactManifest, view *exactIndexView) error {
	header := make([]byte, exactGenHeaderSize)
	encodeExactGenHeader(header, manifest)
	// Precompute block offsets: directory follows header, blocks follow it.
	offset := uint64(exactGenHeaderSize) + manifest.blockCount*dirEntrySize
	dirBuf := make([]byte, manifest.blockCount*dirEntrySize)
	for idx, b := range view.blocks {
		count := uint32(b.count())
		base := idx * dirEntrySize
		copy(dirBuf[base:base+32], b.first[:])
		binary.LittleEndian.PutUint64(dirBuf[base+32:base+40], offset)
		binary.LittleEndian.PutUint32(dirBuf[base+40:base+44], count)
		binary.LittleEndian.PutUint32(dirBuf[base+44:base+48], 0)
		offset += blockHeaderSize + uint64(count)*RecordSize
	}
	blockHeader := make([]byte, blockHeaderSize)
	return writeAtomicFile(exactGenerationPath(dir, manifest.generation), func(file *os.File) error {
		if _, err := file.Write(header); err != nil {
			return err
		}
		if _, err := file.Write(dirBuf); err != nil {
			return err
		}
		for _, b := range view.blocks {
			encodeBlockHeader(blockHeader, uint32(b.count()), crc32.ChecksumIEEE(b.records))
			if _, err := file.Write(blockHeader); err != nil {
				return err
			}
			if len(b.records) != 0 {
				if _, err := file.Write(b.records); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

// Vacuum repacks live records densely, checkpoints, and reclaims obsolete
// generations. Logical contents never change.
//
// Vacuum holds the exclusive lifecycle lock, so no new write batch can
// arrive; the barrier first drains and applies everything already queued,
// leaving the writer loop idle and the view stable for the repack. The fresh
// WAL epoch therefore starts empty with a zero offset: every record ever
// appended is already inside the repacked generation file.
func (i *ExactIndex) Vacuum() error {
	if i.closed.Load() {
		return ErrClosed
	}
	i.lifecycle.Lock()
	defer i.lifecycle.Unlock()
	if i.closed.Load() {
		return ErrClosed
	}
	if err := i.failedErr(); err != nil {
		return err
	}
	response := make(chan barrierResult, 1)
	select {
	case i.barriers <- response:
	case <-i.stop:
		return ErrClosed
	}
	if res := <-response; res.err != nil {
		return res.err
	}
	// Repack live records at target occupancy, folding any staged
	// overlay in: staged adds join the repack, staged removes drop out via
	// a merge pointer (both sides ordered, so no map and no transient
	// block decodes). The published view carries no overlay.
	current := i.view.Load()
	var ovAdds, ovRemoves []ExactEntry
	if current.overlay != nil {
		ovAdds, ovRemoves = current.overlay.adds, current.overlay.removes
	}
	all := make([]ExactEntry, 0, current.liveCount+uint64(len(ovAdds)))
	ri := 0
	for _, b := range current.blocks {
		n := len(b.records) / RecordSize
		for k := 0; k < n; k++ {
			e := decodeRecord(b.records[k*RecordSize : (k+1)*RecordSize])
			for ri < len(ovRemoves) && compareEntry(ovRemoves[ri], e) < 0 {
				ri++
			}
			if ri < len(ovRemoves) && ovRemoves[ri] == e {
				ri++
				continue
			}
			all = append(all, e)
		}
	}
	all = append(all, ovAdds...)
	sortEntries(all)
	maxRecords := maxRecordsPerBlock(i.config.BlockSize)
	target := targetRecordsPerBlock(i.config.BlockSize, i.config.TargetFillPct)
	parts := partitionEntries(all, maxRecords, target)
	blocks := make([]blockHandle, len(parts))
	for k, part := range parts {
		rec := make([]byte, len(part)*RecordSize)
		for j, e := range part {
			encodeRecord(rec[j*RecordSize:(j+1)*RecordSize], e)
		}
		if len(part) > 0 {
			blocks[k] = blockHandle{first: part[0].Hash, records: rec}
		}
	}
	// Drop empty trailing handles when the index is empty.
	if len(all) == 0 {
		blocks = nil
	}
	// Only publish when repacking changes the block layout; otherwise keep
	// the current generation so a no-op vacuum does not churn generations.
	needsPublish := len(blocks) != len(current.blocks)
	if !needsPublish {
		for k := range blocks {
			if blocks[k].first != current.blocks[k].first || len(blocks[k].records) != len(current.blocks[k].records) {
				needsPublish = true
				break
			}
		}
	}
	if needsPublish {
		next := &exactIndexView{generation: current.generation + 1, liveCount: uint64(len(all)), blocks: blocks}
		i.view.Store(next)
		i.generation.Store(next.generation)
	}
	i.persistence.mu.Lock()
	defer i.persistence.mu.Unlock()
	view := i.view.Load()
	manifest := exactManifest{
		generation: view.generation, indexID: i.indexID, liveCount: view.liveCount,
		blockCount: uint64(len(view.blocks)),
		flags:      i.flags, blockSize: i.config.BlockSize,
		algorithm: i.algorithm, targetFillPct: i.config.TargetFillPct,
	}
	// Persist the repacked layout when it changed, then roll to a fresh WAL
	// epoch and publish a manifest that references it with a zero offset.
	// The barrier above drained the loop and the exclusive lock admits no
	// new batch, so the generation file holds every record ever appended
	// and the new epoch legitimately starts empty. A crash before the
	// manifest leaves the old generation and WAL authoritative; a crash
	// after it leaves a trivially consistent empty WAL. The old WAL file is
	// removed only after the new root is durable. The handle swap is direct
	// because the writer loop is idle with no possible sender.
	if view.generation != i.persistence.persistedGen {
		if err := writeExactGenerationFile(i.persistence.dir, manifest, view); err != nil {
			return err
		}
	}
	epoch := i.persistence.walEpoch + 1
	newWAL, err := openExactWAL(exactWALPath(i.persistence.dir, epoch), int(i.config.MaxBatchEntries))
	if err != nil {
		return err
	}
	manifest.walOffset = 0
	manifest.walEpoch = uint32(epoch)
	if err = persist.WriteManifest(i.persistence.dir, encodeManifest(manifest)); err != nil {
		_ = newWAL.Close()
		return err
	}
	oldWAL := i.persistence.wal
	i.persistence.wal = newWAL
	i.persistence.walEpoch = epoch
	i.persistence.persistedGen = view.generation
	_ = oldWAL.Close()
	i.readers.WaitForPriorReaders()
	keep := exactGenerationPath(i.persistence.dir, i.persistence.persistedGen)
	if err := persist.RemoveObsolete(i.persistence.dir, exactGenGlob, keep); err != nil {
		return err
	}
	if err := persist.RemoveObsolete(i.persistence.dir, exactWALGlob, exactWALPath(i.persistence.dir, epoch)); err != nil {
		return err
	}
	// The repacked view holds fresh heap blocks after the reader drain above,
	// so the retired generation mapping is unreachable and can be released.
	// Without a repack the published view still references the mapping, which
	// stays alive until Close.
	if needsPublish {
		for _, m := range i.persistence.mappings {
			_ = persist.Unmap(m)
		}
		i.persistence.mappings = nil
	}
	// Remove stray tmp files from crashed checkpoints.
	matches, _ := filepath.Glob(filepath.Join(i.persistence.dir, exactGenGlob+".tmp"))
	for _, p := range matches {
		_ = os.Remove(p)
	}
	return nil
}

// OnDiskBytes reports the total bytes of generation files, the WAL, and the
// manifest.
func (i *ExactIndex) OnDiskBytes() uint64 {
	return indexutil.DirSize(i.persistence.dir, exactGenGlob, exactWALGlob, "manifest")
}

// StorageAmplification returns the on-disk bytes per live logical byte.
func (i *ExactIndex) StorageAmplification() float64 {
	physical := i.OnDiskBytes()
	if physical == 0 {
		physical = i.PhysicalBytes()
	}
	return storageAmplification(i.LiveBytes(), physical)
}
