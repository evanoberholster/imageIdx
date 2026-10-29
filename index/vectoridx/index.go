package vectoridx

import (
	"context"
	"errors"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/evanoberholster/imageIdx/internal/storage"
	"github.com/evanoberholster/imageIdx/roar"
	schema "github.com/evanoberholster/imageIdx/schema"
)

// VectorIdx is a persistent HNSW approximate-nearest-neighbor index over one
// fixed vector space (Namespace, Dimensions, ElementType, metric).
//
// Concurrency: reads are lock-free — they acquire a ReaderTracker token and
// load one atomically published snapshot, taking no mutex. One writer
// serializes Add/Delete/Checkpoint/Compact through writeMu (plus an async
// queue for background work). Under DurabilitySync the per-mutation fsync
// runs after the writer releases writeMu, so it overlaps the next writer's
// graph insertion instead of serializing every insert behind it. Published
// snapshots share backing arrays with copy-on-write rules: appends beyond
// the published length are safe in place, middle inserts copy, dense fills
// filter via bounds checks, link rewrites clone short inner slices, and
// tombstones swap an immutable roar bitmap. See doc.go.
type vectorSnapshot struct {
	cfg      Config
	dist     distRaw
	dot      distRaw   // raw dot product; set for MetricCosine
	norms    []float64 // per-ordinal vector norms; set for MetricCosine
	vecBytes int
	elemSize int
	dims     int

	// Dense physical storage in insertion-ordinal order.
	ids     []NodeID
	vectors []byte // len == len(ids)*vecBytes, contiguous
	levels  []int
	links   []*atomic.Pointer[nodeLayers] // links[ord] -> per-node layers

	// Storage locators. sortedIDs/sortedOrd resolve NodeID -> ordinal via
	// binary search; dense holds an O(1) table when useDense.
	sortedIDs []NodeID
	sortedOrd []int
	dense     *denseTable
	useDense  bool

	tomb       roar.Bitmap
	tombSorted []NodeID

	entry    NodeID
	hasEntry bool
	maxLevel int
	gen      uint64
}

type asyncOp struct {
	fn   func()
	done chan struct{}
}

type VectorIdx struct {
	cfgBase    Config // persisted identity (Namespace/Dims/Elem/Metric/M/M0/Ef*), immutable
	durability Durability
	path       string // empty for in-memory indexes (NewMemory)

	view    atomic.Pointer[vectorSnapshot]
	readers *storage.ReaderTracker
	writeMu sync.Mutex
	closed  atomic.Bool
	gen     atomic.Uint64 // published generation, bumped per mutation

	lsn              uint64        // writer-only under writeMu
	checkpointLSN    uint64        // writer-only under writeMu
	checkpointLSNPub atomic.Uint64 // published for Stats without the writer lock

	rng *rand.Rand // writer-exclusive
	mL  float64    // immutable

	dist     distRaw // immutable (metric fixed for lifetime)
	dot      distRaw // immutable; raw dot product for MetricCosine
	vecBytes int
	elemSize int
	dims     int

	// Writer-only scratch buffers guarded by writeMu. They remove per-vector
	// allocations from the Add and WAL paths, and per-prune allocations from
	// graph maintenance.
	encodeBuf   []byte
	walBuf      []byte
	pruneScored []pruneScore
	pruneCands  []searchEntry

	walPath string
	wal     *os.File // writer-only under writeMu (active segment)
	// pendingSync records the segment handle of the last successful
	// DurabilitySync append so the writer can fsync it after releasing
	// writeMu. Guarded by writeMu; cleared by its own writer.
	pendingSync  *os.File
	walSeq       uint64   // active segment sequence
	walSize      int64    // active segment bytes on disk
	walSegBytes  int64    // rotation threshold
	lock         *os.File // exclusive store lock held until Close
	syncInterval time.Duration
	syncMu       sync.Mutex // serializes WAL fsync calls
	walDirty     atomic.Bool
	walRecords   atomic.Uint64
	walBytes     atomic.Uint64
	walSyncs     atomic.Uint64

	compactMu   sync.Mutex
	lastCompact atomic.Pointer[CompactionResult]

	manifest         atomic.Pointer[manifestData]
	nextDeltaGen     uint64              // writer-only
	pending          map[NodeID]struct{} // writer-only, changes since last checkpoint
	pendingMutations int                 // writer-only, raw mutation count
	coalescedRecords atomic.Uint64
	loadingDelta     bool // writer-only during OpenStore

	asyncCh      chan asyncOp
	asyncWg      sync.WaitGroup
	asyncPending atomic.Int64
}

// NewMemory returns an in-memory index with no persistence. It is the fastest
// path for tests and for callers that manage durability themselves. Search,
// Compact (rebuild), and WAL-free mutation work; Checkpoint, Sync, and Vacuum
// return ErrNotPersistent.
func NewMemory(cfg Config) (*VectorIdx, error) {
	validated, err := cfg.withDefaults()
	if err != nil {
		return nil, err
	}
	if !storage.HostIsLittleEndian() {
		return nil, ErrUnsupportedHost
	}
	dist := resolveDistance(validated)
	if dist == nil {
		return nil, ErrInvalidConfig
	}
	var dot distRaw
	if validated.Metric == MetricCosine {
		if dot = resolveDot(validated); dot == nil {
			return nil, ErrInvalidConfig
		}
	}
	v := &VectorIdx{
		cfgBase:  validated,
		readers:  &storage.ReaderTracker{},
		rng:      rand.New(rand.NewSource(1)),
		mL:       levelMultiplier(validated.M),
		dist:     dist,
		dot:      dot,
		vecBytes: validated.VectorBytes(),
		elemSize: validated.ElementSize(),
		dims:     int(validated.Dimensions),
	}
	v.view.Store(&vectorSnapshot{
		cfg:      validated,
		dist:     dist,
		dot:      dot,
		vecBytes: v.vecBytes,
		elemSize: v.elemSize,
		dims:     v.dims,
		gen:      1,
	})
	v.gen.Store(1)
	v.startAsyncWorker()
	return v, nil
}

// NewStore creates (truncating) a persistent index at path and checkpoints an
// empty generation so OpenStore can recover it later. Durability selects whether
// each acknowledged mutation is fsynced (DurabilitySync) or left to the OS
// page cache (DurabilityOS). WithSyncInterval adds periodic group commit for
// DurabilityOS. The path is exclusively locked until Close.
func NewStore(path string, cfg Config, durability Durability, opts ...Option) (*VectorIdx, error) {
	validated, err := cfg.withDefaults()
	if err != nil {
		return nil, err
	}
	if path == "" || !durability.Valid() {
		return nil, ErrInvalidConfig
	}
	if !storage.HostIsLittleEndian() {
		return nil, ErrUnsupportedHost
	}
	runtimeOpts, err := applyOptions(opts)
	if err != nil {
		return nil, err
	}
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
	}
	lock, err := storage.AcquireLock(filepath.Dir(path), filepath.Base(path)+".lock", false)
	if err != nil {
		return nil, err
	}
	v := &VectorIdx{
		cfgBase:      validated,
		durability:   durability,
		path:         path,
		readers:      &storage.ReaderTracker{},
		rng:          rand.New(rand.NewSource(1)),
		mL:           levelMultiplier(validated.M),
		vecBytes:     validated.VectorBytes(),
		elemSize:     validated.ElementSize(),
		dims:         int(validated.Dimensions),
		lock:         lock,
		syncInterval: runtimeOpts.syncInterval,
		walSegBytes:  runtimeOpts.walSegmentBytes,
	}
	v.dist = resolveDistance(validated)
	if validated.Metric == MetricCosine {
		v.dot = resolveDot(validated)
		if v.dot == nil {
			_ = storage.ReleaseLock(lock)
			return nil, ErrInvalidConfig
		}
	}
	v.view.Store(&vectorSnapshot{
		cfg:      validated,
		dist:     v.dist,
		dot:      v.dot,
		vecBytes: v.vecBytes,
		elemSize: v.elemSize,
		dims:     v.dims,
		gen:      1,
	})
	v.gen.Store(1)
	if err := removeWALFiles(path); err != nil {
		_ = storage.ReleaseLock(lock)
		return nil, err
	}
	if err := v.openWALSegment(1); err != nil {
		_ = storage.ReleaseLock(lock)
		return nil, err
	}
	v.manifest.Store(&manifestData{baseGeneration: 1})
	v.nextDeltaGen = 2
	if err := v.writeBaseLocked(); err != nil {
		_ = v.wal.Close()
		_ = storage.ReleaseLock(lock)
		return nil, err
	}
	if err := v.writeManifestLocked(&manifestData{baseGeneration: v.gen.Load(), checkpointLSN: v.lsn}); err != nil {
		_ = v.wal.Close()
		_ = storage.ReleaseLock(lock)
		return nil, err
	}
	v.startAsyncWorker()
	return v, nil
}

// OpenStore opens an index created by NewStore, validates its superblock and region
// checksums, then replays WAL records after CheckpointLSN. Recovery stops at
// the first incomplete trailing record and truncates the torn tail so the
// next open is deterministic. Durability and options are runtime policy and
// need not match the values used at NewStore time. The path is exclusively
// locked until Close.
func OpenStore(path string, durability Durability, opts ...Option) (*VectorIdx, error) {
	if path == "" || !durability.Valid() {
		return nil, ErrInvalidConfig
	}
	if !storage.HostIsLittleEndian() {
		return nil, ErrUnsupportedHost
	}
	runtimeOpts, err := applyOptions(opts)
	if err != nil {
		return nil, err
	}
	lock, err := storage.AcquireLock(filepath.Dir(path), filepath.Base(path)+".lock", false)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		_ = storage.ReleaseLock(lock)
		return nil, err
	}
	header, regions, err := decodeFile(data)
	if err != nil {
		_ = storage.ReleaseLock(lock)
		return nil, err
	}
	manifest, err := readManifest(path)
	if err != nil {
		_ = storage.ReleaseLock(lock)
		return nil, err
	}
	checkpointLSN := header.checkpointLSN
	baseGeneration := header.generation
	if manifest != nil {
		checkpointLSN = manifest.checkpointLSN
		baseGeneration = manifest.baseGeneration
	} else {
		manifest = &manifestData{baseGeneration: header.generation, checkpointLSN: header.checkpointLSN}
	}
	cfg := Config{
		Namespace:      header.namespace,
		Dimensions:     Dimensions(header.dims),
		ElementType:    header.elem,
		Metric:         header.metric,
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
		cfgBase:       validated,
		durability:    durability,
		path:          path,
		readers:       &storage.ReaderTracker{},
		rng:           rand.New(rand.NewSource(1)),
		mL:            levelMultiplier(validated.M),
		vecBytes:      validated.VectorBytes(),
		elemSize:      validated.ElementSize(),
		dims:          int(validated.Dimensions),
		checkpointLSN: checkpointLSN,
		lsn:           checkpointLSN,
		lock:          lock,
		syncInterval:  runtimeOpts.syncInterval,
		walSegBytes:   runtimeOpts.walSegmentBytes,
	}
	v.checkpointLSNPub.Store(checkpointLSN)
	v.manifest.Store(manifest)
	v.nextDeltaGen = baseGeneration + 1
	for _, d := range manifest.deltas {
		if d.gen >= v.nextDeltaGen {
			v.nextDeltaGen = d.gen + 1
		}
	}
	v.dist = resolveDistance(validated)
	if validated.Metric == MetricCosine {
		v.dot = resolveDot(validated)
		if v.dot == nil {
			_ = storage.ReleaseLock(lock)
			return nil, ErrCorruptStorage
		}
	}
	v.view.Store(&vectorSnapshot{
		cfg:      validated,
		dist:     v.dist,
		dot:      v.dot,
		vecBytes: v.vecBytes,
		elemSize: v.elemSize,
		dims:     v.dims,
		gen:      header.generation + 1,
	})
	if err := v.loadRegions(header, regions); err != nil {
		_ = storage.ReleaseLock(lock)
		return nil, err
	}
	v.gen.Store(header.generation + 1)
	if err := v.applyDeltasLocked(manifest); err != nil {
		_ = storage.ReleaseLock(lock)
		return nil, err
	}
	if err := v.replayWAL(); err != nil {
		if v.wal != nil {
			_ = v.wal.Close()
		}
		_ = storage.ReleaseLock(lock)
		return nil, err
	}
	v.startAsyncWorker()
	return v, nil
}

// Close marks the index closed, stops the async worker, checkpoints no state,
// and waits for prior readers. Further mutations return ErrClosed; searches
// on a closed index return ErrClosed. It is idempotent.
func (v *VectorIdx) Close() error {
	v.writeMu.Lock()
	if v.closed.Load() {
		v.writeMu.Unlock()
		return nil
	}
	v.closed.Store(true)
	// Sync and close the WAL under the write lock: a writer that appended
	// before closed was set is covered by this sync, so its post-unlock
	// syncWALAfterWrite sees a closed handle it treats as durable.
	var syncErr error
	wal := v.wal
	if wal != nil {
		if err := wal.Sync(); err != nil {
			syncErr = err
		} else {
			v.walSyncs.Add(1)
		}
		_ = wal.Close()
	}
	v.wal = nil
	v.pendingSync = nil
	ch := v.asyncCh
	v.asyncCh = nil
	if ch != nil {
		close(ch)
	}
	v.writeMu.Unlock()
	v.asyncWg.Wait()
	v.readers.WaitForPriorReaders()
	lock := v.lock
	v.lock = nil
	return errors.Join(syncErr, storage.ReleaseLock(lock))
}

// Config returns the validated index configuration.
func (v *VectorIdx) Config() Config {
	return v.view.Load().cfg
}

// Generation returns the current published generation number.
func (v *VectorIdx) Generation() uint64 { return v.gen.Load() }

// LiveCount returns the number of live (present, non-tombstoned) nodes.
// It is lock-free: one snapshot load plus a tracker token.
func (v *VectorIdx) LiveCount() int {
	token := v.readers.Acquire()
	snap := v.view.Load()
	n := len(snap.ids) - len(snap.tombSorted)
	v.readers.Release(token)
	return n
}

// TombstoneCount returns the number of tombstoned nodes awaiting Compact.
func (v *VectorIdx) TombstoneCount() int {
	token := v.readers.Acquire()
	snap := v.view.Load()
	n := len(snap.tombSorted)
	v.readers.Release(token)
	return n
}

// Metadata is a copied summary of the current generation.
type Metadata struct {
	Namespace      schema.Namespace
	Dimensions     Dimensions
	ElementType    ElementType
	Generation     uint64
	LiveCount      uint64
	TombstoneCount uint64
	Entry          NodeID
	HasEntry       bool
	MaxLevel       int
}

// Metadata returns a copy of current index metadata. Lock-free.
func (v *VectorIdx) Metadata() Metadata {
	token := v.readers.Acquire()
	snap := v.view.Load()
	m := Metadata{
		Namespace:      snap.cfg.Namespace,
		Dimensions:     snap.cfg.Dimensions,
		ElementType:    snap.cfg.ElementType,
		Generation:     v.gen.Load(),
		LiveCount:      uint64(len(snap.ids) - len(snap.tombSorted)),
		TombstoneCount: uint64(len(snap.tombSorted)),
		Entry:          snap.entry,
		HasEntry:       snap.hasEntry,
		MaxLevel:       snap.maxLevel,
	}
	v.readers.Release(token)
	return m
}

// ---- writer helpers (writeMu exclusive) ----

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

// cloneSnapshot shallow-copies slice headers sharing backing arrays.
// Appends beyond the published length are safe in place; middle inserts copy.
func cloneSnapshot(s *vectorSnapshot) *vectorSnapshot {
	c := *s
	return &c
}

// pickNeighbors selects up to m candidates using the configured policy.
func pickNeighbors(s *vectorSnapshot, cands []searchEntry, m int) []searchEntry {
	if s.cfg.UseHeuristic {
		return selectHeuristic(cands, m, s)
	}
	return selectClosest(cands, m)
}

// sortedInsert inserts id->ord into the snapshot's sorted locator. Appends at
// the end reuse backing safely; middle inserts allocate new arrays so
// published readers never see a shifted range.
func sortedInsert(s *vectorSnapshot, id NodeID, ord int) {
	if s.useDense {
		return
	}
	pos := sort.Search(len(s.sortedIDs), func(i int) bool { return s.sortedIDs[i] >= id })
	if pos == len(s.sortedIDs) {
		s.sortedIDs = append(s.sortedIDs, id)
		s.sortedOrd = append(s.sortedOrd, ord)
		return
	}
	newIDs := make([]NodeID, len(s.sortedIDs)+1)
	newOrd := make([]int, len(s.sortedOrd)+1)
	copy(newIDs, s.sortedIDs[:pos])
	copy(newOrd, s.sortedOrd[:pos])
	newIDs[pos] = id
	newOrd[pos] = ord
	copy(newIDs[pos+1:], s.sortedIDs[pos:])
	copy(newOrd[pos+1:], s.sortedOrd[pos:])
	s.sortedIDs = newIDs
	s.sortedOrd = newOrd
}

const maxDenseEntries = 1 << 26

// denseFeasible reports whether [min,max] fits a dense table at count nodes.
func denseFeasible(min, max NodeID, count int) bool {
	if max < min {
		return true
	}
	rng := uint64(max-min) + 1
	if rng > maxDenseEntries {
		return false
	}
	if rng <= uint64(count)*4+1024 {
		return true
	}
	return false
}

// denseEnsure makes the dense table cover id when feasible, building it from
// the snapshot on first use. It returns false when the index stays sparse.
func denseEnsure(s *vectorSnapshot, id NodeID, count int) bool {
	if s.useDense && s.dense != nil {
		min, mx := s.dense.min, s.dense.min+NodeID(len(s.dense.vals))-1
		lo, hi := min, mx
		if id < lo {
			lo = id
		}
		if id > hi {
			hi = id
		}
		if !denseFeasible(lo, hi, count) {
			s.dense = nil
			s.useDense = false
			s.rebuildSorted()
			// rebuildSorted already included id: report the locator as
			// updated so the caller does not insert it a second time.
			return true
		}
		if id < s.dense.min {
			gap := uint64(s.dense.min-id) + uint64(len(s.dense.vals))
			_ = gap
			// Prepend requires rebase: rebuild (rare).
			s.rebuildDenseWith(lo, hi)
			return true
		}
		if want := uint64(id-s.dense.min) + 1; want > uint64(len(s.dense.vals)) {
			need := int(want) - len(s.dense.vals)
			s.dense.vals = append(s.dense.vals, make([]int32, need)...)
			for i := len(s.dense.vals) - need; i < len(s.dense.vals); i++ {
				s.dense.vals[i] = -1
			}
		}
		return true
	}
	// Sparse: check feasibility including id.
	var lo, hi NodeID
	if len(s.ids) == 0 {
		lo, hi = id, id
	} else {
		lo, hi = s.ids[0], s.ids[0]
		for _, x := range s.ids {
			if x < lo {
				lo = x
			}
			if x > hi {
				hi = x
			}
		}
		if id < lo {
			lo = id
		}
		if id > hi {
			hi = id
		}
	}
	_ = hi
	if !denseFeasible(lo, hi, count) {
		return false
	}
	s.rebuildDenseWith(lo, hi)
	return true
}

// rebuildDenseWith builds a fresh dense table over [lo,hi] from current ids.
func (s *vectorSnapshot) rebuildDenseWith(lo, hi NodeID) {
	n := int(uint64(hi-lo) + 1)
	vals := make([]int32, n)
	for i := range vals {
		vals[i] = -1
	}
	for ord, id := range s.ids {
		if id >= lo && id <= hi {
			vals[uint64(id-lo)] = int32(ord)
		}
	}
	s.dense = &denseTable{min: lo, vals: vals}
	s.useDense = true
	s.sortedIDs = nil
	s.sortedOrd = nil
}

// rebuildSorted rebuilds the sorted locator from ids (sparse fallback).
func (s *vectorSnapshot) rebuildSorted() {
	n := len(s.ids)
	s.sortedIDs = make([]NodeID, n)
	s.sortedOrd = make([]int, n)
	for ord, id := range s.ids {
		s.sortedIDs[ord] = id
		s.sortedOrd[ord] = ord
	}
	sort.Slice(s.sortedIDs, func(i, j int) bool { return s.sortedIDs[i] < s.sortedIDs[j] })
	pos := make(map[NodeID]int, n)
	for ord, id := range s.ids {
		pos[id] = ord
	}
	for i, id := range s.sortedIDs {
		s.sortedOrd[i] = pos[id]
	}
}

// denseSet records id->ord with atomic stores safe for lock-free readers.
func denseSet(s *vectorSnapshot, id NodeID, ord int) {
	dt := s.dense
	if dt == nil || id < dt.min {
		return
	}
	idx := uint64(id - dt.min)
	if idx >= uint64(len(dt.vals)) {
		return
	}
	atomic.StoreInt32(&dt.vals[idx], int32(ord))
}

// rebuildTombeset publishes a new immutable tomb bitmap from a sorted list.
func rebuildTombeset(sorted []NodeID) (roar.Bitmap, []NodeID) {
	cp := append([]NodeID(nil), sorted...)
	if len(cp) == 0 {
		return roar.Bitmap{}, cp
	}
	u64 := make([]uint64, len(cp))
	for i, id := range cp {
		u64[i] = uint64(id)
	}
	bm, err := roar.FromSorted(u64)
	if err != nil {
		return roar.Bitmap{}, cp
	}
	return bm, cp
}

// insertTombSorted returns sorted with id inserted (copy).
func insertTombSorted(sorted []NodeID, id NodeID) []NodeID {
	pos := sort.Search(len(sorted), func(i int) bool { return sorted[i] >= id })
	if pos < len(sorted) && sorted[pos] == id {
		return sorted
	}
	out := make([]NodeID, len(sorted)+1)
	copy(out, sorted[:pos])
	out[pos] = id
	copy(out[pos+1:], sorted[pos:])
	return out
}

// removeTombSorted returns sorted without id (copy).
func removeTombSorted(sorted []NodeID, id NodeID) []NodeID {
	pos := sort.Search(len(sorted), func(i int) bool { return sorted[i] >= id })
	if pos >= len(sorted) || sorted[pos] != id {
		return sorted
	}
	out := make([]NodeID, len(sorted)-1)
	copy(out, sorted[:pos])
	copy(out[pos:], sorted[pos+1:])
	return out
}

func (v *VectorIdx) addRawLocked(cur *vectorSnapshot, id NodeID, raw []byte) (*vectorSnapshot, error) {
	return v.addRawLockedAtLevel(cur, id, raw, -1)
}

// addRawLockedAtLevel adds id with the given HNSW level; a negative level
// samples from the writer RNG. Delta replay supplies the stored level so a
// reloaded graph does not depend on RNG state.
func (v *VectorIdx) addRawLockedAtLevel(cur *vectorSnapshot, id NodeID, raw []byte, fixedLevel int) (*vectorSnapshot, error) {
	if len(raw) != v.vecBytes {
		return nil, ErrInvalidVector
	}
	if ord, exists := cur.locate(id); exists {
		_ = ord
		if !cur.tomb.Contains(uint64(id)) {
			return nil, ErrDuplicateNodeID
		}
		return v.resurrectLocked(cur, ord, id, raw)
	}
	if v.wal != nil {
		if err := v.appendWALRecord(walTypeAdd, id, raw); err != nil {
			return nil, err
		}
		v.lsn++
	}
	level := fixedLevel
	if level < 0 {
		level = v.sampleLevelLocked()
	}
	// Neighbor search runs before the node becomes visible: search the graph
	// without the new node, then append and wire.
	type fwdLinks struct {
		level int
		ids   []NodeID
	}
	var fwds []fwdLinks
	if cur.hasEntry {
		ctx := acquireSearchContext()
		ctx.resetForNodes(len(cur.ids), v.vecBytes)
		ctx.queryNorm = cur.rawNorm(raw)
		entryOrd, ok := cur.locate(cur.entry)
		curBest := -1
		if ok {
			curBest = entryOrd
			for l := cur.maxLevel; l > level; l-- {
				curBest = cur.greedyStep(curBest, l, raw, ctx.queryNorm)
			}
			for l := min(cur.maxLevel, level); l >= 0; l-- {
				ef := int(cur.cfg.EfConstruction)
				cands := cur.searchLayer(ctx, raw, ctx.queryNorm, nil, curBest, ef, l)
				m := int(cur.cfg.M)
				if l == 0 {
					m = int(cur.cfg.M0)
				}
				picked := pickNeighbors(cur, cands, m)
				fwd := make([]NodeID, len(picked))
				for i, p := range picked {
					fwd[i] = cur.ids[p.ord]
				}
				fwds = append(fwds, fwdLinks{level: l, ids: fwd})
				if len(picked) != 0 {
					curBest = picked[0].ord
				}
			}
		}
		releaseSearchContext(ctx)
	}
	ns := cloneSnapshot(cur)
	ord := len(ns.ids)
	ns.ids = append(ns.ids, id)
	ns.vectors = append(ns.vectors, raw...)
	ns.appendVectorNorm(raw)
	ns.levels = append(ns.levels, level)
	holder := &atomic.Pointer[nodeLayers]{}
	holder.Store(&nodeLayers{layers: make([][]NodeID, level+1)})
	ns.links = append(ns.links, holder)
	// Locator: dense fast path when feasible, else sorted insert.
	newCount := len(ns.ids)
	if denseEnsure(ns, id, newCount) {
		denseSet(ns, id, ord)
	} else {
		sortedInsert(ns, id, ord)
	}
	// Wire forward links captured above (ordinals pre-append are still valid
	// because append never shifts existing ordinals).
	for _, fw := range fwds {
		l := fw.level
		g := ns.links[ord].Load()
		g.layers[l] = append(g.layers[l][:0], fw.ids...)
		for _, nb := range fw.ids {
			nOrd, ok := ns.locate(nb)
			if !ok || nOrd == ord {
				continue
			}
			if l > ns.levels[nOrd] {
				continue
			}
			m := int(ns.cfg.M)
			if l == 0 {
				m = int(ns.cfg.M0)
			}
			v.linkAppend(ns, nOrd, l, id, m)
		}
	}
	if !ns.hasEntry || level > ns.maxLevel {
		ns.entry = id
		ns.hasEntry = true
		ns.maxLevel = level
	}
	ns.gen = v.gen.Add(1)
	v.markPendingLocked(id)
	return ns, nil
}

// linkAppend adds id to ord's layer-l neighbors with closest/heuristic
// pruning. It clones the node's layer set so published readers keep theirs.
func (v *VectorIdx) linkAppend(s *vectorSnapshot, ord, l int, id NodeID, m int) {
	old := s.links[ord].Load()
	if old == nil || l >= len(old.layers) {
		return
	}
	nl := &nodeLayers{layers: make([][]NodeID, len(old.layers))}
	copy(nl.layers, old.layers)
	nl.layers[l] = append(append([]NodeID(nil), old.layers[l]...), id)
	if len(nl.layers[l]) > m {
		nl.layers[l] = v.pruneList(s, ord, nl.layers[l], m)
	}
	s.links[ord].Store(nl)
}

type pruneScore struct {
	id   NodeID
	dist float64
	ord  int
}

// pruneList keeps the m best neighbors of ord from nbs by current distance.
func (v *VectorIdx) pruneList(s *vectorSnapshot, ord int, nbs []NodeID, m int) []NodeID {
	scored := v.pruneScored[:0]
	for _, nb := range nbs {
		nOrd, ok := s.locate(nb)
		if !ok {
			continue
		}
		scored = append(scored, pruneScore{id: nb, dist: s.distOrds(ord, nOrd), ord: nOrd})
	}
	v.pruneScored = scored
	if s.cfg.UseHeuristic {
		// Heuristic on neighbor-neighbor diversity with base as query.
		cands := v.pruneCands[:0]
		for _, sc := range scored {
			cands = append(cands, searchEntry{ord: sc.ord, dist: sc.dist})
		}
		v.pruneCands = cands
		picked := selectHeuristic(cands, m, s)
		out := make([]NodeID, len(picked))
		for i, p := range picked {
			out[i] = s.ids[p.ord]
		}
		return out
	}
	sort.Slice(scored, func(i, j int) bool {
		if scored[i].dist != scored[j].dist {
			return scored[i].dist < scored[j].dist
		}
		return scored[i].id < scored[j].id
	})
	if len(scored) > m {
		scored = scored[:m]
	}
	out := make([]NodeID, len(scored))
	for i, sc := range scored {
		out[i] = sc.id
	}
	return out
}

// resurrectLocked replaces the vector of a tombstoned node and reconnects it
// to the graph. The node keeps its ordinal and HNSW level. The vector write
// is in-place (concurrent readers may observe it fresh); links swap via new
// per-node layer sets. Caller holds the writer lock.
func (v *VectorIdx) resurrectLocked(cur *vectorSnapshot, ord int, id NodeID, raw []byte) (*vectorSnapshot, error) {
	if len(raw) != v.vecBytes {
		return nil, ErrInvalidVector
	}
	if v.wal != nil {
		if err := v.appendWALRecord(walTypeAdd, id, raw); err != nil {
			return nil, err
		}
		v.lsn++
	}
	ns := cloneSnapshot(cur)
	copy(ns.vectorAt(ord), raw)
	if ns.cfg.Metric == MetricCosine {
		ns.norms[ord] = ns.rawNorm(raw)
	}
	ns.tombSorted = removeTombSorted(ns.tombSorted, id)
	ns.tomb, ns.tombSorted = rebuildTombeset(ns.tombSorted)
	if len(ns.ids) > 1 && ns.hasEntry {
		if seed, ok := wiringSeed(ns, id); ok {
			level := ns.levels[ord]
			fresh := &nodeLayers{layers: make([][]NodeID, level+1)}
			ns.links[ord].Store(fresh)
			ctx := acquireSearchContext()
			ctx.resetForNodes(len(ns.ids), v.vecBytes)
			restoreEntry := ns.entry
			ns.entry = seed
			v.wireRebuilt(ctx, ns, raw, ns.normAt(ord), ord, level, id)
			ns.entry = restoreEntry
			releaseSearchContext(ctx)
		}
	}
	ns.gen = v.gen.Add(1)
	v.markPendingLocked(id)
	return ns, nil
}

// wiringSeed returns a traversal seed other than exclude that reaches
// maxLevel. Only the entry is guaranteed to have maxLevel; when exclude is
// the entry and no other node shares that level, rewiring is skipped.
func wiringSeed(s *vectorSnapshot, exclude NodeID) (NodeID, bool) {
	ids := s.sortedIDs
	if len(ids) == 0 {
		ids = s.ids
	}
	for _, id := range ids {
		if id == exclude {
			continue
		}
		ord, ok := s.locate(id)
		if ok && s.levels[ord] == s.maxLevel {
			return id, true
		}
	}
	return 0, false
}

// Delete tombstones id. Tombstoned nodes stay traversable but are never
// returned; Compact purges them. Delete is idempotent for tombstoned nodes.
func (v *VectorIdx) Delete(id NodeID) error {
	v.writeMu.Lock()
	if v.closed.Load() {
		v.writeMu.Unlock()
		return ErrClosed
	}
	cur := v.view.Load()
	if _, ok := cur.locate(id); !ok {
		v.writeMu.Unlock()
		return ErrNodeNotFound
	}
	if cur.tomb.Contains(uint64(id)) {
		v.writeMu.Unlock()
		return nil
	}
	if v.wal != nil {
		if err := v.appendWALRecord(walTypeDelete, id, nil); err != nil {
			return err
		}
		v.lsn++
	}
	ns := cloneSnapshot(cur)
	ns.tombSorted = insertTombSorted(ns.tombSorted, id)
	ns.tomb, ns.tombSorted = rebuildTombeset(ns.tombSorted)
	ns.gen = v.gen.Add(1)
	v.view.Store(ns)
	v.markPendingLocked(id)
	v.maybeAutoCompactLocked(ns)
	wal := v.takePendingSyncLocked()
	v.writeMu.Unlock()
	return v.syncWALAfterWrite(wal)
}

// maybeAutoCompactLocked enqueues background maintenance when policy is
// exceeded: tombstone ratio selects Compact (graph rebuild), while delta
// count or delta fragmentation selects Vacuum (base merge). Caller holds
// writeMu.
func (v *VectorIdx) maybeAutoCompactLocked(snap *vectorSnapshot) {
	if v.closed.Load() {
		return
	}
	live := len(snap.ids) - len(snap.tombSorted)
	compact := false
	if ratio := snap.cfg.AutoCompactRatio; ratio > 0 && live > 0 {
		if snap.cfg.CompactMinNodes <= 0 || live >= snap.cfg.CompactMinNodes {
			compact = float64(len(snap.tombSorted))/float64(live) >= ratio
		}
	}
	vacuum := false
	if m := v.manifest.Load(); m != nil && len(m.deltas) > 0 {
		if max := snap.cfg.AutoVacuumDeltas; max > 0 && len(m.deltas) >= max {
			vacuum = true
		}
		if frag := snap.cfg.AutoVacuumFragmentation; frag > 0 && m.fragmentation() >= frag {
			vacuum = true
		}
	}
	if !compact && !vacuum {
		return
	}
	if compact {
		v.tryEnqueueLocked(func() { _, _ = v.Compact() })
		return
	}
	v.tryEnqueueLocked(func() { _ = v.Vacuum() })
}

// tryEnqueueLocked submits fn to the async queue without blocking. Caller
// holds writeMu.
func (v *VectorIdx) tryEnqueueLocked(fn func()) {
	v.asyncPending.Add(1)
	ch := v.asyncCh
	if ch == nil {
		v.asyncPending.Add(-1)
		return
	}
	select {
	case ch <- asyncOp{fn: func() { fn(); v.asyncPending.Add(-1) }}:
	default:
		v.asyncPending.Add(-1)
	}
}

// ---- typed Add/Search wrappers ----

// AddInt8 inserts one int8 vector. A live duplicate NodeID returns
// ErrDuplicateNodeID; re-adding a tombstoned NodeID replaces its vector and
// reconnects it, so Delete followed by Add is an update.
func (v *VectorIdx) AddInt8(id NodeID, vec []int8) error {
	if v.cfgBase.ElementType != ElementInt8 {
		return ErrInvalidVector
	}
	if len(vec) != v.dims {
		return ErrInvalidVector
	}
	v.writeMu.Lock()
	if v.closed.Load() {
		v.writeMu.Unlock()
		return ErrClosed
	}
	raw := encodeRawInt8(v.encodeBuf[:0], vec)
	v.encodeBuf = raw
	cur := v.view.Load()
	ns, err := v.addRawLocked(cur, id, raw)
	if err != nil {
		v.writeMu.Unlock()
		return err
	}
	v.view.Store(ns)
	v.maybeAutoCompactLocked(ns)
	wal := v.takePendingSyncLocked()
	v.writeMu.Unlock()
	return v.syncWALAfterWrite(wal)
}

// AddUint8 inserts one uint8 vector.
func (v *VectorIdx) AddUint8(id NodeID, vec []uint8) error {
	if v.cfgBase.ElementType != ElementUint8 {
		return ErrInvalidVector
	}
	if len(vec) != v.dims {
		return ErrInvalidVector
	}
	v.writeMu.Lock()
	if v.closed.Load() {
		v.writeMu.Unlock()
		return ErrClosed
	}
	raw := encodeRawUint8(v.encodeBuf[:0], vec)
	v.encodeBuf = raw
	cur := v.view.Load()
	ns, err := v.addRawLocked(cur, id, raw)
	if err != nil {
		v.writeMu.Unlock()
		return err
	}
	v.view.Store(ns)
	v.maybeAutoCompactLocked(ns)
	wal := v.takePendingSyncLocked()
	v.writeMu.Unlock()
	return v.syncWALAfterWrite(wal)
}

// AddInt16 inserts one int16 vector.
func (v *VectorIdx) AddInt16(id NodeID, vec []int16) error {
	if v.cfgBase.ElementType != ElementInt16 {
		return ErrInvalidVector
	}
	if len(vec) != v.dims {
		return ErrInvalidVector
	}
	v.writeMu.Lock()
	if v.closed.Load() {
		v.writeMu.Unlock()
		return ErrClosed
	}
	raw := encodeRawInt16(v.encodeBuf[:0], vec)
	v.encodeBuf = raw
	cur := v.view.Load()
	ns, err := v.addRawLocked(cur, id, raw)
	if err != nil {
		v.writeMu.Unlock()
		return err
	}
	v.view.Store(ns)
	v.maybeAutoCompactLocked(ns)
	wal := v.takePendingSyncLocked()
	v.writeMu.Unlock()
	return v.syncWALAfterWrite(wal)
}

// AddUint16 inserts one uint16 vector.
func (v *VectorIdx) AddUint16(id NodeID, vec []uint16) error {
	if v.cfgBase.ElementType != ElementUint16 {
		return ErrInvalidVector
	}
	if len(vec) != v.dims {
		return ErrInvalidVector
	}
	v.writeMu.Lock()
	if v.closed.Load() {
		v.writeMu.Unlock()
		return ErrClosed
	}
	raw := encodeRawUint16(v.encodeBuf[:0], vec)
	v.encodeBuf = raw
	cur := v.view.Load()
	ns, err := v.addRawLocked(cur, id, raw)
	if err != nil {
		v.writeMu.Unlock()
		return err
	}
	v.view.Store(ns)
	v.maybeAutoCompactLocked(ns)
	wal := v.takePendingSyncLocked()
	v.writeMu.Unlock()
	return v.syncWALAfterWrite(wal)
}

// AddFloat32 inserts one float32 vector.
func (v *VectorIdx) AddFloat32(id NodeID, vec []float32) error {
	if v.cfgBase.ElementType != ElementFloat32 {
		return ErrInvalidVector
	}
	if len(vec) != v.dims {
		return ErrInvalidVector
	}
	v.writeMu.Lock()
	if v.closed.Load() {
		v.writeMu.Unlock()
		return ErrClosed
	}
	raw := encodeRawFloat32(v.encodeBuf[:0], vec)
	v.encodeBuf = raw
	cur := v.view.Load()
	ns, err := v.addRawLocked(cur, id, raw)
	if err != nil {
		v.writeMu.Unlock()
		return err
	}
	v.view.Store(ns)
	v.maybeAutoCompactLocked(ns)
	wal := v.takePendingSyncLocked()
	v.writeMu.Unlock()
	return v.syncWALAfterWrite(wal)
}

// queryEncoder writes the typed query into dst (reusing pooled capacity)
// and returns the raw encoding. It runs lock-free with ctx alive.
type queryEncoder func(dst []byte) []byte

func (v *VectorIdx) searchEncoded(filter CandidateFilter, k int, encode queryEncoder) ([]Result, error) {
	return v.searchEncodedWithHint(filter, k, UnknownFilterHint, encode)
}

// searchEncodedWithHint is searchEncoded with a filter cardinality hint for
// the cost-based flat fallback (Weaviate flatSearchCutoff analogue).
func (v *VectorIdx) searchEncodedWithHint(filter CandidateFilter, k int, filterHint int, encode queryEncoder) ([]Result, error) {
	return v.searchEncodedWithOptions(nil, filter, k, filterHint, 0, 0, encode)
}

func (v *VectorIdx) searchEncodedWithOptions(queryCtx context.Context, filter CandidateFilter, k, filterHint int, ef, oversample uint32, encode queryEncoder) ([]Result, error) {
	if k <= 0 {
		return nil, ErrInvalidQuery
	}
	token := v.readers.Acquire()
	snap := v.view.Load()
	if v.closed.Load() {
		v.readers.Release(token)
		return nil, ErrClosed
	}
	out, err := snap.searchEncodedWithOptions(queryCtx, filter, k, filterHint, ef, oversample, encode)
	v.readers.Release(token)
	return out, err
}

func (s *vectorSnapshot) searchEncodedWithOptions(queryCtx context.Context, filter CandidateFilter, k, filterHint int, ef, oversample uint32, encode queryEncoder) ([]Result, error) {
	if k <= 0 {
		return nil, ErrInvalidQuery
	}
	if queryCtx != nil {
		if err := queryCtx.Err(); err != nil {
			return nil, err
		}
	}
	ctx := acquireSearchContext()
	defer releaseSearchContext(ctx)
	ctx.ctx = queryCtx
	ctx.err = nil
	if cap(ctx.queryBuf) < s.vecBytes {
		ctx.queryBuf = make([]byte, s.vecBytes)
	}
	query := encode(ctx.queryBuf[:0])
	ctx.queryBuf = query
	if !s.hasEntry || len(s.ids) == 0 {
		return nil, nil
	}
	if k > len(s.ids) {
		k = len(s.ids)
	}
	results, _ := s.searchWithHint(ctx, query, filter, k, filterHint, ef, oversample)
	if ctx.err != nil {
		return nil, ctx.err
	}
	out := make([]Result, len(results))
	copy(out, results)
	return out, nil
}

func (v *VectorIdx) searchIntoEncoded(dst []Result, filter CandidateFilter, k int, encode queryEncoder) ([]Result, error) {
	return v.searchIntoEncodedWithHint(dst, filter, k, UnknownFilterHint, encode)
}

func (v *VectorIdx) searchIntoEncodedWithHint(dst []Result, filter CandidateFilter, k int, filterHint int, encode queryEncoder) ([]Result, error) {
	return v.searchIntoEncodedWithOptions(nil, dst, filter, k, filterHint, 0, 0, encode)
}

func (v *VectorIdx) searchIntoEncodedWithOptions(queryCtx context.Context, dst []Result, filter CandidateFilter, k, filterHint int, ef, oversample uint32, encode queryEncoder) ([]Result, error) {
	if k <= 0 {
		return nil, ErrInvalidQuery
	}
	token := v.readers.Acquire()
	snap := v.view.Load()
	if v.closed.Load() {
		v.readers.Release(token)
		return nil, ErrClosed
	}
	dst, err := snap.searchIntoEncodedWithOptions(queryCtx, dst, filter, k, filterHint, ef, oversample, encode)
	v.readers.Release(token)
	return dst, err
}

func (s *vectorSnapshot) searchIntoEncodedWithOptions(queryCtx context.Context, dst []Result, filter CandidateFilter, k, filterHint int, ef, oversample uint32, encode queryEncoder) ([]Result, error) {
	if k <= 0 {
		return nil, ErrInvalidQuery
	}
	if queryCtx != nil {
		if err := queryCtx.Err(); err != nil {
			return dst, err
		}
	}
	ctx := acquireSearchContext()
	defer releaseSearchContext(ctx)
	ctx.ctx = queryCtx
	ctx.err = nil
	if cap(ctx.queryBuf) < s.vecBytes {
		ctx.queryBuf = make([]byte, s.vecBytes)
	}
	query := encode(ctx.queryBuf[:0])
	ctx.queryBuf = query
	if s.hasEntry && len(s.ids) != 0 {
		if k > len(s.ids) {
			k = len(s.ids)
		}
		results, _ := s.searchWithHint(ctx, query, filter, k, filterHint, ef, oversample)
		if ctx.err != nil {
			return dst, ctx.err
		}
		dst = append(dst, results...)
	}
	return dst, nil
}

// SearchInt8 returns up to k nearest live neighbors of an int8 query.
func (v *VectorIdx) SearchInt8(query []int8, filter CandidateFilter, k int) ([]Result, error) {
	if v.cfgBase.ElementType != ElementInt8 || len(query) != v.dims {
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
	if v.cfgBase.ElementType != ElementUint8 || len(query) != v.dims {
		return nil, ErrInvalidVector
	}
	return v.searchEncoded(filter, k, func(dst []byte) []byte {
		return append(dst, query...)
	})
}

// SearchInt16 returns up to k nearest live neighbors of an int16 query.
func (v *VectorIdx) SearchInt16(query []int16, filter CandidateFilter, k int) ([]Result, error) {
	if v.cfgBase.ElementType != ElementInt16 || len(query) != v.dims {
		return nil, ErrInvalidVector
	}
	return v.searchEncoded(filter, k, func(dst []byte) []byte {
		return encodeRawInt16(dst, query)
	})
}

// SearchUint16 returns up to k nearest live neighbors of a uint16 query.
func (v *VectorIdx) SearchUint16(query []uint16, filter CandidateFilter, k int) ([]Result, error) {
	if v.cfgBase.ElementType != ElementUint16 || len(query) != v.dims {
		return nil, ErrInvalidVector
	}
	return v.searchEncoded(filter, k, func(dst []byte) []byte {
		return encodeRawUint16(dst, query)
	})
}

// SearchFloat32 returns up to k nearest live neighbors of a float32 query.
func (v *VectorIdx) SearchFloat32(query []float32, filter CandidateFilter, k int) ([]Result, error) {
	if v.cfgBase.ElementType != ElementFloat32 || len(query) != v.dims {
		return nil, ErrInvalidVector
	}
	return v.searchEncoded(filter, k, func(dst []byte) []byte {
		return encodeRawFloat32(dst, query)
	})
}

// SearchFloat32WithOptions is SearchFloat32 with cancellation and per-query
// ef/oversample/filter-hint controls.
func (v *VectorIdx) SearchFloat32WithOptions(queryCtx context.Context, query []float32, opts SearchOptions) ([]Result, error) {
	if v.cfgBase.ElementType != ElementFloat32 || len(query) != v.dims {
		return nil, ErrInvalidVector
	}
	hint := opts.FilterHint
	if hint == 0 {
		hint = UnknownFilterHint
	}
	return v.searchEncodedWithOptions(queryCtx, opts.Filter, opts.K, hint, opts.EfSearch, opts.Oversample, func(dst []byte) []byte {
		return encodeRawFloat32(dst, query)
	})
}

// SearchFloat32Context is SearchFloat32 with cancellation.
func (v *VectorIdx) SearchFloat32Context(queryCtx context.Context, query []float32, filter CandidateFilter, k int) ([]Result, error) {
	return v.SearchFloat32WithOptions(queryCtx, query, SearchOptions{K: k, Filter: filter, FilterHint: UnknownFilterHint})
}

// SearchIntoFloat32WithOptions is SearchIntoFloat32 with cancellation and
// per-query controls. It reuses dst and pooled scratch after warm-up.
func (v *VectorIdx) SearchIntoFloat32WithOptions(queryCtx context.Context, dst []Result, query []float32, opts SearchOptions) ([]Result, error) {
	if v.cfgBase.ElementType != ElementFloat32 || len(query) != v.dims {
		return nil, ErrInvalidVector
	}
	hint := opts.FilterHint
	if hint == 0 {
		hint = UnknownFilterHint
	}
	return v.searchIntoEncodedWithOptions(queryCtx, dst, opts.Filter, opts.K, hint, opts.EfSearch, opts.Oversample, func(scratch []byte) []byte {
		return encodeRawFloat32(scratch, query)
	})
}

// SearchIntoFloat32Context is SearchIntoFloat32 with cancellation.
func (v *VectorIdx) SearchIntoFloat32Context(queryCtx context.Context, dst []Result, query []float32, filter CandidateFilter, k int) ([]Result, error) {
	return v.SearchIntoFloat32WithOptions(queryCtx, dst, query, SearchOptions{K: k, Filter: filter, FilterHint: UnknownFilterHint})
}

// SearchCandidatesFloat32 scores exactly the supplied candidate NodeIDs and
// returns the best k. Candidates should be unique. It is the selective-filter
// path: cost scales with len(candidates), not with the index size. Tombstoned
// or unknown IDs are skipped.
func (v *VectorIdx) SearchCandidatesFloat32(queryCtx context.Context, candidates []NodeID, query []float32, k int) ([]Result, error) {
	if v.cfgBase.ElementType != ElementFloat32 || len(query) != v.dims {
		return nil, ErrInvalidVector
	}
	if k <= 0 {
		return nil, ErrInvalidQuery
	}
	out := make([]Result, 0, min(k, len(candidates)))
	return v.SearchCandidatesIntoFloat32(queryCtx, out, candidates, query, k)
}

// SearchCandidatesIntoFloat32 appends the best k candidate matches to dst.
func (v *VectorIdx) SearchCandidatesIntoFloat32(queryCtx context.Context, dst []Result, candidates []NodeID, query []float32, k int) ([]Result, error) {
	if v.cfgBase.ElementType != ElementFloat32 || len(query) != v.dims {
		return dst, ErrInvalidVector
	}
	return v.searchCandidatesIntoFloat32(queryCtx, dst, candidates, query, k)
}

func (v *VectorIdx) searchCandidatesIntoFloat32(queryCtx context.Context, dst []Result, candidates []NodeID, query []float32, k int) ([]Result, error) {
	if k <= 0 {
		return dst, ErrInvalidQuery
	}
	token := v.readers.Acquire()
	snap := v.view.Load()
	if v.closed.Load() {
		v.readers.Release(token)
		return dst, ErrClosed
	}
	out, err := snap.searchCandidatesFloat32(queryCtx, dst, candidates, query, k)
	v.readers.Release(token)
	return out, err
}

// SearchIntoInt8 appends up to k matches to dst, reusing caller storage.
func (v *VectorIdx) SearchIntoInt8(dst []Result, query []int8, filter CandidateFilter, k int) ([]Result, error) {
	if v.cfgBase.ElementType != ElementInt8 || len(query) != v.dims {
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
	if v.cfgBase.ElementType != ElementUint8 || len(query) != v.dims {
		return nil, ErrInvalidVector
	}
	return v.searchIntoEncoded(dst, filter, k, func(scratch []byte) []byte {
		return append(scratch, query...)
	})
}

// SearchIntoInt16 appends up to k matches to dst, reusing caller storage.
func (v *VectorIdx) SearchIntoInt16(dst []Result, query []int16, filter CandidateFilter, k int) ([]Result, error) {
	if v.cfgBase.ElementType != ElementInt16 || len(query) != v.dims {
		return nil, ErrInvalidVector
	}
	return v.searchIntoEncoded(dst, filter, k, func(scratch []byte) []byte {
		return encodeRawInt16(scratch, query)
	})
}

// SearchIntoUint16 appends up to k matches to dst, reusing caller storage.
func (v *VectorIdx) SearchIntoUint16(dst []Result, query []uint16, filter CandidateFilter, k int) ([]Result, error) {
	if v.cfgBase.ElementType != ElementUint16 || len(query) != v.dims {
		return nil, ErrInvalidVector
	}
	return v.searchIntoEncoded(dst, filter, k, func(scratch []byte) []byte {
		return encodeRawUint16(scratch, query)
	})
}

// SearchIntoFloat32 appends up to k matches to dst, reusing caller storage.
func (v *VectorIdx) SearchIntoFloat32(dst []Result, query []float32, filter CandidateFilter, k int) ([]Result, error) {
	if v.cfgBase.ElementType != ElementFloat32 || len(query) != v.dims {
		return nil, ErrInvalidVector
	}
	return v.searchIntoEncoded(dst, filter, k, func(scratch []byte) []byte {
		return encodeRawFloat32(scratch, query)
	})
}

// ExactSearchFloat32 runs the scalar exact reference scan for the index
// metric. It is the recall oracle for tests and benchmarks.
func (v *VectorIdx) ExactSearchFloat32(query []float32, filter CandidateFilter, k int) ([]Result, error) {
	if v.cfgBase.ElementType != ElementFloat32 || len(query) != v.dims {
		return nil, ErrInvalidVector
	}
	if k <= 0 {
		return nil, ErrInvalidQuery
	}
	raw := encodeRawFloat32(nil, query)
	token := v.readers.Acquire()
	snap := v.view.Load()
	if v.closed.Load() {
		v.readers.Release(token)
		return nil, ErrClosed
	}
	out, err := snap.exactSearch(nil, raw, filter, k)
	v.readers.Release(token)
	return out, err
}

// ExactSearchFloat32Context is ExactSearchFloat32 with cancellation.
func (v *VectorIdx) ExactSearchFloat32Context(queryCtx context.Context, query []float32, filter CandidateFilter, k int) ([]Result, error) {
	if v.cfgBase.ElementType != ElementFloat32 || len(query) != v.dims {
		return nil, ErrInvalidVector
	}
	if k <= 0 {
		return nil, ErrInvalidQuery
	}
	if queryCtx != nil {
		if err := queryCtx.Err(); err != nil {
			return nil, err
		}
	}
	raw := encodeRawFloat32(nil, query)
	token := v.readers.Acquire()
	snap := v.view.Load()
	if v.closed.Load() {
		v.readers.Release(token)
		return nil, ErrClosed
	}
	out, err := snap.exactSearch(queryCtx, raw, filter, k)
	v.readers.Release(token)
	return out, err
}

// SearchWithFilterHint runs SearchFloat32 with a filter cardinality hint for
// the cost-based flat fallback.
func (v *VectorIdx) SearchWithFilterHint(query []float32, filter CandidateFilter, k int, filterHint int) ([]Result, error) {
	if v.cfgBase.ElementType != ElementFloat32 || len(query) != v.dims {
		return nil, ErrInvalidVector
	}
	return v.searchEncodedWithHint(filter, k, filterHint, func(dst []byte) []byte {
		return encodeRawFloat32(dst, query)
	})
}

// Checkpoint persists a self-consistent generation and truncates the WAL
// tail it covers.
func (v *VectorIdx) Checkpoint() error {
	v.writeMu.Lock()
	defer v.writeMu.Unlock()
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
// compacted index is deterministic given its live set. A concurrent Compact
// returns ErrCompactionInProgress.
func (v *VectorIdx) Compact() (CompactionResult, error) {
	if !v.compactMu.TryLock() {
		return CompactionResult{}, ErrCompactionInProgress
	}
	defer v.compactMu.Unlock()
	start := time.Now()
	v.writeMu.Lock()
	defer v.writeMu.Unlock()
	if v.closed.Load() {
		return CompactionResult{}, ErrClosed
	}
	cur := v.view.Load()
	result := CompactionResult{
		NodesBefore:      len(cur.ids),
		TombstonesPurged: len(cur.tombSorted),
	}
	type live struct {
		id  NodeID
		ord int
	}
	var liveIDs []NodeID
	if len(cur.sortedIDs) != 0 {
		liveIDs = make([]NodeID, 0, len(cur.ids)-len(cur.tombSorted))
		for _, id := range cur.sortedIDs {
			if cur.tomb.Contains(uint64(id)) {
				continue
			}
			liveIDs = append(liveIDs, id)
		}
	} else {
		liveIDs = make([]NodeID, 0, len(cur.ids)-len(cur.tombSorted))
		for _, id := range cur.ids {
			if cur.tomb.Contains(uint64(id)) {
				continue
			}
			liveIDs = append(liveIDs, id)
		}
		sort.Slice(liveIDs, func(i, j int) bool { return liveIDs[i] < liveIDs[j] })
	}
	lives := make([]live, 0, len(liveIDs))
	for _, id := range liveIDs {
		ord, ok := cur.locate(id)
		if !ok {
			return CompactionResult{}, ErrCorruptStorage
		}
		lives = append(lives, live{id: id, ord: ord})
	}
	ns := &vectorSnapshot{
		cfg:      cur.cfg,
		dist:     v.dist,
		dot:      v.dot,
		vecBytes: v.vecBytes,
		elemSize: v.elemSize,
		dims:     v.dims,
		vectors:  make([]byte, 0, len(lives)*v.vecBytes),
	}
	if cur.cfg.Metric == MetricCosine {
		ns.norms = make([]float64, 0, len(lives))
	}
	v.rng = rand.New(rand.NewSource(0xC0FAC7))
	ctx := acquireSearchContext()
	defer releaseSearchContext(ctx)
	for _, lv := range lives {
		raw := cur.vectorAt(lv.ord)
		level := v.sampleLevelLocked()
		ord := len(ns.ids)
		ns.ids = append(ns.ids, lv.id)
		ns.vectors = append(ns.vectors, raw...)
		if ns.cfg.Metric == MetricCosine {
			ns.norms = append(ns.norms, cur.norms[lv.ord])
		}
		ns.levels = append(ns.levels, level)
		holder := &atomic.Pointer[nodeLayers]{}
		holder.Store(&nodeLayers{layers: make([][]NodeID, level+1)})
		ns.links = append(ns.links, holder)
		if denseEnsure(ns, lv.id, len(ns.ids)) {
			denseSet(ns, lv.id, ord)
		} else {
			sortedInsert(ns, lv.id, ord)
		}
		if len(ns.ids) > 1 {
			ctx.resetForNodes(len(ns.ids), v.vecBytes)
			v.wireRebuilt(ctx, ns, raw, cur.normAt(lv.ord), ord, level, lv.id)
		} else {
			ns.entry = lv.id
			ns.hasEntry = true
			ns.maxLevel = level
		}
		if level > ns.maxLevel {
			ns.entry = lv.id
			ns.maxLevel = level
		}
	}
	ns.gen = v.gen.Add(1)
	v.view.Store(ns)
	result.NodesAfter = len(ns.ids)
	result.BytesReclaimed = int64(result.NodesBefore-result.NodesAfter) * int64(v.vecBytes)
	if v.path != "" {
		if m := v.manifest.Load(); m != nil {
			result.DeltasMerged = len(m.deltas)
			for _, d := range m.deltas {
				result.ReclaimedRecords += int64(d.tombstones)
			}
		}
		result.ReclaimedRecords += int64(v.coalescedRecords.Load())
		if err := v.vacuumLocked(); err != nil {
			return result, err
		}
	}
	result.Duration = time.Since(start)
	v.lastCompact.Store(&result)
	return result, nil
}

// wireRebuilt connects rebuilt ordinal ord (already appended) into the
// prefix graph. It mirrors addRawLocked without WAL/LSN/locator-append.
// ns is private until published, so direct mutation is safe.
func (v *VectorIdx) wireRebuilt(ctx *SearchContext, s *vectorSnapshot, raw []byte, queryNorm float64, ord, level int, id NodeID) {
	ctx.queryNorm = queryNorm
	entryOrd, ok := s.locate(s.entry)
	if !ok {
		s.entry = id
		s.hasEntry = true
		s.maxLevel = level
		return
	}
	cur := entryOrd
	if cur == ord {
		cur = 0
	}
	for l := s.maxLevel; l > level; l-- {
		cur = s.greedyStep(cur, l, raw, ctx.queryNorm)
	}
	for l := min(s.maxLevel, level); l >= 0; l-- {
		ef := int(s.cfg.EfConstruction)
		cands := s.searchLayer(ctx, raw, ctx.queryNorm, nil, cur, ef, l)
		filtered := cands[:0]
		for _, c := range cands {
			if c.ord != ord {
				filtered = append(filtered, c)
			}
		}
		cands = filtered
		m := int(s.cfg.M)
		if l == 0 {
			m = int(s.cfg.M0)
		}
		picked := pickNeighbors(s, cands, m)
		g := s.links[ord].Load()
		for _, p := range picked {
			g.layers[l] = append(g.layers[l], s.ids[p.ord])
		}
		for _, p := range picked {
			nb := p.ord
			if nb == ord || l > s.levels[nb] {
				continue
			}
			v.linkAppend(s, nb, l, id, m)
		}
		if len(picked) != 0 {
			cur = picked[0].ord
		}
	}
}

// Verify performs a deep structural check: locator consistency, vector
// lengths, entry validity, edge targets, level bounds, and tombstone subset.
// Lock-free. It does not verify WAL contents.
func (v *VectorIdx) Verify() error {
	token := v.readers.Acquire()
	s := v.view.Load()
	defer v.readers.Release(token)
	if len(s.vectors) != len(s.ids)*v.vecBytes {
		return ErrCorruptStorage
	}
	if s.cfg.Metric == MetricCosine {
		if s.dot == nil || len(s.norms) != len(s.ids) {
			return ErrCorruptStorage
		}
		for ord := range s.ids {
			want := math.Sqrt(s.dot(s.vectorAt(ord), s.vectorAt(ord)))
			if math.Abs(want-s.norms[ord]) > 1e-9*math.Max(1, want) {
				return ErrCorruptStorage
			}
		}
	}
	if len(s.levels) != len(s.ids) || len(s.links) != len(s.ids) {
		return ErrCorruptStorage
	}
	seen := make(map[NodeID]int, len(s.ids))
	for ord, id := range s.ids {
		if _, dup := seen[id]; dup {
			return ErrCorruptStorage
		}
		seen[id] = ord
		g := s.links[ord].Load()
		if g == nil {
			return ErrCorruptStorage
		}
		if s.levels[ord] < 0 || len(g.layers) != s.levels[ord]+1 {
			return ErrCorruptStorage
		}
	}
	if len(s.sortedIDs) != 0 {
		if len(s.sortedIDs) != len(s.ids) || len(s.sortedOrd) != len(s.ids) {
			return ErrCorruptStorage
		}
		for i := 1; i < len(s.sortedIDs); i++ {
			if s.sortedIDs[i-1] >= s.sortedIDs[i] {
				return ErrCorruptStorage
			}
		}
		for i, id := range s.sortedIDs {
			ord := s.sortedOrd[i]
			if ord < 0 || ord >= len(s.ids) || s.ids[ord] != id {
				return ErrCorruptStorage
			}
		}
	}
	if s.useDense {
		if s.dense == nil {
			return ErrCorruptStorage
		}
		for ord, id := range s.ids {
			o, ok := denseGet(s.dense, id)
			if !ok || o != ord {
				return ErrCorruptStorage
			}
		}
	} else if len(s.sortedIDs) == 0 && len(s.ids) != 0 {
		return ErrCorruptStorage
	}
	for _, id := range s.tombSorted {
		if _, ok := seen[id]; !ok {
			return ErrCorruptStorage
		}
		if !s.tomb.Contains(uint64(id)) {
			return ErrCorruptStorage
		}
	}
	if uint64(len(s.tombSorted)) != s.tomb.Cardinality() {
		return ErrCorruptStorage
	}
	if s.hasEntry {
		entryOrd, ok := s.locate(s.entry)
		if !ok {
			return ErrCorruptStorage
		}
		if s.levels[entryOrd] != s.maxLevel {
			return ErrCorruptStorage
		}
	} else if len(s.ids) != 0 {
		return ErrCorruptStorage
	}
	for ord := range s.ids {
		g := s.links[ord].Load()
		for level, nbs := range g.layers {
			if level > s.levels[ord] {
				return ErrCorruptStorage
			}
			for _, nb := range nbs {
				nOrd, ok := s.locate(nb)
				if !ok {
					return ErrCorruptStorage
				}
				if s.levels[nOrd] < level {
					return ErrCorruptStorage
				}
			}
		}
	}
	return nil
}

// ---- async writer queue + runtime policies ----

// startAsyncWorker launches the single background writer for async ops. When
// a sync interval is configured, the worker also fsyncs the WAL once after
// each drained batch and on the interval timer (group commit).
func (v *VectorIdx) startAsyncWorker() {
	ch := make(chan asyncOp, 1024)
	v.asyncCh = ch
	var ticker *time.Ticker
	var tickC <-chan time.Time
	if v.syncInterval > 0 && v.durability == DurabilityOS {
		ticker = time.NewTicker(v.syncInterval)
		tickC = ticker.C
	}
	v.asyncWg.Add(1)
	go func() {
		defer v.asyncWg.Done()
		if ticker != nil {
			defer ticker.Stop()
		}
		for {
			select {
			case op, ok := <-ch:
				if !ok {
					return
				}
				op.fn()
				if op.done != nil {
					close(op.done)
				}
				if len(ch) == 0 {
					v.flushWAL()
				}
			case <-tickC:
				v.flushWAL()
			}
		}
	}()
}

// flushWAL fsyncs the WAL when dirty records are pending. It is called from
// the async worker and from the sync-interval ticker. The writer lock is only
// taken to read the handle, so appends keep flowing while fsync runs; a
// concurrent append re-dirties the flag and the next flush covers its tail.
func (v *VectorIdx) flushWAL() {
	if !v.walDirty.Swap(false) {
		return
	}
	v.syncMu.Lock()
	defer v.syncMu.Unlock()
	v.writeMu.Lock()
	wal := v.wal
	v.writeMu.Unlock()
	if wal == nil {
		return
	}
	if err := wal.Sync(); err != nil {
		// Keep the dirty flag so a later flush retries.
		v.walDirty.Store(true)
		return
	}
	v.walSyncs.Add(1)
}

// takePendingSyncLocked returns the segment handle recorded by the last
// successful DurabilitySync append and clears it. Caller holds writeMu.
func (v *VectorIdx) takePendingSyncLocked() *os.File {
	wal := v.pendingSync
	v.pendingSync = nil
	return wal
}

// syncWALAfterWrite makes a DurabilitySync mutation durable after the
// writer has released writeMu, so the fsync overlaps the next writer's
// graph insertion instead of serializing behind the write lock; syncs
// serialize on syncMu. The handle is the segment the record was appended
// to. A closed handle means a rotation or Close already fsynced and closed
// that segment, so the record is durable.
func (v *VectorIdx) syncWALAfterWrite(wal *os.File) error {
	if wal == nil || v.durability != DurabilitySync {
		return nil
	}
	v.syncMu.Lock()
	defer v.syncMu.Unlock()
	if err := wal.Sync(); err != nil {
		if errors.Is(err, os.ErrClosed) {
			return nil
		}
		return err
	}
	v.walSyncs.Add(1)
	return nil
}

// Sync fsyncs the WAL, establishing a durable boundary for every mutation
// acknowledged so far. It returns ErrNotPersistent for in-memory indexes.
func (v *VectorIdx) Sync() error {
	v.writeMu.Lock()
	defer v.writeMu.Unlock()
	if v.closed.Load() {
		return ErrClosed
	}
	if v.wal == nil {
		return ErrNotPersistent
	}
	if err := v.wal.Sync(); err != nil {
		return err
	}
	v.walDirty.Store(false)
	v.walSyncs.Add(1)
	return nil
}

// enqueueAsync submits fn to the background writer; callers use WaitIdle to
// observe completion. It drops the op when closed or full to stay non-blocking.
func (v *VectorIdx) enqueueAsync(fn func()) {
	v.writeMu.Lock()
	defer v.writeMu.Unlock()
	if v.closed.Load() || v.asyncCh == nil {
		return
	}
	v.tryEnqueueLocked(fn)
}

// AddAsync enqueues an Add without blocking the caller.
func (v *VectorIdx) AddAsync(id NodeID, vec []float32) {
	q := append([]float32(nil), vec...)
	v.enqueueAsync(func() { _ = v.AddFloat32(id, q) })
}

// DeleteAsync enqueues a Delete without blocking the caller.
func (v *VectorIdx) DeleteAsync(id NodeID) {
	v.enqueueAsync(func() { _ = v.Delete(id) })
}

// CompactAsync enqueues a Compact without blocking the caller.
func (v *VectorIdx) CompactAsync() {
	v.enqueueAsync(func() { _, _ = v.Compact() })
}

// PendingOps reports queued background operations.
func (v *VectorIdx) PendingOps() int64 { return v.asyncPending.Load() }

// WaitIdle blocks until queued background operations drain or ctx is done.
func (v *VectorIdx) WaitIdle(ctx context.Context) error {
	for v.asyncPending.Load() != 0 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			runtime.Gosched()
		}
	}
	return nil
}

// SetSearchParams updates runtime search policies (EfSearch, dynamic ef,
// flat cutoff/threshold, oversample) without rebuilding the graph.
func (v *VectorIdx) SetSearchParams(efSearch uint32, dynFactor, dynMin, dynMax uint32, flatCutoff, flatThreshold int, oversample uint32) error {
	v.writeMu.Lock()
	defer v.writeMu.Unlock()
	if v.closed.Load() {
		return ErrClosed
	}
	cur := v.view.Load()
	cfg := cur.cfg
	if efSearch != 0 {
		if efSearch < 1 || efSearch > 4096 {
			return ErrInvalidConfig
		}
		cfg.EfSearch = efSearch
	}
	cfg.DynamicEfFactor = dynFactor
	if dynFactor > 0 {
		if dynMin == 0 {
			dynMin = DefaultDynamicEfMin
		}
		if dynMax == 0 {
			dynMax = DefaultDynamicEfMax
		}
		if dynMin < 1 || dynMax < dynMin || dynMax > 4096 {
			return ErrInvalidConfig
		}
		cfg.DynamicEfMin, cfg.DynamicEfMax = dynMin, dynMax
	}
	if flatCutoff != 0 {
		cfg.FlatSearchCutoff = flatCutoff
	}
	if flatThreshold != 0 {
		cfg.FlatThreshold = flatThreshold
	}
	if oversample != 0 {
		if oversample < 1 || oversample > 64 {
			return ErrInvalidConfig
		}
		cfg.OversampleFactor = oversample
	}
	ns := cloneSnapshot(cur)
	ns.cfg = cfg
	ns.gen = v.gen.Add(1)
	v.view.Store(ns)
	return nil
}

// SetHeuristic toggles diversity-based neighbor selection for future writes
// and compactions.
func (v *VectorIdx) SetHeuristic(use bool) error {
	v.writeMu.Lock()
	defer v.writeMu.Unlock()
	if v.closed.Load() {
		return ErrClosed
	}
	cur := v.view.Load()
	ns := cloneSnapshot(cur)
	ns.cfg.UseHeuristic = use
	ns.gen = v.gen.Add(1)
	v.view.Store(ns)
	return nil
}

// SetAutoCompact enables (ratio > 0) or disables (ratio == 0) background
// compaction on writes.
func (v *VectorIdx) SetAutoCompact(ratio float64, minNodes int) error {
	if ratio < 0 || ratio > 1 || minNodes < 0 {
		return ErrInvalidConfig
	}
	v.writeMu.Lock()
	defer v.writeMu.Unlock()
	if v.closed.Load() {
		return ErrClosed
	}
	cur := v.view.Load()
	ns := cloneSnapshot(cur)
	ns.cfg.AutoCompactRatio = ratio
	ns.cfg.CompactMinNodes = minNodes
	ns.gen = v.gen.Add(1)
	v.view.Store(ns)
	return nil
}

// SetAutoVacuum enables (values > 0) or disables (values == 0) background
// vacuum on writes: when the manifest holds maxDeltas generations or delta
// tombstone fragmentation reaches minFragmentation, the async worker merges
// base and deltas. Either trigger can be disabled by passing zero.
func (v *VectorIdx) SetAutoVacuum(maxDeltas int, minFragmentation float64) error {
	if maxDeltas < 0 || minFragmentation < 0 || minFragmentation > 1 {
		return ErrInvalidConfig
	}
	v.writeMu.Lock()
	defer v.writeMu.Unlock()
	if v.closed.Load() {
		return ErrClosed
	}
	cur := v.view.Load()
	ns := cloneSnapshot(cur)
	ns.cfg.AutoVacuumDeltas = maxDeltas
	ns.cfg.AutoVacuumFragmentation = minFragmentation
	ns.gen = v.gen.Add(1)
	v.view.Store(ns)
	return nil
}
