package index

import (
	"cmp"
	"math"
	"slices"

	common "github.com/evanoberholster/imageIdx/common"
	"github.com/evanoberholster/imageIdx/index/bitmapidx"
	"github.com/evanoberholster/imageIdx/index/geoidx"
	"github.com/evanoberholster/imageIdx/index/radixhash"
	"github.com/evanoberholster/imageIdx/index/simhash"
	"github.com/evanoberholster/imageIdx/index/timeidx"
	"github.com/evanoberholster/imageIdx/index/vectoridx"
)

// Staged operations group one batch by engine so each engine commits once per
// affected namespace instead of once per value. The slices are writer-owned and
// reused across batches; they are only touched while mu is held.

type stagedTime struct {
	ns common.Namespace
	m  timeidx.Mutation
}

type stagedGeo struct {
	ns     common.Namespace
	entry  geoidx.Entry
	delete bool
}

type stagedSim struct {
	ns  common.Namespace
	id  common.ValueID
	rec simhash.Record
}

type stagedHash struct {
	ns     common.Namespace
	entry  radixhash.Entry
	delete bool
}

type stagedBitmap struct {
	ns     common.Namespace
	entry  bitmapidx.Entry
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
	live   []stagedLive
}

func (s *Store) resetStage() {
	s.stage.time = s.stage.time[:0]
	s.stage.geo = s.stage.geo[:0]
	s.stage.sim = s.stage.sim[:0]
	s.stage.hash = s.stage.hash[:0]
	s.stage.bitmap = s.stage.bitmap[:0]
	s.stage.live = s.stage.live[:0]
}

// ---- staging (no engine mutation) ----

func (s *Store) stageTime(v TimeValue, id common.ValueID, entity common.EntityKind) {
	s.stage.time = append(s.stage.time, stagedTime{ns: v.Namespace, m: timeidx.Insert(id, v.Timestamp)})
	s.stageLiveAdd(entity, id)
}

func (s *Store) stageTimeDelete(ns common.Namespace, id common.ValueID) {
	s.stage.time = append(s.stage.time, stagedTime{ns: ns, m: timeidx.Delete(id, 0)})
}

func (s *Store) stageGeo(v GeoValue, id common.ValueID, entity common.EntityKind) {
	s.stage.geo = append(s.stage.geo, stagedGeo{ns: v.Namespace, entry: geoidx.Entry{Key: v.Key, ImageID: id}})
	s.stageLiveAdd(entity, id)
}

func (s *Store) stageGeoDelete(ns common.Namespace, key geoidx.S2Key, id common.ValueID) {
	s.stage.geo = append(s.stage.geo, stagedGeo{ns: ns, entry: geoidx.Entry{Key: key, ImageID: id}, delete: true})
}

func (s *Store) stageSim(v SimValue, id common.ValueID, entity common.EntityKind) {
	s.stage.sim = append(s.stage.sim, stagedSim{ns: v.Namespace, id: id, rec: v.Record})
	s.stageLiveAdd(entity, id)
}

func (s *Store) stageHash(v HashValue, id common.ValueID, entity common.EntityKind) {
	s.stage.hash = append(s.stage.hash, stagedHash{ns: v.Namespace, entry: radixhash.Entry{Hash: v.Hash, ImageID: id}})
	s.stageLiveAdd(entity, id)
}

func (s *Store) stageHashDelete(v HashValue, id common.ValueID) {
	s.stage.hash = append(s.stage.hash, stagedHash{ns: v.Namespace, entry: radixhash.Entry{Hash: v.Hash, ImageID: id}, delete: true})
}

func (s *Store) stageBitmap(v BitmapValue, id common.ValueID, entity common.EntityKind) {
	s.stage.bitmap = append(s.stage.bitmap, stagedBitmap{ns: v.Namespace, entry: bitmapidx.Entry{ImageID: id, Value: v.Value}})
	s.stageLiveAdd(entity, id)
}

func (s *Store) stageBitmapDelete(v BitmapValue, id common.ValueID) {
	s.stage.bitmap = append(s.stage.bitmap, stagedBitmap{ns: v.Namespace, entry: bitmapidx.Entry{ImageID: id, Value: v.Value}, delete: true})
}

func (s *Store) stageLiveAdd(entity common.EntityKind, id common.ValueID) {
	if id == 0 {
		return
	}
	s.stage.live = append(s.stage.live, stagedLive{add: true, entry: bitmapidx.Entry{ImageID: id, Value: livenessValue(entity)}})
}

func (s *Store) stageLiveRemove(entity common.EntityKind, id common.ValueID) {
	s.stage.live = append(s.stage.live, stagedLive{add: false, entry: bitmapidx.Entry{ImageID: id, Value: livenessValue(entity)}})
}

// ---- flush (one engine commit per affected namespace) ----

func (s *Store) flushStage() error {
	// Deletions clear liveness before any value change; additions set it last.
	if err := s.flushLiveness(false); err != nil {
		return err
	}
	if err := s.flushTime(); err != nil {
		return err
	}
	if err := s.flushGeo(); err != nil {
		return err
	}
	if err := s.flushSim(); err != nil {
		return err
	}
	if err := s.flushHash(); err != nil {
		return err
	}
	if err := s.flushBitmap(); err != nil {
		return err
	}
	return s.flushLiveness(true)
}

func sortStageByNS[T any](ops []T, ns func(T) common.Namespace) {
	slices.SortStableFunc(ops, func(a, b T) int { return cmp.Compare(ns(a), ns(b)) })
}

func (s *Store) flushTime() error {
	sortStageByNS(s.stage.time, func(o stagedTime) common.Namespace { return o.ns })
	for i := 0; i < len(s.stage.time); {
		j := i
		for j < len(s.stage.time) && s.stage.time[j].ns == s.stage.time[i].ns {
			j++
		}
		idx, _, err := s.timeFor(s.stage.time[i].ns)
		if err != nil {
			return err
		}
		batch := timeidx.NewBatch(j - i)
		for k := i; k < j; k++ {
			if !batch.Append(s.stage.time[k].m) {
				return timeidx.ErrBatchTooLarge
			}
		}
		if err := idx.Apply(batch); err != nil {
			return err
		}
		i = j
	}
	return nil
}

func (s *Store) flushGeo() error {
	sortStageByNS(s.stage.geo, func(o stagedGeo) common.Namespace { return o.ns })
	for i := 0; i < len(s.stage.geo); {
		j := i
		for j < len(s.stage.geo) && s.stage.geo[j].ns == s.stage.geo[i].ns {
			j++
		}
		idx, _, err := s.geoFor(s.stage.geo[i].ns)
		if err != nil {
			return err
		}
		var inserts, deletes []geoidx.Entry
		for k := i; k < j; k++ {
			if s.stage.geo[k].delete {
				deletes = append(deletes, s.stage.geo[k].entry)
			} else {
				inserts = append(inserts, s.stage.geo[k].entry)
			}
		}
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
	sortStageByNS(s.stage.sim, func(o stagedSim) common.Namespace { return o.ns })
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

// flushSimNamespace commits one namespace's staged records. simhash assigns
// dense 1-based IDs: records beyond the committed high-water mark are appended
// as a contiguous dense run (AppendBatch); records already committed are
// updated (UpsertBatch). Prevalidation guarantees the dense ordering.
func (s *Store) flushSimNamespace(ops []stagedSim) error {
	idx, _, err := s.simFor(ops[0].ns)
	if err != nil {
		return err
	}
	slices.SortStableFunc(ops, func(a, b stagedSim) int { return cmp.Compare(a.id, b.id) })
	n := idx.Len()
	// Collapse repeated ids, keeping the last write for each.
	unique := ops[:0]
	for _, op := range ops {
		if len(unique) > 0 && unique[len(unique)-1].id == op.id {
			unique[len(unique)-1] = op
			continue
		}
		unique = append(unique, op)
	}
	var appends []simhash.Record
	var updates []simhash.Update
	expected := n + 1
	for _, op := range unique {
		switch {
		case op.id <= n:
			updates = append(updates, simhash.Update{ImageID: op.id, Record: op.rec})
		case op.id == expected:
			appends = append(appends, op.rec)
			expected++
		default:
			return ErrNamespaceIDModel
		}
	}
	if len(appends) != 0 {
		if _, err := idx.AppendBatch(appends); err != nil {
			return err
		}
	}
	if len(updates) != 0 {
		if err := idx.UpsertBatch(updates); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) flushHash() error {
	sortStageByNS(s.stage.hash, func(o stagedHash) common.Namespace { return o.ns })
	for i := 0; i < len(s.stage.hash); {
		j := i
		for j < len(s.stage.hash) && s.stage.hash[j].ns == s.stage.hash[i].ns {
			j++
		}
		idx, _, err := s.radixFor(s.stage.hash[i].ns)
		if err != nil {
			return err
		}
		var adds, dels []radixhash.Entry
		for k := i; k < j; k++ {
			if s.stage.hash[k].delete {
				dels = append(dels, s.stage.hash[k].entry)
			} else {
				adds = append(adds, s.stage.hash[k].entry)
			}
		}
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
	sortStageByNS(s.stage.bitmap, func(o stagedBitmap) common.Namespace { return o.ns })
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
		return addErr
	}
	if err := applyBitmapBatch(live, batch); err != nil {
		return err
	}
	// Publish a new liveness version once the change is visible, so a matcher
	// that starts after this batch returns rebuilds its cached live bitmap.
	// A committed removal also disables the matcher's no-removal fast path.
	s.liveVersion.Add(1)
	if !add {
		s.liveRemovals.Store(true)
	}
	return nil
}

// vector staging applies immediately because vectoridx has no batch insertion
// API beyond one typed vector at a time.
func (s *Store) applyVectorValue(v VectorValue, id common.ValueID) error {
	idx, _, err := s.vectorFor(v.Namespace)
	if err != nil {
		return err
	}
	if id > math.MaxUint32 {
		return ErrValueIDTooLarge
	}
	return addVector(idx, vectoridx.NodeID(id), v)
}
