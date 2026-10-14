package vectoridx

import (
	"encoding/binary"
	"hash/crc32"
	"os"
	"path/filepath"
	"sort"
	"sync/atomic"

	schema "github.com/evanoberholster/imageIdx/schema"
	"github.com/evanoberholster/imageIdx/internal/storage"
)

var _ = atomic.LoadInt32

// Single-file multi-region format (VectorIdx.MD section 8).
//
// All integers are little-endian with explicitly sized fields; no Go struct
// is ever cast onto disk. The header is a fixed 256 bytes with a trailing
// CRC32 over bytes [0:252]. The region directory follows at offset 256 with
// 32-byte entries; region payloads follow packed and 8-byte aligned.
//
// Region kinds: 1=NodeMeta, 2=Vectors, 3=Graph, 4=Tombstones.
//
// The 16-bit identity field at header offset 24 is a plain schema.Namespace.
// The format is byte-identical to the era when it held a packed SpaceID: the
// field was always one validated uint16, so no version bump is needed and old
// files reopen with their old packed value read as a namespace.

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
	namespace      schema.Namespace
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
	binary.LittleEndian.PutUint16(dst[24:26], uint16(h.namespace))
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
	h.namespace = schema.Namespace(binary.LittleEndian.Uint16(data[24:26]))
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
	if h.metric != vectorMetricL2 && h.metric != MetricCosine && h.metric != MetricDot {
		return h, ErrCorruptStorage
	}
	switch h.elem {
	case ElementInt8, ElementUint8, ElementInt16, ElementUint16, ElementFloat32:
	default:
		return h, ErrCorruptStorage
	}
	if !Dimensions(h.dims).Valid() {
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
	dirEnd, ok := storage.CheckedEnd(dirOff, 1, dirLen, uint64(len(data)))
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
		end, ok := storage.CheckedEnd(entry.offset, 1, entry.length, uint64(len(data)))
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

// loadRegions populates the published snapshot from validated region payloads.
// Caller holds no snapshot yet (construction/recovery); the built snapshot is
// stored directly.
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
	snap := v.view.Load()
	ids := make([]NodeID, 0, count)
	levels := make([]int, 0, count)
	linkHolders := make([]*atomic.Pointer[nodeLayers], 0, count)
	for i := range count {
		off := i * 8
		id := NodeID(binary.LittleEndian.Uint32(nodeMeta[off : off+4]))
		level := int(binary.LittleEndian.Uint16(nodeMeta[off+4 : off+6]))
		if level < 0 || level > 64 {
			return ErrCorruptStorage
		}
		ids = append(ids, id)
		levels = append(levels, level)
		holder := &atomic.Pointer[nodeLayers]{}
		holder.Store(&nodeLayers{layers: make([][]NodeID, level+1)})
		linkHolders = append(linkHolders, holder)
	}
	vec := append([]byte(nil), vectors...)
	// Graph payload: per node u16 numLevels then per level u16 degree + ids.
	pos := 0
	for ord := range count {
		if pos+2 > len(graph) {
			return ErrCorruptStorage
		}
		numLevels := int(binary.LittleEndian.Uint16(graph[pos : pos+2]))
		pos += 2
		if numLevels != levels[ord]+1 {
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
			linkHolders[ord].Load().layers[l] = nbs
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
	tombSorted := make([]NodeID, 0, tombCount)
	for i := range tombCount {
		id := NodeID(binary.LittleEndian.Uint32(tombstones[8+i*4:]))
		tombSorted = append(tombSorted, id)
	}
	if !sort.SliceIsSorted(tombSorted, func(i, j int) bool { return tombSorted[i] < tombSorted[j] }) {
		return ErrCorruptStorage
	}
	tomb, tombSorted := rebuildTombeset(tombSorted)
	if uint64(len(tombSorted)) != tombCount {
		return ErrCorruptStorage
	}
	// Rebuild the locator from node IDs: dense when feasible, else sorted.
	ns := &vectorSnapshot{
		cfg:        snap.cfg,
		dist:       v.dist,
		vecBytes:   v.vecBytes,
		elemSize:   v.elemSize,
		dims:       v.dims,
		ids:        ids,
		vectors:    vec,
		levels:     levels,
		links:      linkHolders,
		tomb:       tomb,
		tombSorted: tombSorted,
		entry:      h.entry,
		hasEntry:   h.hasEntry,
		maxLevel:   int(h.maxLevel),
		gen:        h.generation + 1,
	}
	type pair struct {
		id  NodeID
		ord int
	}
	pairs := make([]pair, count)
	for ord, id := range ids {
		pairs[ord] = pair{id: id, ord: ord}
	}
	sort.Slice(pairs, func(i, j int) bool { return pairs[i].id < pairs[j].id })
	for i := 1; i < len(pairs); i++ {
		if pairs[i-1].id == pairs[i].id {
			return ErrCorruptStorage
		}
	}
	var lo, hi NodeID
	if count > 0 {
		lo, hi = pairs[0].id, pairs[len(pairs)-1].id
	}
	if count > 0 && denseFeasible(lo, hi, count) {
		ns.rebuildDenseWith(lo, hi)
	} else {
		ns.sortedIDs = make([]NodeID, 0, count)
		ns.sortedOrd = make([]int, 0, count)
		for _, p := range pairs {
			ns.sortedIDs = append(ns.sortedIDs, p.id)
			ns.sortedOrd = append(ns.sortedOrd, p.ord)
		}
	}
	if ns.hasEntry {
		if _, ok := ns.locate(ns.entry); !ok {
			return ErrCorruptStorage
		}
	}
	v.view.Store(ns)
	return nil
}

// writeBaseLocked serializes the published snapshot and atomically publishes
// it as the base generation. Caller holds the writer lock. It does not touch
// the WAL or the manifest: Vacuum wraps it with both, while Create publishes
// the empty base before any delta exists.
func (v *VectorIdx) writeBaseLocked() error {
	snap := v.view.Load()
	// NodeMeta region: 8 bytes per node (id u32, level u16, reserved u16).
	nodeMeta := make([]byte, len(snap.ids)*8)
	for i, id := range snap.ids {
		binary.LittleEndian.PutUint32(nodeMeta[i*8:], uint32(id))
		binary.LittleEndian.PutUint16(nodeMeta[i*8+4:], uint16(snap.levels[i]))
	}
	// Graph region.
	graphSize := 0
	for _, holder := range snap.links {
		g := holder.Load()
		graphSize += 2
		for _, nbs := range g.layers {
			graphSize += 2 + len(nbs)*4
		}
	}
	graph := make([]byte, 0, graphSize)
	var u16buf [2]byte
	var u32buf [4]byte
	for _, holder := range snap.links {
		g := holder.Load()
		binary.LittleEndian.PutUint16(u16buf[:], uint16(len(g.layers)))
		graph = append(graph, u16buf[:]...)
		for _, nbs := range g.layers {
			binary.LittleEndian.PutUint16(u16buf[:], uint16(len(nbs)))
			graph = append(graph, u16buf[:]...)
			for _, nb := range nbs {
				binary.LittleEndian.PutUint32(u32buf[:], uint32(nb))
				graph = append(graph, u32buf[:]...)
			}
		}
	}
	// Tombstones region: u64 count + sorted u32 ids.
	tombIDs := snap.tombSorted
	tomb := make([]byte, 8+len(tombIDs)*4)
	binary.LittleEndian.PutUint64(tomb[0:8], uint64(len(tombIDs)))
	for i, id := range tombIDs {
		binary.LittleEndian.PutUint32(tomb[8+i*4:], uint32(id))
	}
	v.checkpointLSN = v.lsn
	v.checkpointLSNPub.Store(v.checkpointLSN)
	h := fileHeader{
		generation:     v.gen.Load(),
		namespace:      snap.cfg.Namespace,
		elem:           snap.cfg.ElementType,
		metric:         snap.cfg.Metric,
		dims:           uint16(snap.cfg.Dimensions),
		m:              snap.cfg.M,
		m0:             snap.cfg.M0,
		efConstruction: snap.cfg.EfConstruction,
		efSearch:       snap.cfg.EfSearch,
		entry:          snap.entry,
		hasEntry:       snap.hasEntry,
		maxLevel:       uint16(snap.maxLevel),
		liveCount:      uint64(len(snap.ids) - len(snap.tombSorted)),
		tombstoneCount: uint64(len(snap.tombSorted)),
		checkpointLSN:  v.checkpointLSN,
	}
	payloads := [][]byte{nodeMeta, append([]byte(nil), snap.vectors...), graph, tomb}
	kinds := []uint16{regionKindNodeMeta, regionKindVectors, regionKindGraph, regionKindTombstones}
	dirOff := uint64(vectorHeaderSize)
	total := dirOff + uint64(4*regionEntrySize)
	offsets := make([]uint64, 4)
	for i, p := range payloads {
		total = storage.Align(total, 8)
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
	if err := storage.WriteFileAtomic(dir, base+".tmp", base, file, 0o644); err != nil {
		return err
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
