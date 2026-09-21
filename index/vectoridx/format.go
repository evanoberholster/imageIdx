package vectoridx

import (
	"encoding/binary"
	"hash/crc32"
	"os"
	"path/filepath"
	"sort"

	"github.com/evanoberholster/imageIdx/internal/indexutil"
)

// Single-file multi-region format (VectorIdx.MD section 8).
//
// All integers are little-endian with explicitly sized fields; no Go struct
// is ever cast onto disk. The header is a fixed 256 bytes with a trailing
// CRC32 over bytes [0:252]. The region directory follows at offset 256 with
// 32-byte entries; region payloads follow packed and 8-byte aligned.
//
// Region kinds: 1=NodeMeta, 2=Vectors, 3=Graph, 4=Tombstones.

const (
	vectorMagic0 = 'V'
	vectorMagic1 = 'C'
	vectorMagic2 = 'T'
	vectorMagic3 = 'R'
	vectorMagic4 = 'I'
	vectorMagic5 = 'D'
	vectorMagic6 = 'X'
	vectorMagic7 = '1'

	vectorFormatVersion = 1
	vectorHeaderSize    = 256
	vectorMetricL2      = 1

	regionKindNodeMeta   = 1
	regionKindVectors    = 2
	regionKindGraph      = 3
	regionKindTombstones = 4

	regionEntrySize = 32
)

var vectorMagic = [8]byte{
	vectorMagic0, vectorMagic1, vectorMagic2, vectorMagic3,
	vectorMagic4, vectorMagic5, vectorMagic6, vectorMagic7,
}

type fileHeader struct {
	generation     uint64
	typeID         TypeID
	elem           ElementType
	metric         uint8
	dims           uint16
	m              uint16
	m0             uint16
	efConstruction uint32
	efSearch       uint32
	entry          NodeID
	hasEntry       bool
	maxLevel       uint16
	liveCount      uint64
	tombstoneCount uint64
	checkpointLSN  uint64
}

type fileRegion struct {
	kind     uint16
	flags    uint16
	offset   uint64
	length   uint64
	checksum uint64
}

func encodeHeader(dst []byte, h fileHeader) {
	copy(dst[0:8], vectorMagic[:])
	binary.LittleEndian.PutUint32(dst[8:12], vectorFormatVersion)
	binary.LittleEndian.PutUint32(dst[12:16], vectorHeaderSize)
	binary.LittleEndian.PutUint64(dst[16:24], h.generation)
	binary.LittleEndian.PutUint16(dst[24:26], uint16(h.typeID))
	dst[26] = byte(h.elem)
	dst[27] = h.metric
	binary.LittleEndian.PutUint16(dst[28:30], h.dims)
	binary.LittleEndian.PutUint16(dst[30:32], h.m)
	binary.LittleEndian.PutUint16(dst[32:34], h.m0)
	binary.LittleEndian.PutUint32(dst[34:38], h.efConstruction)
	binary.LittleEndian.PutUint32(dst[38:42], h.efSearch)
	binary.LittleEndian.PutUint32(dst[42:46], uint32(h.entry))
	if h.hasEntry {
		binary.LittleEndian.PutUint16(dst[46:48], 1)
	} else {
		binary.LittleEndian.PutUint16(dst[46:48], 0)
	}
	binary.LittleEndian.PutUint16(dst[48:50], h.maxLevel)
	binary.LittleEndian.PutUint64(dst[50:58], h.liveCount)
	binary.LittleEndian.PutUint64(dst[58:66], h.tombstoneCount)
	binary.LittleEndian.PutUint64(dst[66:74], vectorHeaderSize) // region dir offset
	binary.LittleEndian.PutUint32(dst[74:78], 4)                // region count
	binary.LittleEndian.PutUint64(dst[78:86], h.checkpointLSN)
	for i := 86; i < 252; i++ {
		dst[i] = 0
	}
	binary.LittleEndian.PutUint32(dst[252:256], crc32.ChecksumIEEE(dst[:252]))
}

func decodeHeader(data []byte) (fileHeader, error) {
	var h fileHeader
	if len(data) < vectorHeaderSize {
		return h, ErrCorruptStorage
	}
	for i := range 8 {
		if data[i] != vectorMagic[i] {
			return h, ErrCorruptStorage
		}
	}
	if binary.LittleEndian.Uint32(data[8:12]) != vectorFormatVersion {
		return h, ErrCorruptStorage
	}
	if binary.LittleEndian.Uint32(data[12:16]) != vectorHeaderSize {
		return h, ErrCorruptStorage
	}
	if crc32.ChecksumIEEE(data[:252]) != binary.LittleEndian.Uint32(data[252:256]) {
		return h, ErrCorruptStorage
	}
	h.generation = binary.LittleEndian.Uint64(data[16:24])
	h.typeID = TypeID(binary.LittleEndian.Uint16(data[24:26]))
	h.elem = ElementType(data[26])
	h.metric = data[27]
	h.dims = binary.LittleEndian.Uint16(data[28:30])
	h.m = binary.LittleEndian.Uint16(data[30:32])
	h.m0 = binary.LittleEndian.Uint16(data[32:34])
	h.efConstruction = binary.LittleEndian.Uint32(data[34:38])
	h.efSearch = binary.LittleEndian.Uint32(data[38:42])
	h.entry = NodeID(binary.LittleEndian.Uint32(data[42:46]))
	h.hasEntry = binary.LittleEndian.Uint16(data[46:48]) == 1
	h.maxLevel = binary.LittleEndian.Uint16(data[48:50])
	h.liveCount = binary.LittleEndian.Uint64(data[50:58])
	h.tombstoneCount = binary.LittleEndian.Uint64(data[58:66])
	if binary.LittleEndian.Uint64(data[66:74]) != vectorHeaderSize {
		return h, ErrCorruptStorage
	}
	if binary.LittleEndian.Uint32(data[74:78]) != 4 {
		return h, ErrCorruptStorage
	}
	h.checkpointLSN = binary.LittleEndian.Uint64(data[78:86])
	if h.metric != vectorMetricL2 {
		return h, ErrCorruptStorage
	}
	switch h.elem {
	case ElementInt8, ElementUint8, ElementInt16, ElementUint16, ElementFloat32:
	default:
		return h, ErrCorruptStorage
	}
	if h.dims != Dim512 && h.dims != Dim768 && h.dims != Dim1024 {
		return h, ErrCorruptStorage
	}
	return h, nil
}

func encodeRegionEntry(dst []byte, r fileRegion) {
	binary.LittleEndian.PutUint16(dst[0:2], r.kind)
	binary.LittleEndian.PutUint16(dst[2:4], r.flags)
	binary.LittleEndian.PutUint64(dst[4:12], r.offset)
	binary.LittleEndian.PutUint64(dst[12:20], r.length)
	binary.LittleEndian.PutUint64(dst[20:28], r.checksum)
	binary.LittleEndian.PutUint32(dst[28:32], 0)
}

func decodeRegionEntry(src []byte) fileRegion {
	return fileRegion{
		kind:     binary.LittleEndian.Uint16(src[0:2]),
		flags:    binary.LittleEndian.Uint16(src[2:4]),
		offset:   binary.LittleEndian.Uint64(src[4:12]),
		length:   binary.LittleEndian.Uint64(src[12:20]),
		checksum: binary.LittleEndian.Uint64(src[20:28]),
	}
}

// decodeFile validates the header and all region bounds/checksums, returning
// zero-copy region slices. No mmap is retained; Open copies payloads into
// heap storage (V1 simplification documented in doc.go).
func decodeFile(data []byte) (fileHeader, [][]byte, error) {
	h, err := decodeHeader(data)
	if err != nil {
		return h, nil, err
	}
	dirOff := uint64(vectorHeaderSize)
	dirLen := uint64(4 * regionEntrySize)
	dirEnd, ok := indexutil.CheckedEnd(dirOff, 1, dirLen, uint64(len(data)))
	if !ok || dirEnd > uint64(len(data)) {
		return h, nil, ErrCorruptStorage
	}
	regions := make([][]byte, 0, 4)
	seen := map[uint16]bool{}
	for i := range 4 {
		entry := decodeRegionEntry(data[dirOff+uint64(i*regionEntrySize):])
		if seen[entry.kind] {
			return h, nil, ErrCorruptStorage
		}
		seen[entry.kind] = true
		end, ok := indexutil.CheckedEnd(entry.offset, 1, entry.length, uint64(len(data)))
		if !ok || end > uint64(len(data)) {
			return h, nil, ErrCorruptStorage
		}
		payload := data[entry.offset : entry.offset+entry.length]
		if uint64(crc32.ChecksumIEEE(payload)) != (entry.checksum & 0xffffffff) {
			return h, nil, ErrCorruptStorage
		}
		regions = append(regions, payload)
	}
	// Directory order must be NodeMeta, Vectors, Graph, Tombstones.
	kinds := []uint16{regionKindNodeMeta, regionKindVectors, regionKindGraph, regionKindTombstones}
	for i, want := range kinds {
		entry := decodeRegionEntry(data[dirOff+uint64(i*regionEntrySize):])
		if entry.kind != want {
			return h, nil, ErrCorruptStorage
		}
	}
	return h, regions, nil
}

// loadRegions populates index state from validated region payloads.
func (v *VectorIdx) loadRegions(h fileHeader, regions [][]byte) error {
	nodeMeta, vectors, graph, tombstones := regions[0], regions[1], regions[2], regions[3]
	if len(nodeMeta)%8 != 0 {
		return ErrCorruptStorage
	}
	count := len(nodeMeta) / 8
	if h.liveCount+h.tombstoneCount != uint64(count) {
		return ErrCorruptStorage
	}
	vecBytes := v.vecBytes
	if len(vectors) != count*vecBytes {
		return ErrCorruptStorage
	}
	v.ids = make([]NodeID, 0, count)
	v.vectors = make([]byte, 0, len(vectors))
	v.levels = make([]int, 0, count)
	v.links = make([][][]NodeID, 0, count)
	v.sortedIDs = make([]NodeID, 0, count)
	v.sortedOrd = make([]int, 0, count)
	for i := range count {
		off := i * 8
		id := NodeID(binary.LittleEndian.Uint32(nodeMeta[off : off+4]))
		level := int(binary.LittleEndian.Uint16(nodeMeta[off+4 : off+6]))
		if level < 0 || level > 64 {
			return ErrCorruptStorage
		}
		v.ids = append(v.ids, id)
		v.levels = append(v.levels, level)
		v.links = append(v.links, make([][]NodeID, level+1))
	}
	v.vectors = append(v.vectors, vectors...)
	// Graph payload: per node u16 numLevels then per level u16 degree + ids.
	pos := 0
	for ord := range count {
		if pos+2 > len(graph) {
			return ErrCorruptStorage
		}
		numLevels := int(binary.LittleEndian.Uint16(graph[pos : pos+2]))
		pos += 2
		if numLevels != v.levels[ord]+1 {
			return ErrCorruptStorage
		}
		for l := range numLevels {
			if pos+2 > len(graph) {
				return ErrCorruptStorage
			}
			deg := int(binary.LittleEndian.Uint16(graph[pos : pos+2]))
			pos += 2
			if pos+deg*4 > len(graph) {
				return ErrCorruptStorage
			}
			nbs := make([]NodeID, deg)
			for j := range deg {
				nbs[j] = NodeID(binary.LittleEndian.Uint32(graph[pos+j*4 : pos+j*4+4]))
			}
			pos += deg * 4
			v.links[ord][l] = nbs
		}
	}
	if pos != len(graph) {
		return ErrCorruptStorage
	}
	if len(tombstones) < 8 {
		return ErrCorruptStorage
	}
	tombCount := binary.LittleEndian.Uint64(tombstones[0:8])
	if uint64(len(tombstones)) != 8+tombCount*4 {
		return ErrCorruptStorage
	}
	if tombCount != h.tombstoneCount {
		return ErrCorruptStorage
	}
	for i := range tombCount {
		id := NodeID(binary.LittleEndian.Uint32(tombstones[8+i*4:]))
		v.tombstones[id] = struct{}{}
	}
	// Rebuild the sorted locator from node IDs.
	type pair struct {
		id  NodeID
		ord int
	}
	pairs := make([]pair, count)
	for ord, id := range v.ids {
		pairs[ord] = pair{id: id, ord: ord}
	}
	sort.Slice(pairs, func(i, j int) bool { return pairs[i].id < pairs[j].id })
	for i := 1; i < len(pairs); i++ {
		if pairs[i-1].id == pairs[i].id {
			return ErrCorruptStorage
		}
	}
	for _, p := range pairs {
		v.sortedIDs = append(v.sortedIDs, p.id)
		v.sortedOrd = append(v.sortedOrd, p.ord)
	}
	v.entry = h.entry
	v.hasEntry = h.hasEntry
	v.maxLevel = int(h.maxLevel)
	if v.hasEntry {
		if _, ok := v.locateLocked(v.entry); !ok {
			return ErrCorruptStorage
		}
	}
	return nil
}

// checkpointLocked serializes the current state and atomically publishes it.
// Caller holds the writer lock.
func (v *VectorIdx) checkpointLocked() error {
	// NodeMeta region: 8 bytes per node (id u32, level u16, reserved u16).
	nodeMeta := make([]byte, len(v.ids)*8)
	for i, id := range v.ids {
		binary.LittleEndian.PutUint32(nodeMeta[i*8:], uint32(id))
		binary.LittleEndian.PutUint16(nodeMeta[i*8+4:], uint16(v.levels[i]))
	}
	// Graph region.
	graphSize := 0
	for _, layerLinks := range v.links {
		graphSize += 2
		for _, nbs := range layerLinks {
			graphSize += 2 + len(nbs)*4
		}
	}
	graph := make([]byte, 0, graphSize)
	var u16buf [2]byte
	var u32buf [4]byte
	for _, layerLinks := range v.links {
		binary.LittleEndian.PutUint16(u16buf[:], uint16(len(layerLinks)))
		graph = append(graph, u16buf[:]...)
		for _, nbs := range layerLinks {
			binary.LittleEndian.PutUint16(u16buf[:], uint16(len(nbs)))
			graph = append(graph, u16buf[:]...)
			for _, nb := range nbs {
				binary.LittleEndian.PutUint32(u32buf[:], uint32(nb))
				graph = append(graph, u32buf[:]...)
			}
		}
	}
	// Tombstones region: u64 count + sorted u32 ids.
	tombIDs := make([]NodeID, 0, len(v.tombstones))
	for id := range v.tombstones {
		tombIDs = append(tombIDs, id)
	}
	sortNodeIDs(tombIDs)
	tomb := make([]byte, 8+len(tombIDs)*4)
	binary.LittleEndian.PutUint64(tomb[0:8], uint64(len(tombIDs)))
	for i, id := range tombIDs {
		binary.LittleEndian.PutUint32(tomb[8+i*4:], uint32(id))
	}
	v.checkpointLSN = v.lsn
	h := fileHeader{
		generation:     v.gen.Load(),
		typeID:         v.cfg.TypeID,
		elem:           v.cfg.ElementType,
		metric:         vectorMetricL2,
		dims:           v.cfg.Dimensions,
		m:              v.cfg.M,
		m0:             v.cfg.M0,
		efConstruction: v.cfg.EfConstruction,
		efSearch:       v.cfg.EfSearch,
		entry:          v.entry,
		hasEntry:       v.hasEntry,
		maxLevel:       uint16(v.maxLevel),
		liveCount:      uint64(len(v.ids) - len(v.tombstones)),
		tombstoneCount: uint64(len(v.tombstones)),
		checkpointLSN:  v.checkpointLSN,
	}
	payloads := [][]byte{nodeMeta, append([]byte(nil), v.vectors...), graph, tomb}
	kinds := []uint16{regionKindNodeMeta, regionKindVectors, regionKindGraph, regionKindTombstones}
	dirOff := uint64(vectorHeaderSize)
	total := dirOff + uint64(4*regionEntrySize)
	offsets := make([]uint64, 4)
	for i, p := range payloads {
		total = indexutil.Align(total, 8)
		offsets[i] = total
		total += uint64(len(p))
	}
	file := make([]byte, total)
	encodeHeader(file[0:vectorHeaderSize], h)
	for i, p := range payloads {
		r := fileRegion{
			kind:     kinds[i],
			offset:   offsets[i],
			length:   uint64(len(p)),
			checksum: uint64(crc32.ChecksumIEEE(p)),
		}
		encodeRegionEntry(file[dirOff+uint64(i*regionEntrySize):], r)
		copy(file[offsets[i]:], p)
	}
	dir := filepath.Dir(v.path)
	base := filepath.Base(v.path)
	if err := indexutil.WriteFileAtomic(dir, base+".tmp", base, file, 0o644); err != nil {
		return err
	}
	// Truncate the WAL to the checkpoint: everything replayable is durable
	// in the new generation.
	if v.wal != nil {
		if err := v.wal.Truncate(0); err != nil {
			return err
		}
		if _, err := v.wal.Seek(0, 0); err != nil {
			return err
		}
	}
	return nil
}

func sortNodeIDs(ids []NodeID) {
	for i := 1; i < len(ids); i++ {
		for j := i; j > 0 && ids[j-1] > ids[j]; j-- {
			ids[j-1], ids[j] = ids[j], ids[j-1]
		}
	}
}

// VerifyFile validates the file at path without opening an index: header,
// region bounds, and checksums. It is the offline integrity checker that
// complements the fast structural validation in Open.
func VerifyFile(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	_, _, err = decodeFile(data)
	return err
}
