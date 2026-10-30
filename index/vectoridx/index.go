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
	cfg        Config
	dist       distRaw
	dot        distRaw     // raw dot product; set for MetricCosine
	normChunks [][]float64 // per-chunk cached norms; set for MetricCosine
	vecBytes   int
	elemSize   int
	dims       int

	// Dense physical storage in insertion-ordinal order. Vectors live in
	// fixed-size chunks so appends never copy the whole store and an in-place
	// update copies only one chunk (copy-on-write for readers).
	ids     []NodeID
	vectors [][]byte // chunk directory; published bytes are bounded by len(ids)
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
	data, err := storage.MapFile(path, ErrCorruptStorage)
	if err != nil {
		_ = storage.ReleaseLock(lock)
		return nil, err
	}
	mapped := true
	defer func() {
		if mapped {
			_ = storage.Unmap(data)
		}
	}()
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
		Model:          header.model,
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
	// loadRegions copies the regions it retains; release the read-only map
	// before replaying deltas/WAL.
	_ = storage.Unmap(data)
	mapped = false
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

// Model returns the application-defined embedding model attached to this
// namespace, or an empty string when none was configured.
func (v *VectorIdx) Model() string {
	return v.view.Load().cfg.Model
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
	Model          string
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
		Model:          snap.cfg.Model,
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
	ns.appendVectorAt(ord, raw)
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
	ns.replaceVectorAt(ord, raw)
	if ns.cfg.Metric == MetricCosine {
		ns.replaceVectorNorm(ord, ns.rawNorm(raw))
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
