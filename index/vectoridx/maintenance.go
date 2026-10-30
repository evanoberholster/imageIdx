package vectoridx

import (
	"context"
	"errors"
	"math"
	"math/rand"
	"os"
	"runtime"
	"sort"
	"sync/atomic"
	"time"
)

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
		vectors:  make([][]byte, 0, (len(lives)+vectorChunkVectors-1)>>vectorChunkShift),
	}
	if cur.cfg.Metric == MetricCosine {
		ns.normChunks = make([][]float64, 0, (len(lives)+vectorChunkVectors-1)>>vectorChunkShift)
	}
	v.rng = rand.New(rand.NewSource(0xC0FAC7))
	ctx := acquireSearchContext()
	defer releaseSearchContext(ctx)
	for _, lv := range lives {
		raw := cur.vectorAt(lv.ord)
		level := v.sampleLevelLocked()
		ord := len(ns.ids)
		ns.ids = append(ns.ids, lv.id)
		ns.appendVectorAt(ord, raw)
		if ns.cfg.Metric == MetricCosine {
			ns.setVectorNorm(ord, cur.normAt(lv.ord))
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
	if len(s.vectors) != s.vectorChunkCount() {
		return ErrCorruptStorage
	}
	for _, chunk := range s.vectors {
		if len(chunk) != vectorChunkVectors*v.vecBytes {
			return ErrCorruptStorage
		}
	}
	if s.cfg.Metric == MetricCosine {
		if s.dot == nil || len(s.normChunks) != s.vectorChunkCount() {
			return ErrCorruptStorage
		}
		for ord := range s.ids {
			want := math.Sqrt(s.dot(s.vectorAt(ord), s.vectorAt(ord)))
			if math.Abs(want-s.normAt(ord)) > 1e-9*math.Max(1, want) {
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
