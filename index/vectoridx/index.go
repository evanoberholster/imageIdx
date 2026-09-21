package vectoridx

import (
	"errors"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"

	"github.com/evanoberholster/imageIdx/internal/indexutil"
)

// VectorIdx is a persistent HNSW approximate-nearest-neighbor index over one
// fixed vector space (TypeID, Dimensions, ElementType, L2 metric).
//
// Concurrency: one writer serializes Add/Delete/Checkpoint/Compact through
// mu. Readers hold mu read-locked plus a ReaderTracker token for the whole
// search, so published state is never mutated under them. V1 readers take a
// shared lock rather than fully lock-free mmap epoch reads; the file
// generations they read remain immutable. See doc.go.
type VectorIdx struct {
	cfg  Config
	path string // empty for in-memory indexes (New)

	mu      sync.RWMutex
	readers *indexutil.ReaderTracker
	closed  atomic.Bool
	gen     atomic.Uint64 // published generation, bumped per mutation

	lsn           uint64 // last applied WAL sequence
	checkpointLSN uint64 // LSN covered by the published file generation

	// Dense physical storage in insertion-ordinal order.
	ids     []NodeID
	vectors []byte // len == len(ids)*vecBytes, contiguous
	levels  []int
	links   [][][]NodeID // links[ord][level] = neighbor NodeIDs

	// Sorted storage locator: sortedIDs/sortedOrd resolve NodeID -> ordinal.
	sortedIDs []NodeID
	sortedOrd []int

	tombstones map[NodeID]struct{}

	entry    NodeID
	hasEntry bool
	maxLevel int

	rng *rand.Rand
	mL  float64

	dist     distRaw
	vecBytes int
	elemSize int
	dims     int

	walPath string
	wal     *os.File
}

// New returns an in-memory index with no persistence. It is the fastest path
// for tests and for callers that manage durability themselves. Checkpoint,
// Compact (rebuild), search, and WAL-free mutation all work; file recovery
// does not apply.
func New(cfg Config) (*VectorIdx, error) {
	validated, err := cfg.withDefaults()
	if err != nil {
		return nil, err
	}
	if !indexutil.HostIsLittleEndian() {
		return nil, ErrUnsupportedHost
	}
	v := &VectorIdx{
		cfg:        validated,
		readers:    &indexutil.ReaderTracker{},
		tombstones: make(map[NodeID]struct{}),
		rng:        rand.New(rand.NewSource(1)),
		mL:         levelMultiplier(validated.M),
		vecBytes:   validated.VectorBytes(),
		elemSize:   validated.ElementSize(),
		dims:       int(validated.Dimensions),
	}
	v.dist = resolveDistance(validated)
	v.gen.Store(1)
	return v, nil
}

// Create creates (truncating) a persistent index at path and checkpoints an
// empty generation so Open can recover it later.
func Create(path string, cfg Config) (*VectorIdx, error) {
	validated, err := cfg.withDefaults()
	if err != nil {
		return nil, err
	}
	if path == "" {
		return nil, ErrInvalidConfig
	}
	if !indexutil.HostIsLittleEndian() {
		return nil, ErrUnsupportedHost
	}
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
	}
	v := &VectorIdx{
		cfg:        validated,
		path:       path,
		readers:    &indexutil.ReaderTracker{},
		tombstones: make(map[NodeID]struct{}),
		rng:        rand.New(rand.NewSource(1)),
		mL:         levelMultiplier(validated.M),
		vecBytes:   validated.VectorBytes(),
		elemSize:   validated.ElementSize(),
		dims:       int(validated.Dimensions),
		walPath:    path + ".wal",
	}
	v.dist = resolveDistance(validated)
	v.gen.Store(1)
	wal, err := os.OpenFile(v.walPath, os.O_CREATE|os.O_RDWR|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, err
	}
	v.wal = wal
	if err := v.checkpointLocked(); err != nil {
		_ = wal.Close()
		return nil, err
	}
	return v, nil
}

// Open opens an index created by Create, validates its superblock and region
// checksums, then replays WAL records after CheckpointLSN. Recovery stops at
// the first incomplete trailing record and truncates the torn tail so the
// next open is deterministic.
func Open(path string) (*VectorIdx, error) {
	if path == "" {
		return nil, ErrInvalidConfig
	}
	if !indexutil.HostIsLittleEndian() {
		return nil, ErrUnsupportedHost
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	header, regions, err := decodeFile(data)
	if err != nil {
		return nil, err
	}
	cfg := Config{
		TypeID:         header.typeID,
		Dimensions:     header.dims,
		ElementType:    header.elem,
		M:              header.m,
		M0:             header.m0,
		EfConstruction: header.efConstruction,
		EfSearch:       header.efSearch,
	}
	validated, err := cfg.withDefaults()
	if err != nil {
		return nil, ErrCorruptStorage
	}
	v := &VectorIdx{
		cfg:           validated,
		path:          path,
		readers:       &indexutil.ReaderTracker{},
		tombstones:    make(map[NodeID]struct{}),
		rng:           rand.New(rand.NewSource(1)),
		mL:            levelMultiplier(validated.M),
		vecBytes:      validated.VectorBytes(),
		elemSize:      validated.ElementSize(),
		dims:          int(validated.Dimensions),
		walPath:       path + ".wal",
		checkpointLSN: header.checkpointLSN,
		lsn:           header.checkpointLSN,
	}
	v.dist = resolveDistance(validated)
	if err := v.loadRegions(header, regions); err != nil {
		return nil, err
	}
	v.gen.Store(header.generation + 1)
	wal, err := os.OpenFile(v.walPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	v.wal = wal
	if err := v.replayWAL(); err != nil {
		_ = wal.Close()
		return nil, err
	}
	return v, nil
}

// Close marks the index closed, checkpoints no state, and waits for prior
// readers. Further mutations return ErrClosed; searches on a closed index
// return ErrClosed.
func (v *VectorIdx) Close() error {
	v.mu.Lock()
	if v.closed.Load() {
		v.mu.Unlock()
		return nil
	}
	v.closed.Store(true)
	wal := v.wal
	v.wal = nil
	v.mu.Unlock()
	if wal != nil {
		_ = wal.Sync()
		_ = wal.Close()
	}
	v.readers.WaitForPriorReaders()
	return nil
}

// Config returns the validated index configuration.
func (v *VectorIdx) Config() Config {
	return v.cfg
}

// Generation returns the current published generation number.
func (v *VectorIdx) Generation() uint64 { return v.gen.Load() }

// LiveCount returns the number of live (present, non-tombstoned) nodes.
func (v *VectorIdx) LiveCount() int {
	token := v.readers.Acquire()
	v.mu.RLock()
	n := len(v.ids) - len(v.tombstones)
	v.mu.RUnlock()
	v.readers.Release(token)
	return n
}

// TombstoneCount returns the number of tombstoned nodes awaiting Compact.
func (v *VectorIdx) TombstoneCount() int {
	token := v.readers.Acquire()
	v.mu.RLock()
	n := len(v.tombstones)
	v.mu.RUnlock()
	v.readers.Release(token)
	return n
}

// Metadata is a copied summary of the current generation.
type Metadata struct {
	TypeID         TypeID
	Dimensions     uint16
	ElementType    ElementType
	Generation     uint64
	LiveCount      uint64
	TombstoneCount uint64
	Entry          NodeID
	HasEntry       bool
	MaxLevel       int
}

// Metadata returns a copy of current index metadata.
func (v *VectorIdx) Metadata() Metadata {
	token := v.readers.Acquire()
	v.mu.RLock()
	m := Metadata{
		TypeID:         v.cfg.TypeID,
		Dimensions:     v.cfg.Dimensions,
		ElementType:    v.cfg.ElementType,
		Generation:     v.gen.Load(),
		LiveCount:      uint64(len(v.ids) - len(v.tombstones)),
		TombstoneCount: uint64(len(v.tombstones)),
		Entry:          v.entry,
		HasEntry:       v.hasEntry,
		MaxLevel:       v.maxLevel,
	}
	v.mu.RUnlock()
	v.readers.Release(token)
	return m
}

// ---- mutation ----

// sampleLevelLocked draws an HNSW level. Caller holds the writer lock; the
// RNG is writer-exclusive.
func (v *VectorIdx) sampleLevelLocked() int {
	uniform := v.rng.Float64()
	if uniform <= 0 {
		uniform = 0.5
	} else if uniform >= 1 {
		uniform = 1 - 1e-9
	}
	level := int(-log1pNeg(uniform) * v.mL)
	if level < 0 {
		return 0
	}
	return level
}

func log1pNeg(u float64) float64 {
	// -ln(1-u) without importing extra precision paths; u in (0,1).
	return -logFloat(1 - u)
}

func logFloat(x float64) float64 {
	return math.Log(x)
}

// appendNodeLocked stores the raw vector and empty graph slots at a new
// physical ordinal, returning it. The caller wires graph links afterwards.
func (v *VectorIdx) appendNodeLocked(id NodeID, raw []byte, level int) int {
	ord := len(v.ids)
	v.ids = append(v.ids, id)
	v.vectors = append(v.vectors, raw...)
	v.levels = append(v.levels, level)
	layerLinks := make([][]NodeID, level+1)
	v.links = append(v.links, layerLinks)
	// Insert into the sorted locator.
	pos := sort.Search(len(v.sortedIDs), func(i int) bool { return v.sortedIDs[i] >= id })
	v.sortedIDs = append(v.sortedIDs, 0)
	v.sortedOrd = append(v.sortedOrd, 0)
	copy(v.sortedIDs[pos+1:], v.sortedIDs[pos:])
	copy(v.sortedOrd[pos+1:], v.sortedOrd[pos:])
	v.sortedIDs[pos] = id
	v.sortedOrd[pos] = ord
	return ord
}

func (v *VectorIdx) addRawLocked(id NodeID, raw []byte) error {
	if _, exists := v.locateLocked(id); exists {
		return ErrDuplicateNodeID
	}
	if len(raw) != v.vecBytes {
		return ErrInvalidVector
	}
	if v.wal != nil {
		if err := v.appendWALRecord(walTypeAdd, id, raw); err != nil {
			return err
		}
		v.lsn++
	}
	level := v.sampleLevelLocked()
	// Neighbor search runs before the node becomes visible: search the graph
	// without the new node, then append and wire.
	var selected [][]NodeID
	var entryPath []int
	if v.hasEntry {
		ctx := acquireSearchContext()
		ctx.resetForNodes(len(v.ids), v.vecBytes)
		entryOrd, ok := v.locateLocked(v.entry)
		cur := -1
		if ok {
			cur = entryOrd
			for l := v.maxLevel; l > level; l-- {
				cur = v.greedyStepLocked(cur, l, raw)
			}
			selected = make([][]NodeID, 0)
			entryPath = make([]int, 0)
			for l := min(v.maxLevel, level); l >= 0; l-- {
				ef := int(v.cfg.EfConstruction)
				cands := v.searchLayerLocked(ctx, raw, cur, ef, l)
				m := int(v.cfg.M)
				if l == 0 {
					m = int(v.cfg.M0)
				}
				picked := selectClosest(cands, m)
				fwd := make([]NodeID, len(picked))
				for i, p := range picked {
					fwd[i] = v.ids[p.ord]
				}
				selected = append(selected, fwd)
				if len(picked) != 0 {
					cur = picked[0].ord
				}
				entryPath = append(entryPath, cur)
			}
		}
		releaseSearchContext(ctx)
	}
	ord := v.appendNodeLocked(id, raw, level)
	// Wire forward links captured above (ordinals pre-append are still valid
	// because append never shifts existing ordinals).
	li := 0
	for l := min(v.maxLevel, level); l >= 0; l-- {
		if li < len(selected) {
			v.links[ord][l] = append(v.links[ord][l][:0], selected[li]...)
			for _, nb := range selected[li] {
				nOrd, ok := v.locateLocked(nb)
				if !ok || nOrd == ord {
					continue
				}
				if l > v.levels[nOrd] {
					continue
				}
				m := int(v.cfg.M)
				if l == 0 {
					m = int(v.cfg.M0)
				}
				v.links[nOrd][l] = append(v.links[nOrd][l], id)
				if len(v.links[nOrd][l]) > m {
					v.pruneNeighborsLocked(nOrd, l, m)
				}
			}
		}
		li++
	}
	if !v.hasEntry || level > v.maxLevel {
		v.entry = id
		v.hasEntry = true
		v.maxLevel = level
	}
	v.gen.Add(1)
	return nil
}

// Delete tombstones id. Tombstoned nodes stay traversable but are never
// returned; Compact purges them. Delete is idempotent for tombstoned nodes.
func (v *VectorIdx) Delete(id NodeID) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.closed.Load() {
		return ErrClosed
	}
	ord, ok := v.locateLocked(id)
	_ = ord
	if !ok {
		return ErrNodeNotFound
	}
	if _, tombstoned := v.tombstones[id]; tombstoned {
		return nil
	}
	if v.wal != nil {
		if err := v.appendWALRecord(walTypeDelete, id, nil); err != nil {
			return err
		}
		v.lsn++
	}
	v.tombstones[id] = struct{}{}
	v.gen.Add(1)
	return nil
}

// ---- typed Add/Search wrappers ----

// AddInt8 inserts one int8 vector. It rejects duplicate NodeIDs (live or
// tombstoned); re-adding after Delete requires Compact first.
func (v *VectorIdx) AddInt8(id NodeID, vec []int8) error {
	if v.cfg.ElementType != ElementInt8 {
		return ErrInvalidVector
	}
	if len(vec) != v.dims {
		return ErrInvalidVector
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.closed.Load() {
		return ErrClosed
	}
	raw := make([]byte, v.vecBytes)
	copy(bytesAsInt8(raw, len(vec)), vec)
	return v.addRawLocked(id, raw)
}

// AddUint8 inserts one uint8 vector.
func (v *VectorIdx) AddUint8(id NodeID, vec []uint8) error {
	if v.cfg.ElementType != ElementUint8 {
		return ErrInvalidVector
	}
	if len(vec) != v.dims {
		return ErrInvalidVector
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.closed.Load() {
		return ErrClosed
	}
	raw := make([]byte, v.vecBytes)
	copy(raw, vec)
	return v.addRawLocked(id, raw)
}

// AddInt16 inserts one int16 vector.
func (v *VectorIdx) AddInt16(id NodeID, vec []int16) error {
	if v.cfg.ElementType != ElementInt16 {
		return ErrInvalidVector
	}
	if len(vec) != v.dims {
		return ErrInvalidVector
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.closed.Load() {
		return ErrClosed
	}
	raw := encodeRawInt16(nil, vec)
	return v.addRawLocked(id, raw)
}

// AddUint16 inserts one uint16 vector.
func (v *VectorIdx) AddUint16(id NodeID, vec []uint16) error {
	if v.cfg.ElementType != ElementUint16 {
		return ErrInvalidVector
	}
	if len(vec) != v.dims {
		return ErrInvalidVector
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.closed.Load() {
		return ErrClosed
	}
	raw := encodeRawUint16(nil, vec)
	return v.addRawLocked(id, raw)
}

// AddFloat32 inserts one float32 vector.
func (v *VectorIdx) AddFloat32(id NodeID, vec []float32) error {
	if v.cfg.ElementType != ElementFloat32 {
		return ErrInvalidVector
	}
	if len(vec) != v.dims {
		return ErrInvalidVector
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.closed.Load() {
		return ErrClosed
	}
	raw := encodeRawFloat32(nil, vec)
	return v.addRawLocked(id, raw)
}

// searchRawLocked runs the full ANN path for an encoded query. Caller holds
// at least the read lock and owns ctx (reset by this function).
func (v *VectorIdx) searchRawLocked(ctx *SearchContext, query []byte, filter CandidateFilter, k int) []Result {
	if !v.hasEntry || len(v.ids) == 0 {
		return nil
	}
	if k <= 0 {
		return nil
	}
	if k > len(v.ids) {
		k = len(v.ids)
	}
	ef := int(v.cfg.EfSearch)
	if ef < k {
		ef = k
	}
	if filter != nil {
		// Oversample so post-filtering still fills k where reachable.
		if want := k * 4; want > ef {
			ef = want
		}
	}
	if ef > len(v.ids) {
		ef = len(v.ids)
	}
	if ef < 1 {
		ef = 1
	}
	ctx.resetForNodes(len(v.ids), v.vecBytes)
	entries := v.layer0SearchLocked(ctx, query, ef)
	results := v.assembleLocked(entries, filter, k, ctx.output[:0])
	ctx.output = results
	if filter != nil && len(results) < k {
		// ANN coverage fell short under filtering; the exact scan is the
		// ground-truth fallback and guarantees k-or-available semantics.
		available := 0
		for _, id := range v.sortedIDs {
			if _, tombstoned := v.tombstones[id]; tombstoned {
				continue
			}
			if !filter(id) {
				continue
			}
			available++
		}
		if available <= k {
			return v.exactSearchLocked(query, filter, k)
		}
		if len(results) < k {
			exact := v.exactSearchLocked(query, filter, k)
			if len(exact) > len(results) {
				return exact
			}
		}
	}
	return results
}

// queryEncoder writes the typed query into dst (reusing pooled capacity)
// and returns the raw encoding. It runs under the read lock with ctx alive.
type queryEncoder func(dst []byte) []byte

func (v *VectorIdx) searchEncoded(filter CandidateFilter, k int, encode queryEncoder) ([]Result, error) {
	if k <= 0 {
		return nil, ErrInvalidQuery
	}
	token := v.readers.Acquire()
	v.mu.RLock()
	if v.closed.Load() {
		v.mu.RUnlock()
		v.readers.Release(token)
		return nil, ErrClosed
	}
	ctx := acquireSearchContext()
	if cap(ctx.queryBuf) < v.vecBytes {
		ctx.queryBuf = make([]byte, v.vecBytes)
	}
	query := encode(ctx.queryBuf[:0])
	ctx.queryBuf = query
	results := v.searchRawLocked(ctx, query, filter, k)
	out := make([]Result, len(results))
	copy(out, results)
	releaseSearchContext(ctx)
	v.mu.RUnlock()
	v.readers.Release(token)
	return out, nil
}

func (v *VectorIdx) searchIntoEncoded(dst []Result, filter CandidateFilter, k int, encode queryEncoder) ([]Result, error) {
	if k <= 0 {
		return nil, ErrInvalidQuery
	}
	token := v.readers.Acquire()
	v.mu.RLock()
	if v.closed.Load() {
		v.mu.RUnlock()
		v.readers.Release(token)
		return nil, ErrClosed
	}
	ctx := acquireSearchContext()
	if cap(ctx.queryBuf) < v.vecBytes {
		ctx.queryBuf = make([]byte, v.vecBytes)
	}
	query := encode(ctx.queryBuf[:0])
	ctx.queryBuf = query
	results := v.searchRawLocked(ctx, query, filter, k)
	dst = append(dst, results...)
	releaseSearchContext(ctx)
	v.mu.RUnlock()
	v.readers.Release(token)
	return dst, nil
}

// SearchInt8 returns up to k nearest live neighbors of an int8 query.
func (v *VectorIdx) SearchInt8(query []int8, filter CandidateFilter, k int) ([]Result, error) {
	if v.cfg.ElementType != ElementInt8 || len(query) != v.dims {
		return nil, ErrInvalidVector
	}
	return v.searchEncoded(filter, k, func(dst []byte) []byte {
		if cap(dst) < len(query) {
			dst = make([]byte, len(query))
		} else {
			dst = dst[:len(query)]
		}
		copy(bytesAsInt8(dst, len(query)), query)
		return dst
	})
}

// SearchUint8 returns up to k nearest live neighbors of a uint8 query.
func (v *VectorIdx) SearchUint8(query []uint8, filter CandidateFilter, k int) ([]Result, error) {
	if v.cfg.ElementType != ElementUint8 || len(query) != v.dims {
		return nil, ErrInvalidVector
	}
	return v.searchEncoded(filter, k, func(dst []byte) []byte {
		return append(dst, query...)
	})
}

// SearchInt16 returns up to k nearest live neighbors of an int16 query.
func (v *VectorIdx) SearchInt16(query []int16, filter CandidateFilter, k int) ([]Result, error) {
	if v.cfg.ElementType != ElementInt16 || len(query) != v.dims {
		return nil, ErrInvalidVector
	}
	return v.searchEncoded(filter, k, func(dst []byte) []byte {
		return encodeRawInt16(dst, query)
	})
}

// SearchUint16 returns up to k nearest live neighbors of a uint16 query.
func (v *VectorIdx) SearchUint16(query []uint16, filter CandidateFilter, k int) ([]Result, error) {
	if v.cfg.ElementType != ElementUint16 || len(query) != v.dims {
		return nil, ErrInvalidVector
	}
	return v.searchEncoded(filter, k, func(dst []byte) []byte {
		return encodeRawUint16(dst, query)
	})
}

// SearchFloat32 returns up to k nearest live neighbors of a float32 query.
func (v *VectorIdx) SearchFloat32(query []float32, filter CandidateFilter, k int) ([]Result, error) {
	if v.cfg.ElementType != ElementFloat32 || len(query) != v.dims {
		return nil, ErrInvalidVector
	}
	return v.searchEncoded(filter, k, func(dst []byte) []byte {
		return encodeRawFloat32(dst, query)
	})
}

// SearchIntoInt8 appends up to k matches to dst, reusing caller storage.
func (v *VectorIdx) SearchIntoInt8(dst []Result, query []int8, filter CandidateFilter, k int) ([]Result, error) {
	if v.cfg.ElementType != ElementInt8 || len(query) != v.dims {
		return nil, ErrInvalidVector
	}
	return v.searchIntoEncoded(dst, filter, k, func(scratch []byte) []byte {
		if cap(scratch) < len(query) {
			scratch = make([]byte, len(query))
		} else {
			scratch = scratch[:len(query)]
		}
		copy(bytesAsInt8(scratch, len(query)), query)
		return scratch
	})
}

// SearchIntoUint8 appends up to k matches to dst, reusing caller storage.
func (v *VectorIdx) SearchIntoUint8(dst []Result, query []uint8, filter CandidateFilter, k int) ([]Result, error) {
	if v.cfg.ElementType != ElementUint8 || len(query) != v.dims {
		return nil, ErrInvalidVector
	}
	return v.searchIntoEncoded(dst, filter, k, func(scratch []byte) []byte {
		return append(scratch, query...)
	})
}

// SearchIntoInt16 appends up to k matches to dst, reusing caller storage.
func (v *VectorIdx) SearchIntoInt16(dst []Result, query []int16, filter CandidateFilter, k int) ([]Result, error) {
	if v.cfg.ElementType != ElementInt16 || len(query) != v.dims {
		return nil, ErrInvalidVector
	}
	return v.searchIntoEncoded(dst, filter, k, func(scratch []byte) []byte {
		return encodeRawInt16(scratch, query)
	})
}

// SearchIntoUint16 appends up to k matches to dst, reusing caller storage.
func (v *VectorIdx) SearchIntoUint16(dst []Result, query []uint16, filter CandidateFilter, k int) ([]Result, error) {
	if v.cfg.ElementType != ElementUint16 || len(query) != v.dims {
		return nil, ErrInvalidVector
	}
	return v.searchIntoEncoded(dst, filter, k, func(scratch []byte) []byte {
		return encodeRawUint16(scratch, query)
	})
}

// SearchIntoFloat32 appends up to k matches to dst, reusing caller storage.
func (v *VectorIdx) SearchIntoFloat32(dst []Result, query []float32, filter CandidateFilter, k int) ([]Result, error) {
	if v.cfg.ElementType != ElementFloat32 || len(query) != v.dims {
		return nil, ErrInvalidVector
	}
	return v.searchIntoEncoded(dst, filter, k, func(scratch []byte) []byte {
		return encodeRawFloat32(scratch, query)
	})
}

// ExactSearchFloat32 runs the scalar exact-L2 reference scan. It is the
// recall oracle for tests and benchmarks, not the production path.
func (v *VectorIdx) ExactSearchFloat32(query []float32, filter CandidateFilter, k int) ([]Result, error) {
	if v.cfg.ElementType != ElementFloat32 || len(query) != v.dims {
		return nil, ErrInvalidVector
	}
	if k <= 0 {
		return nil, ErrInvalidQuery
	}
	raw := encodeRawFloat32(nil, query)
	token := v.readers.Acquire()
	v.mu.RLock()
	if v.closed.Load() {
		v.mu.RUnlock()
		v.readers.Release(token)
		return nil, ErrClosed
	}
	out := v.exactSearchLocked(raw, filter, k)
	v.mu.RUnlock()
	v.readers.Release(token)
	return out, nil
}

// Checkpoint persists a self-consistent generation and truncates the WAL
// tail it covers. It is explicit in V1; no background compaction exists.
func (v *VectorIdx) Checkpoint() error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.closed.Load() {
		return ErrClosed
	}
	if v.path == "" {
		return ErrNotPersistent
	}
	return v.checkpointLocked()
}

// Compact rebuilds the graph from live vectors only, preserving NodeIDs,
// then checkpoints. Tombstones are purged and the WAL restarts after the new
// checkpoint LSN. Reinsertion uses NodeID order with a fixed seed so a
// compacted index is deterministic given its live set.
func (v *VectorIdx) Compact() error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.closed.Load() {
		return ErrClosed
	}
	type live struct {
		id  NodeID
		raw []byte
	}
	liveIDs := make([]NodeID, 0, len(v.ids)-len(v.tombstones))
	for _, id := range v.sortedIDs {
		if _, tombstoned := v.tombstones[id]; tombstoned {
			continue
		}
		liveIDs = append(liveIDs, id)
	}
	lives := make([]live, 0, len(liveIDs))
	for _, id := range liveIDs {
		ord, ok := v.locateLocked(id)
		if !ok {
			return ErrCorruptStorage
		}
		raw := make([]byte, v.vecBytes)
		copy(raw, v.vectorAtLocked(ord))
		lives = append(lives, live{id: id, raw: raw})
	}
	v.ids = v.ids[:0]
	v.vectors = v.vectors[:0]
	v.levels = v.levels[:0]
	v.links = v.links[:0]
	v.sortedIDs = v.sortedIDs[:0]
	v.sortedOrd = v.sortedOrd[:0]
	v.tombstones = make(map[NodeID]struct{})
	v.hasEntry = false
	v.maxLevel = 0
	v.rng = rand.New(rand.NewSource(0xC0FAC7))
	ctx := acquireSearchContext()
	defer releaseSearchContext(ctx)
	for _, lv := range lives {
		level := v.sampleLevelLocked()
		ord := len(v.ids)
		v.ids = append(v.ids, lv.id)
		v.vectors = append(v.vectors, lv.raw...)
		v.levels = append(v.levels, level)
		v.links = append(v.links, make([][]NodeID, level+1))
		pos := sort.Search(len(v.sortedIDs), func(i int) bool { return v.sortedIDs[i] >= lv.id })
		v.sortedIDs = append(v.sortedIDs, 0)
		v.sortedOrd = append(v.sortedOrd, 0)
		copy(v.sortedIDs[pos+1:], v.sortedIDs[pos:])
		copy(v.sortedOrd[pos+1:], v.sortedOrd[pos:])
		v.sortedIDs[pos] = lv.id
		v.sortedOrd[pos] = ord
		if len(v.ids) > 1 {
			// Reuse the standard insertion wiring against the rebuilt prefix.
			// Reset scratch per node: the prefix grows each iteration, so the
			// visited tags must be resized and the generation bumped, exactly
			// like the write path in addRawLocked.
			ctx.resetForNodes(len(v.ids), v.vecBytes)
			v.wireRebuiltLocked(ctx, lv.raw, ord, level, lv.id)
		} else {
			v.entry = lv.id
			v.hasEntry = true
			v.maxLevel = level
		}
		if level > v.maxLevel {
			v.entry = lv.id
			v.maxLevel = level
		}
	}
	v.gen.Add(1)
	if v.path == "" {
		return nil
	}
	return v.checkpointLocked()
}

// wireRebuiltLocked connects rebuilt ordinal ord (already appended) into the
// prefix graph. It mirrors addRawLocked without WAL/LSN/locator-append.
func (v *VectorIdx) wireRebuiltLocked(ctx *SearchContext, raw []byte, ord, level int, id NodeID) {
	entryOrd, ok := v.locateLocked(v.entry)
	if !ok {
		v.entry = id
		v.hasEntry = true
		v.maxLevel = level
		return
	}
	// The new node is at the tail; searches may visit it via reciprocal
	// links added below, but forward search from entry cannot reach it yet
	// because no edges point to it. Temporarily hide it from distance
	// contention by excluding self matches in neighbor loops.
	cur := entryOrd
	if cur == ord {
		cur = 0
	}
	for l := v.maxLevel; l > level; l-- {
		cur = v.greedyStepLocked(cur, l, raw)
	}
	for l := min(v.maxLevel, level); l >= 0; l-- {
		ef := int(v.cfg.EfConstruction)
		cands := v.searchLayerLocked(ctx, raw, cur, ef, l)
		// Drop self if the layer search reached the new node through a
		// just-added upper-layer reciprocal edge.
		filtered := cands[:0]
		for _, c := range cands {
			if c.ord != ord {
				filtered = append(filtered, c)
			}
		}
		cands = filtered
		m := int(v.cfg.M)
		if l == 0 {
			m = int(v.cfg.M0)
		}
		picked := selectClosest(cands, m)
		for _, p := range picked {
			v.links[ord][l] = append(v.links[ord][l], v.ids[p.ord])
		}
		for _, p := range picked {
			nb := p.ord
			if nb == ord || l > v.levels[nb] {
				continue
			}
			v.links[nb][l] = append(v.links[nb][l], id)
			if len(v.links[nb][l]) > m {
				v.pruneNeighborsLocked(nb, l, m)
			}
		}
		if len(picked) != 0 {
			cur = picked[0].ord
		}
	}
}

// Verify performs a deep structural check: locator consistency, vector
// lengths, entry validity, edge targets, level bounds, and tombstone subset.
// It does not verify WAL contents.
func (v *VectorIdx) Verify() error {
	token := v.readers.Acquire()
	v.mu.RLock()
	defer func() {
		v.mu.RUnlock()
		v.readers.Release(token)
	}()
	if len(v.vectors) != len(v.ids)*v.vecBytes {
		return ErrCorruptStorage
	}
	if len(v.levels) != len(v.ids) || len(v.links) != len(v.ids) {
		return ErrCorruptStorage
	}
	if len(v.sortedIDs) != len(v.ids) || len(v.sortedOrd) != len(v.ids) {
		return ErrCorruptStorage
	}
	seen := make(map[NodeID]int, len(v.ids))
	for ord, id := range v.ids {
		if _, dup := seen[id]; dup {
			return ErrCorruptStorage
		}
		seen[id] = ord
		if v.levels[ord] < 0 || len(v.links[ord]) != v.levels[ord]+1 {
			return ErrCorruptStorage
		}
	}
	for i := 1; i < len(v.sortedIDs); i++ {
		if v.sortedIDs[i-1] >= v.sortedIDs[i] {
			return ErrCorruptStorage
		}
	}
	for i, id := range v.sortedIDs {
		ord := v.sortedOrd[i]
		if ord < 0 || ord >= len(v.ids) || v.ids[ord] != id {
			return ErrCorruptStorage
		}
	}
	for id := range v.tombstones {
		if _, ok := seen[id]; !ok {
			return ErrCorruptStorage
		}
	}
	if v.hasEntry {
		entryOrd, ok := v.locateLocked(v.entry)
		if !ok {
			return ErrCorruptStorage
		}
		if v.levels[entryOrd] != v.maxLevel {
			return ErrCorruptStorage
		}
	} else if len(v.ids) != 0 {
		return ErrCorruptStorage
	}
	for ord, layerLinks := range v.links {
		for level, nbs := range layerLinks {
			if level > v.levels[ord] {
				return ErrCorruptStorage
			}
			for _, nb := range nbs {
				nOrd, ok := v.locateLocked(nb)
				if !ok {
					return ErrCorruptStorage
				}
				if v.levels[nOrd] < level {
					return ErrCorruptStorage
				}
			}
		}
	}
	return nil
}

var errClosedSentinel = errors.New("vectoridx: closed")
var _ = errClosedSentinel
