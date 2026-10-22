package index

import (
	"cmp"
	"math"
	"slices"

	"github.com/evanoberholster/imageIdx/index/bitmapidx"
	"github.com/evanoberholster/imageIdx/index/geoidx"
	"github.com/evanoberholster/imageIdx/index/radixhash"
	"github.com/evanoberholster/imageIdx/index/simhash"
	"github.com/evanoberholster/imageIdx/index/timeidx"
	"github.com/evanoberholster/imageIdx/index/vectoridx"
	schema "github.com/evanoberholster/imageIdx/schema"
)

// Staged operations group one batch by engine so each engine commits once per
// affected namespace instead of once per value. The slices are writer-owned and
// reused across batches; they are only touched while mu is held.

type stagedTime struct {
	ns schema.Namespace
	m  timeidx.Mutation
}

type stagedGeo struct {
	ns     schema.Namespace
	entry  geoidx.Entry
	delete bool
}

type stagedSim struct {
	ns  schema.Namespace
	id  schema.ValueID
	rec simhash.Record
}

type stagedHash struct {
	ns     schema.Namespace
	entry  radixhash.Entry
	delete bool
}

type stagedBitmap struct {
	ns     schema.Namespace
	entry  bitmapidx.Entry
	delete bool
}

type stagedVector struct {
	ns     schema.Namespace
	id     schema.ValueID
	value  VectorValue
	delete bool
}

type stagedLive struct {
	add   bool
	entry bitmapidx.Entry
}

type stageState struct {
	time   []stagedTime
	geo    []stagedGeo
	sim    []stagedSim
	hash   []stagedHash
	bitmap []stagedBitmap
	vector []stagedVector
	live   []stagedLive
}

func (s *Store) resetStage() {
	clear(s.stage.time)
	s.stage.time = s.stage.time[:0]
	clear(s.stage.geo)
	s.stage.geo = s.stage.geo[:0]
	clear(s.stage.sim)
	s.stage.sim = s.stage.sim[:0]
	clear(s.stage.hash)
	s.stage.hash = s.stage.hash[:0]
	clear(s.stage.bitmap)
	s.stage.bitmap = s.stage.bitmap[:0]
	clear(s.stage.vector)
	s.stage.vector = s.stage.vector[:0]
	clear(s.stage.live)
	s.stage.live = s.stage.live[:0]
}

// ---- staging (no engine mutation) ----

func (s *Store) stageTime(v TimeValue, id schema.ValueID, entity schema.EntityKind) {
	s.stage.time = append(s.stage.time, stagedTime{ns: v.Namespace, m: timeidx.Insert(id, v.Timestamp)})
	s.stageLiveAdd(entity, id)
}

func (s *Store) stageTimeDelete(ns schema.Namespace, id schema.ValueID) {
	s.stage.time = append(s.stage.time, stagedTime{ns: ns, m: timeidx.Delete(id, 0)})
}

func (s *Store) stageGeo(v GeoValue, id schema.ValueID, entity schema.EntityKind) {
	s.stage.geo = append(s.stage.geo, stagedGeo{ns: v.Namespace, entry: geoidx.Entry{Key: v.Key, ImageID: id}})
	s.stageLiveAdd(entity, id)
}

func (s *Store) stageGeoDelete(ns schema.Namespace, key geoidx.S2Key, id schema.ValueID) {
	s.stage.geo = append(s.stage.geo, stagedGeo{ns: ns, entry: geoidx.Entry{Key: key, ImageID: id}, delete: true})
}

func (s *Store) stageSim(v SimValue, id schema.ValueID, entity schema.EntityKind) {
	s.stage.sim = append(s.stage.sim, stagedSim{ns: v.Namespace, id: id, rec: v.Record})
	s.stageLiveAdd(entity, id)
}

func (s *Store) stageHash(v HashValue, id schema.ValueID, entity schema.EntityKind) {
	s.stage.hash = append(s.stage.hash, stagedHash{ns: v.Namespace, entry: radixhash.Entry{Hash: v.Hash, ImageID: id}})
	s.stageLiveAdd(entity, id)
}

func (s *Store) stageHashDelete(v HashValue, id schema.ValueID) {
	s.stage.hash = append(s.stage.hash, stagedHash{ns: v.Namespace, entry: radixhash.Entry{Hash: v.Hash, ImageID: id}, delete: true})
}

func (s *Store) stageBitmap(v BitmapValue, id schema.ValueID, entity schema.EntityKind) {
	s.stage.bitmap = append(s.stage.bitmap, stagedBitmap{ns: v.Namespace, entry: bitmapidx.Entry{ImageID: id, Value: v.Value}})
	s.stageLiveAdd(entity, id)
}

func (s *Store) stageBitmapDelete(v BitmapValue, id schema.ValueID) {
	s.stage.bitmap = append(s.stage.bitmap, stagedBitmap{ns: v.Namespace, entry: bitmapidx.Entry{ImageID: id, Value: v.Value}, delete: true})
}

func (s *Store) stageVector(v VectorValue, id schema.ValueID, entity schema.EntityKind) {
	s.stage.vector = append(s.stage.vector, stagedVector{ns: v.Namespace, id: id, value: v})
	s.stageLiveAdd(entity, id)
}

func (s *Store) stageVectorDelete(ns schema.Namespace, id schema.ValueID) {
	s.stage.vector = append(s.stage.vector, stagedVector{ns: ns, id: id, delete: true})
}

func (s *Store) stageLiveAdd(entity schema.EntityKind, id schema.ValueID) {
	if id == 0 {
		return
	}
	s.stage.live = append(s.stage.live, stagedLive{add: true, entry: bitmapidx.Entry{ImageID: id, Value: livenessValue(entity)}})
}

func (s *Store) stageLiveRemove(entity schema.EntityKind, id schema.ValueID) {
	s.stage.live = append(s.stage.live, stagedLive{add: false, entry: bitmapidx.Entry{ImageID: id, Value: livenessValue(entity)}})
}

// ---- flush (one engine commit per affected namespace) ----

func (s *Store) flushStage() error {
	// Write-ahead the liveness-removal flag before the first removal commits, so
	// a crash can never leave a durable removal with the flag unset. If the
	// manifest write fails, abort before mutating any engine.
	if !s.liveRemovals.Load() && s.stageHasLivenessRemoval() {
		s.liveRemovals.Store(true)
		if err := s.writeManifest(); err != nil {
			return err
		}
	}
	s.injectCrash("start")
	// Deletions clear liveness before any value change; additions set it last.
	if err := s.flushLiveness(false); err != nil {
		return err
	}
	s.injectCrash("liveness-remove")
	if err := s.flushTime(); err != nil {
		return err
	}
	s.injectCrash("time")
	if err := s.flushGeo(); err != nil {
		return err
	}
	s.injectCrash("geo")
	if err := s.flushSim(); err != nil {
		return err
	}
	s.injectCrash("sim")
	if err := s.flushHash(); err != nil {
		return err
	}
	s.injectCrash("hash")
	if err := s.flushBitmap(); err != nil {
		return err
	}
	s.injectCrash("bitmap")
	if err := s.flushVector(); err != nil {
		return err
	}
	s.injectCrash("vector")
	s.injectCrash("liveness-add-pre")
	if err := s.flushLiveness(true); err != nil {
		return err
	}
	s.injectCrash("liveness-add")
	return nil
}

func (s *Store) stageHasLivenessRemoval() bool {
	for i := range s.stage.live {
		if !s.stage.live[i].add {
			return true
		}
	}
	return false
}

func sortStageByNS[T any](ops []T, ns func(T) schema.Namespace) {
	slices.SortStableFunc(ops, func(a, b T) int { return cmp.Compare(ns(a), ns(b)) })
}

func (s *Store) flushTime() error {
	if len(s.stage.time) == 0 {
		return nil
	}
	if s.flush.timeBatch == nil || s.flush.timeCap < len(s.stage.time) {
		s.flush.timeBatch = timeidx.NewBatch(len(s.stage.time))
		s.flush.timeCap = len(s.stage.time)
	}
	batch := s.flush.timeBatch
	sortStageByNS(s.stage.time, func(o stagedTime) schema.Namespace { return o.ns })
	for i := 0; i < len(s.stage.time); {
		j := i
		for j < len(s.stage.time) && s.stage.time[j].ns == s.stage.time[i].ns {
			j++
		}
		idx, _, err := s.timeFor(s.stage.time[i].ns)
		if err != nil {
			return err
		}
		batch.Reset()
		for k := i; k < j; k++ {
			if !batch.Append(s.stage.time[k].m) {
				return timeidx.ErrBatchTooLarge
			}
		}
		// Commit the mutations and, for an explicit generation batch, the
		// completeness watermark in the same timeidx transaction.
		if s.flushExplicit {
			err = idx.ApplyWithGeneration(batch, s.flushSourceGen)
		} else {
			err = idx.Apply(batch)
		}
		if err != nil {
			return err
		}
		i = j
	}
	return nil
}

func (s *Store) flushGeo() error {
	sortStageByNS(s.stage.geo, func(o stagedGeo) schema.Namespace { return o.ns })
	for i := 0; i < len(s.stage.geo); {
		j := i
		for j < len(s.stage.geo) && s.stage.geo[j].ns == s.stage.geo[i].ns {
			j++
		}
		idx, _, err := s.geoFor(s.stage.geo[i].ns)
		if err != nil {
			return err
		}
		inserts := s.flush.geoIns[:0]
		deletes := s.flush.geoDel[:0]
		for k := i; k < j; k++ {
			if s.stage.geo[k].delete {
				deletes = append(deletes, s.stage.geo[k].entry)
			} else {
				inserts = append(inserts, s.stage.geo[k].entry)
			}
		}
		s.flush.geoIns, s.flush.geoDel = inserts, deletes
		if len(inserts) != 0 || len(deletes) != 0 {
			if err := idx.Apply(inserts, deletes); err != nil {
				return err
			}
		}
		i = j
	}
	return nil
}

func (s *Store) flushSim() error {
	sortStageByNS(s.stage.sim, func(o stagedSim) schema.Namespace { return o.ns })
	for i := 0; i < len(s.stage.sim); {
		j := i
		for j < len(s.stage.sim) && s.stage.sim[j].ns == s.stage.sim[i].ns {
			j++
		}
		if err := s.flushSimNamespace(s.stage.sim[i:j]); err != nil {
			return err
		}
		i = j
	}
	return nil
}

// flushSimNamespace commits one namespace's staged records. SimHash now
// accepts arbitrary external IDs; the engine keeps dense append fast paths for
// dense IDs and a durable sparse overlay for gaps/out-of-order IDs.
func (s *Store) flushSimNamespace(ops []stagedSim) error {
	idx, _, err := s.simFor(ops[0].ns)
	if err != nil {
		return err
	}
	slices.SortStableFunc(ops, func(a, b stagedSim) int { return cmp.Compare(a.id, b.id) })
	unique := ops[:0]
	for _, op := range ops {
		if len(unique) > 0 && unique[len(unique)-1].id == op.id {
			unique[len(unique)-1] = op
			continue
		}
		unique = append(unique, op)
	}
	updates := s.flush.simUpd[:0]
	for _, op := range unique {
		updates = append(updates, simhash.Update{ImageID: op.id, Record: op.rec})
	}
	s.flush.simUpd = updates
	if len(updates) != 0 {
		return idx.ApplyUpdatesAt(updates, s.flushSourceGen)
	}
	return nil
}

func (s *Store) flushHash() error {
	sortStageByNS(s.stage.hash, func(o stagedHash) schema.Namespace { return o.ns })
	for i := 0; i < len(s.stage.hash); {
		j := i
		for j < len(s.stage.hash) && s.stage.hash[j].ns == s.stage.hash[i].ns {
			j++
		}
		idx, _, err := s.radixFor(s.stage.hash[i].ns)
		if err != nil {
			return err
		}
		adds := s.flush.hashAdd[:0]
		dels := s.flush.hashDel[:0]
		for k := i; k < j; k++ {
			if s.stage.hash[k].delete {
				dels = append(dels, s.stage.hash[k].entry)
			} else {
				adds = append(adds, s.stage.hash[k].entry)
			}
		}
		s.flush.hashAdd, s.flush.hashDel = adds, dels
		if len(dels) != 0 {
			if err := idx.Remove(dels); err != nil {
				return err
			}
		}
		if len(adds) != 0 {
			if err := idx.Apply(adds); err != nil {
				return err
			}
		}
		i = j
	}
	return nil
}

func (s *Store) flushBitmap() error {
	sortStageByNS(s.stage.bitmap, func(o stagedBitmap) schema.Namespace { return o.ns })
	for i := 0; i < len(s.stage.bitmap); {
		j := i
		for j < len(s.stage.bitmap) && s.stage.bitmap[j].ns == s.stage.bitmap[i].ns {
			j++
		}
		idx, _, err := s.bitmapFor(s.stage.bitmap[i].ns)
		if err != nil {
			return err
		}
		batch := idx.NewBatch()
		var addErr error
		for k := i; k < j && addErr == nil; k++ {
			if s.stage.bitmap[k].delete {
				addErr = batch.Remove(s.stage.bitmap[k].entry)
			} else {
				addErr = batch.Add(s.stage.bitmap[k].entry)
			}
		}
		if addErr != nil {
			_ = batch.Close()
			return addErr
		}
		if err := applyBitmapBatch(idx, batch); err != nil {
			return err
		}
		i = j
	}
	return nil
}

func (s *Store) flushLiveness(add bool) error {
	// The liveness index is shared across entities.
	live := s.reg.live
	count := 0
	for i := range s.stage.live {
		if s.stage.live[i].add == add {
			count++
		}
	}
	if count == 0 {
		return nil
	}
	if live == nil {
		return ErrNamespaceUnavailable
	}
	// Seqlock: mark a mutation in progress (odd) before it can become visible,
	// and complete (even) only after it is published. A matcher that reads an
	// even sequence is guaranteed no mutation overlapped its snapshot build.
	s.liveSeq.Add(1)
	batch := live.NewBatch()
	var addErr error
	for i := range s.stage.live {
		if s.stage.live[i].add != add {
			continue
		}
		if add {
			addErr = batch.Add(s.stage.live[i].entry)
		} else {
			addErr = batch.Remove(s.stage.live[i].entry)
		}
		if addErr != nil {
			break
		}
	}
	if addErr != nil {
		_ = batch.Close()
		s.liveSeq.Add(1)
		return addErr
	}
	err := applyBitmapBatch(live, batch)
	if err == nil && !add {
		// A committed removal disables the matcher's no-removal fast path.
		s.liveRemovals.Store(true)
	}
	s.liveSeq.Add(1)
	return err
}

// flushVector executes staged vector operations. vectoridx has no value-batch
// API, so operations are applied one typed vector at a time, in staged order,
// grouped by namespace. Staging itself never touches the engine.
func (s *Store) flushVector() error {
	sortStageByNS(s.stage.vector, func(o stagedVector) schema.Namespace { return o.ns })
	for i := 0; i < len(s.stage.vector); {
		j := i
		for j < len(s.stage.vector) && s.stage.vector[j].ns == s.stage.vector[i].ns {
			j++
		}
		idx, _, err := s.vectorFor(s.stage.vector[i].ns)
		if err != nil {
			return err
		}
		for k := i; k < j; k++ {
			op := &s.stage.vector[k]
			if op.delete {
				if err := deleteVectorIndex(idx, op.id); err != nil {
					return err
				}
				continue
			}
			if op.id > math.MaxUint32 {
				return ErrValueIDTooLarge
			}
			if err := addVector(idx, vectoridx.NodeID(op.id), op.value); err != nil {
				return err
			}
		}
		i = j
	}
	return nil
}
