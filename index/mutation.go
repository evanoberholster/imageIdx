package index

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"sync/atomic"

	common "github.com/evanoberholster/imageIdx/common"
	"github.com/evanoberholster/imageIdx/index/bitmapidx"
	"github.com/evanoberholster/imageIdx/index/geoidx"
	"github.com/evanoberholster/imageIdx/index/radixhash"
	"github.com/evanoberholster/imageIdx/index/simhash"
	"github.com/evanoberholster/imageIdx/index/timeidx"
	"github.com/evanoberholster/imageIdx/index/vectoridx"
)

// DeleteOp removes one entity of a kind. Deletion is logical: liveness for the
// (entity, id) pair is cleared immediately, so no search can return it, even
// before physical records are reclaimed.
type DeleteOp struct {
	Entity common.EntityKind
	ID     common.ValueID
}

// Batch is a reusable, caller-owned group of mutations. Add/Update/Delete stage
// operations; Apply commits them through the same internal pipeline as the
// single-operation Store methods. A batch is not safe for concurrent use and
// cannot be mutated while it is being applied.
type Batch struct {
	entries  []Entry
	changes  []Change
	deletes  []DeleteOp
	capacity int
	inFlight atomic.Bool
}

// NewBatch returns an empty batch that grows to capacity without reallocating.
func (s *Store) NewBatch(capacity int) *Batch {
	if capacity < 0 {
		capacity = 0
	}
	return &Batch{capacity: capacity}
}

// Add stages an upsert of one entity's index associations.
func (b *Batch) Add(entry Entry) error {
	if b.inFlight.Load() {
		return ErrBatchClosed
	}
	b.entries = append(b.entries, entry)
	return nil
}

// Update stages a partial change reconciling Before and After.
func (b *Batch) Update(change Change) error {
	if b.inFlight.Load() {
		return ErrBatchClosed
	}
	if _, err := changeID(change); err != nil {
		return err
	}
	b.changes = append(b.changes, change)
	return nil
}

// Delete stages a logical deletion.
func (b *Batch) Delete(entity common.EntityKind, id common.ValueID) error {
	if b.inFlight.Load() {
		return ErrBatchClosed
	}
	if !entity.Valid() {
		return fmt.Errorf("%w: %d", ErrEntityMismatch, entity)
	}
	b.deletes = append(b.deletes, DeleteOp{Entity: entity, ID: id})
	return nil
}

// Len reports the number of staged operations.
func (b *Batch) Len() int {
	if b == nil {
		return 0
	}
	return len(b.entries) + len(b.changes) + len(b.deletes)
}

// Reset clears staged operations while retaining capacity.
func (b *Batch) Reset() error {
	if b.inFlight.Load() {
		return ErrBatchClosed
	}
	b.entries = b.entries[:0]
	b.changes = b.changes[:0]
	b.deletes = b.deletes[:0]
	return nil
}

// Add commits a single entity upsert.
func (s *Store) Add(entry Entry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed.Load() {
		return ErrClosed
	}
	return s.applyOne(entry, nil, nil)
}

// Update commits a single partial change.
func (s *Store) Update(change Change) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed.Load() {
		return ErrClosed
	}
	if _, err := changeID(change); err != nil {
		return err
	}
	return s.applyOne(Entry{}, &change, nil)
}

// Delete commits a single logical deletion.
func (s *Store) Delete(entity common.EntityKind, id common.ValueID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed.Load() {
		return ErrClosed
	}
	if !entity.Valid() {
		return fmt.Errorf("%w: %d", ErrEntityMismatch, entity)
	}
	return s.applyOne(Entry{}, nil, &DeleteOp{Entity: entity, ID: id})
}

// Apply commits every staged operation atomically with respect to other
// writers. It does not roll back engine mutations that a later failure cannot
// undo; see the package consistency contract.
func (s *Store) Apply(b *Batch) error {
	return s.applyBatch(b, 0, false)
}

// ApplyWithGeneration commits a batch and records a canonical-database
// completeness watermark. See the package consistency contract.
func (s *Store) ApplyWithGeneration(b *Batch, sourceGeneration uint64) error {
	return s.applyBatch(b, sourceGeneration, true)
}

func (s *Store) applyBatch(b *Batch, sourceGeneration uint64, explicit bool) error {
	if b == nil {
		return ErrEmptyBatch
	}
	if !b.inFlight.CompareAndSwap(false, true) {
		return ErrBatchClosed
	}
	defer b.inFlight.Store(false)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed.Load() {
		return ErrClosed
	}
	if explicit {
		if sourceGeneration < s.source {
			return fmt.Errorf("%w: %d < %d", timeidx.ErrSourceGeneration, sourceGeneration, s.source)
		}
	} else {
		sourceGeneration = s.source
	}
	if len(b.entries)+len(b.changes)+len(b.deletes) == 0 {
		// An empty batch is a no-op unless it advances the completeness
		// watermark, which is a supported way to mark a bulk import complete.
		if !explicit || sourceGeneration == s.source {
			return ErrEmptyBatch
		}
		s.source = sourceGeneration
		s.gen++
		return s.writeManifest()
	}
	if err := s.applyLocked(b.entries, b.changes, b.deletes); err != nil {
		return err
	}
	changed := sourceGeneration != s.source
	s.source = sourceGeneration
	s.gen++
	if explicit && changed {
		if err := s.writeManifest(); err != nil {
			return err
		}
	}
	return nil
}

// applyOne runs a single staged operation under the caller-held write lock.
func (s *Store) applyOne(entry Entry, change *Change, del *DeleteOp) error {
	if err := s.applyLocked(entrySlice(entry), changeSlice(change), deleteSlice(del)); err != nil {
		return err
	}
	s.gen++
	return nil
}

// The helpers below let single operations reuse the batch pipeline without
// allocating: a nil pointer yields a nil slice, and a present pointer yields a
// one-element slice over caller stack storage.

func entrySlice(e Entry) []Entry {
	if e.ValueID == 0 && len(e.Times) == 0 && len(e.Geos) == 0 && len(e.Sims) == 0 && len(e.Hashes) == 0 && len(e.Vectors) == 0 && len(e.Bitmaps) == 0 {
		return nil
	}
	return []Entry{e}
}

func changeSlice(c *Change) []Change {
	if c == nil {
		return nil
	}
	return []Change{*c}
}

func deleteSlice(d *DeleteOp) []DeleteOp {
	if d == nil {
		return nil
	}
	return []DeleteOp{*d}
}

// applyLocked applies deletes first (clearing liveness), then partial changes,
// then additions, so a later addition of the same entity wins.
func (s *Store) applyLocked(entries []Entry, changes []Change, deletes []DeleteOp) error {
	if len(entries)+len(changes)+len(deletes) == 0 {
		return ErrEmptyBatch
	}
	for i := range deletes {
		if err := s.applyDelete(deletes[i]); err != nil {
			return err
		}
	}
	for i := range changes {
		if err := s.applyChange(changes[i]); err != nil {
			return err
		}
	}
	for i := range entries {
		if err := s.applyEntry(entries[i]); err != nil {
			return err
		}
	}
	return nil
}

func changeID(c Change) (common.ValueID, error) {
	b, a := c.Before.ValueID, c.After.ValueID
	if b != 0 && a != 0 && b != a {
		return 0, fmt.Errorf("%w: before=%d after=%d", ErrEntityMismatch, b, a)
	}
	if a != 0 {
		return a, nil
	}
	return b, nil
}

// applyEntry upserts every value, then marks each involved entity live.
func (s *Store) applyEntry(e Entry) error {
	var touched entitySet
	if len(e.Times) != 0 {
		if err := s.addTimes(e.ValueID, e.Times, &touched); err != nil {
			return err
		}
	}
	if len(e.Geos) != 0 {
		if err := s.addGeos(e.ValueID, e.Geos, &touched); err != nil {
			return err
		}
	}
	if len(e.Sims) != 0 {
		if err := s.addSims(e.ValueID, e.Sims, &touched); err != nil {
			return err
		}
	}
	if len(e.Hashes) != 0 {
		if err := s.addHashes(e.ValueID, e.Hashes, &touched); err != nil {
			return err
		}
	}
	if len(e.Vectors) != 0 {
		if err := s.addVectors(e.ValueID, e.Vectors, &touched); err != nil {
			return err
		}
	}
	if len(e.Bitmaps) != 0 {
		if err := s.addBitmaps(e.ValueID, e.Bitmaps, &touched); err != nil {
			return err
		}
	}
	return s.setLiveness(e.ValueID, touched)
}

// applyChange reconciles Before and After by set difference. Single-valued
// families only remove a namespace absent from After; multi-valued families
// diff individual values.
func (s *Store) applyChange(c Change) error {
	id, err := changeID(c)
	if err != nil {
		return err
	}
	var touched entitySet
	// Single-valued families: set After, then drop namespaces absent from After.
	if err := s.addTimes(id, c.After.Times, &touched); err != nil {
		return err
	}
	for _, bv := range c.Before.Times {
		if !hasTimeNS(c.After.Times, bv.Namespace) {
			if err := s.deleteTime(bv.Namespace, id); err != nil {
				return err
			}
		}
	}
	if err := s.addGeos(id, c.After.Geos, &touched); err != nil {
		return err
	}
	for _, bv := range c.Before.Geos {
		if !hasGeoNS(c.After.Geos, bv.Namespace) {
			if err := s.deleteGeo(bv.Namespace, bv.Key, id); err != nil {
				return err
			}
		}
	}
	if err := s.addSims(id, c.After.Sims, &touched); err != nil {
		return err
	}
	if err := s.addVectors(id, c.After.Vectors, &touched); err != nil {
		return err
	}
	for _, bv := range c.Before.Vectors {
		if !hasVectorNS(c.After.Vectors, bv.Namespace) {
			if err := s.deleteVector(bv.Namespace, id); err != nil {
				return err
			}
		}
	}
	// Multi-valued families: diff individual values.
	for _, av := range c.After.Hashes {
		if err := s.addHash(av, id, &touched); err != nil {
			return err
		}
	}
	for _, bv := range c.Before.Bitmaps {
		if !hasBitmap(c.After.Bitmaps, bv) {
			if err := s.removeBitmap(bv, id, &touched); err != nil {
				return err
			}
		}
	}
	if err := s.addBitmaps(id, c.After.Bitmaps, &touched); err != nil {
		return err
	}
	return s.setLiveness(id, touched)
}

// applyDelete clears liveness first, then removes ID-authoritative records
// where the engine supports it.
func (s *Store) applyDelete(d DeleteOp) error {
	if err := s.clearLiveness(d.Entity, d.ID); err != nil {
		return err
	}
	// Timestamp and vector engines can delete by ID without the old value.
	for ns, rt := range s.reg.routes {
		if rt.slot == noSlot || rt.entity != d.Entity {
			continue
		}
		switch familyOf(rt.kind) {
		case familyTime:
			if err := s.deleteTime(common.Namespace(ns), d.ID); err != nil {
				return err
			}
		case familyVector:
			if err := s.deleteVector(common.Namespace(ns), d.ID); err != nil {
				return err
			}
		}
	}
	return nil
}

// entitySet is a bitset over entity kinds, avoiding a map for touched tracking.
type entitySet uint32

func (e *entitySet) add(k common.EntityKind) { *e |= 1 << uint(k) }

// setLiveness marks the entity live for every touched entity kind.
func (s *Store) setLiveness(id common.ValueID, touched entitySet) error {
	if id == 0 || touched == 0 {
		return nil
	}
	for k := common.EntityImage; k.Valid(); k++ {
		if touched&(1<<uint(k)) == 0 {
			continue
		}
		if err := s.livenessAdd(k, id); err != nil {
			return err
		}
	}
	return nil
}

// ---- per-family add ----

func (s *Store) addTimes(id common.ValueID, values []TimeValue, touched *entitySet) error {
	for _, v := range values {
		idx, rt, err := s.timeFor(v.Namespace)
		if err != nil {
			return err
		}
		if err := applyTimeMutation(idx, timeidx.Insert(id, v.Timestamp)); err != nil {
			return err
		}
		touched.add(rt.entity)
	}
	return nil
}

func (s *Store) addGeos(id common.ValueID, values []GeoValue, touched *entitySet) error {
	for _, v := range values {
		idx, rt, err := s.geoFor(v.Namespace)
		if err != nil {
			return err
		}
		if err := idx.Insert(v.Key, id); err != nil {
			return err
		}
		touched.add(rt.entity)
	}
	return nil
}

func (s *Store) addSims(id common.ValueID, values []SimValue, touched *entitySet) error {
	for _, v := range values {
		if err := s.addSim(v, id, touched); err != nil {
			return err
		}
	}
	return nil
}

// addSim writes one perceptual record. simhash assigns dense, 1-based record
// IDs, so an existing id is updated and a new id must be exactly the next
// dense ValueID; anything else is rejected rather than silently remapped.
func (s *Store) addSim(v SimValue, id common.ValueID, touched *entitySet) error {
	st, rt, err := s.simFor(v.Namespace)
	if err != nil {
		return err
	}
	n := st.Len()
	switch {
	case id != 0 && id <= n:
		if err := st.Upsert(id, v.Record); err != nil {
			return err
		}
	case id == n+1:
		assigned, err := st.Append(v.Record)
		if err != nil {
			return err
		}
		if assigned != id {
			return fmt.Errorf("%w: simhash assigned %d, want %d", ErrNamespaceIDModel, assigned, id)
		}
	default:
		return fmt.Errorf("%w: simhash id %d, next dense id %d", ErrNamespaceIDModel, id, n+1)
	}
	touched.add(rt.entity)
	return nil
}

func (s *Store) addHashes(id common.ValueID, values []HashValue, touched *entitySet) error {
	for _, v := range values {
		if err := s.addHash(v, id, touched); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) addHash(v HashValue, id common.ValueID, touched *entitySet) error {
	idx, rt, err := s.radixFor(v.Namespace)
	if err != nil {
		return err
	}
	if err := idx.Apply([]radixhash.Entry{{Hash: v.Hash, ImageID: id}}); err != nil {
		return err
	}
	touched.add(rt.entity)
	return nil
}

func (s *Store) addVectors(id common.ValueID, values []VectorValue, touched *entitySet) error {
	for _, v := range values {
		idx, rt, err := s.vectorFor(v.Namespace)
		if err != nil {
			return err
		}
		if id > math.MaxUint32 {
			return fmt.Errorf("%w: %d", ErrValueIDTooLarge, id)
		}
		node := vectoridx.NodeID(id)
		if err := addVector(idx, node, v); err != nil {
			return err
		}
		touched.add(rt.entity)
	}
	return nil
}

func addVector(idx *vectoridx.VectorIdx, node vectoridx.NodeID, v VectorValue) error {
	switch v.Element {
	case vectoridx.ElementFloat32:
		return idx.AddFloat32(node, v.Float32)
	case vectoridx.ElementInt8:
		return idx.AddInt8(node, v.Int8)
	case vectoridx.ElementUint8:
		return idx.AddUint8(node, v.Uint8)
	case vectoridx.ElementInt16:
		return idx.AddInt16(node, v.Int16)
	case vectoridx.ElementUint16:
		return idx.AddUint16(node, v.Uint16)
	default:
		return fmt.Errorf("%w: vector element type %d", ErrWrongEngine, v.Element)
	}
}

func (s *Store) addBitmaps(id common.ValueID, values []BitmapValue, touched *entitySet) error {
	for _, v := range values {
		idx, rt, err := s.bitmapFor(v.Namespace)
		if err != nil {
			return err
		}
		batch := idx.NewBatch()
		if err := batch.Add(bitmapidx.Entry{ImageID: id, Value: v.Value}); err != nil {
			_ = batch.Close()
			return err
		}
		if err := applyBitmapBatch(idx, batch); err != nil {
			return err
		}
		touched.add(rt.entity)
	}
	return nil
}

func (s *Store) removeBitmap(v BitmapValue, id common.ValueID, touched *entitySet) error {
	idx, rt, err := s.bitmapFor(v.Namespace)
	if err != nil {
		return err
	}
	batch := idx.NewBatch()
	if err := batch.Remove(bitmapidx.Entry{ImageID: id, Value: v.Value}); err != nil {
		_ = batch.Close()
		return err
	}
	if err := applyBitmapBatch(idx, batch); err != nil {
		return err
	}
	touched.add(rt.entity)
	return nil
}

// applyBitmapBatch commits a bitmap batch and waits for it to become visible.
// bitmapidx Apply only enqueues; Barrier makes the change observable to the
// next read and is required for correct delete/update semantics.
func applyBitmapBatch(idx *bitmapidx.Index, batch *bitmapidx.Batch) error {
	err := batch.Apply()
	if err == nil {
		err = idx.Barrier()
	}
	return errors.Join(err, batch.Close())
}

// ---- per-family remove ----

func (s *Store) deleteTime(ns common.Namespace, id common.ValueID) error {
	idx, _, err := s.timeFor(ns)
	if err != nil {
		return err
	}
	return applyTimeMutation(idx, timeidx.Delete(id, 0))
}

func (s *Store) deleteGeo(ns common.Namespace, key geoidx.S2Key, id common.ValueID) error {
	idx, _, err := s.geoFor(ns)
	if err != nil {
		return err
	}
	return idx.Delete(key, id)
}

func (s *Store) deleteVector(ns common.Namespace, id common.ValueID) error {
	idx, _, err := s.vectorFor(ns)
	if err != nil {
		return err
	}
	if id > math.MaxUint32 {
		return fmt.Errorf("%w: %d", ErrValueIDTooLarge, id)
	}
	if err := idx.Delete(vectoridx.NodeID(id)); err != nil && err != vectoridx.ErrNodeNotFound {
		return err
	}
	return nil
}

// ---- liveness ----

func (s *Store) livenessAdd(entity common.EntityKind, id common.ValueID) error {
	idx, value, ok := s.reg.liveness(entity)
	if !ok {
		return fmt.Errorf("%w: entity %d", ErrEntityMismatch, entity)
	}
	return applyBitmapMutation(idx, true, id, value)
}

func (s *Store) clearLiveness(entity common.EntityKind, id common.ValueID) error {
	idx, value, ok := s.reg.liveness(entity)
	if !ok {
		return fmt.Errorf("%w: entity %d", ErrEntityMismatch, entity)
	}
	return applyBitmapMutation(idx, false, id, value)
}

func applyBitmapMutation(idx *bitmapidx.Index, add bool, id common.ValueID, value bitmapidx.Value) error {
	batch := idx.NewBatch()
	var err error
	if add {
		err = batch.Add(bitmapidx.Entry{ImageID: id, Value: value})
	} else {
		err = batch.Remove(bitmapidx.Entry{ImageID: id, Value: value})
	}
	if err != nil {
		_ = batch.Close()
		return err
	}
	return applyBitmapBatch(idx, batch)
}

// applyTimeMutation commits one timeidx mutation through a fresh batch.
func applyTimeMutation(idx *timeidx.Index, m timeidx.Mutation) error {
	batch := timeidx.NewBatch(1)
	if !batch.Append(m) {
		return timeidx.ErrBatchTooLarge
	}
	return idx.Apply(batch)
}

// ---- routing helpers ----

func (s *Store) timeFor(ns common.Namespace) (*timeidx.Index, route, error) {
	rt, ok := s.reg.lookup(ns)
	if !ok {
		return nil, route{}, fmt.Errorf("%w: %d", ErrNamespaceNotFound, ns)
	}
	idx, ok := s.reg.timeIndex(ns)
	if !ok {
		return nil, rt, fmt.Errorf("%w: %d", ErrNamespaceUnavailable, ns)
	}
	return idx, rt, nil
}

func (s *Store) geoFor(ns common.Namespace) (*geoidx.Index, route, error) {
	rt, ok := s.reg.lookup(ns)
	if !ok {
		return nil, route{}, fmt.Errorf("%w: %d", ErrNamespaceNotFound, ns)
	}
	idx, ok := s.reg.geoIndex(ns)
	if !ok {
		return nil, rt, fmt.Errorf("%w: %d", ErrNamespaceUnavailable, ns)
	}
	return idx, rt, nil
}

func (s *Store) simFor(ns common.Namespace) (*simhash.Store, route, error) {
	rt, ok := s.reg.lookup(ns)
	if !ok {
		return nil, route{}, fmt.Errorf("%w: %d", ErrNamespaceNotFound, ns)
	}
	st, ok := s.reg.simStore(ns)
	if !ok {
		return nil, rt, fmt.Errorf("%w: %d", ErrNamespaceUnavailable, ns)
	}
	return st, rt, nil
}

func (s *Store) radixFor(ns common.Namespace) (*radixhash.Index, route, error) {
	rt, ok := s.reg.lookup(ns)
	if !ok {
		return nil, route{}, fmt.Errorf("%w: %d", ErrNamespaceNotFound, ns)
	}
	idx, ok := s.reg.radixIndex(ns)
	if !ok {
		return nil, rt, fmt.Errorf("%w: %d", ErrNamespaceUnavailable, ns)
	}
	return idx, rt, nil
}

func (s *Store) vectorFor(ns common.Namespace) (*vectoridx.VectorIdx, route, error) {
	rt, ok := s.reg.lookup(ns)
	if !ok {
		return nil, route{}, fmt.Errorf("%w: %d", ErrNamespaceNotFound, ns)
	}
	idx, ok := s.reg.vectorIndex(ns)
	if !ok {
		return nil, rt, fmt.Errorf("%w: %d", ErrNamespaceUnavailable, ns)
	}
	return idx, rt, nil
}

func (s *Store) bitmapFor(ns common.Namespace) (*bitmapidx.Index, route, error) {
	rt, ok := s.reg.lookup(ns)
	if !ok {
		return nil, route{}, fmt.Errorf("%w: %d", ErrNamespaceNotFound, ns)
	}
	idx, ok := s.reg.bitmapIndex(ns)
	if !ok {
		return nil, rt, fmt.Errorf("%w: %d", ErrNamespaceUnavailable, ns)
	}
	return idx, rt, nil
}

// ---- value comparisons for set difference ----

func hasTimeNS(values []TimeValue, ns common.Namespace) bool {
	for _, v := range values {
		if v.Namespace == ns {
			return true
		}
	}
	return false
}

func hasGeoNS(values []GeoValue, ns common.Namespace) bool {
	for _, v := range values {
		if v.Namespace == ns {
			return true
		}
	}
	return false
}

func hasVectorNS(values []VectorValue, ns common.Namespace) bool {
	for _, v := range values {
		if v.Namespace == ns {
			return true
		}
	}
	return false
}

func hasBitmap(values []BitmapValue, want BitmapValue) bool {
	for _, v := range values {
		if v.Namespace == want.Namespace && v.Value.Kind == want.Value.Kind &&
			v.Value.Int == want.Value.Int && v.Value.Bool == want.Value.Bool &&
			bytes.Equal(v.Value.Bytes, want.Value.Bytes) {
			return true
		}
	}
	return false
}
