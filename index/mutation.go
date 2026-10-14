package index

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"sync/atomic"

	schema "github.com/evanoberholster/imageIdx/schema"
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
	Entity schema.EntityKind
	ID     schema.ValueID
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
func (b *Batch) Delete(entity schema.EntityKind, id schema.ValueID) error {
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
func (s *Store) Delete(entity schema.EntityKind, id schema.ValueID) error {
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
	return s.applyBatch(b, 0, false, false)
}

// ApplyWithGeneration commits a batch and records a canonical-database
// completeness watermark. See the package consistency contract.
func (s *Store) ApplyWithGeneration(b *Batch, sourceGeneration uint64) error {
	return s.applyBatch(b, sourceGeneration, true, false)
}

// ApplyWithGenerationAll commits a batch and confirms that every healthy
// declared namespace (including unchanged namespaces) is complete through
// sourceGeneration. This is for a coordinator that owns the full canonical
// mutation stream. It avoids a second empty generation-only batch and does
// not rewrite unchanged index values. Unavailable namespaces still hold the
// store-wide safe generation back.
func (s *Store) ApplyWithGenerationAll(b *Batch, sourceGeneration uint64) error {
	return s.applyBatch(b, sourceGeneration, true, true)
}

func (s *Store) applyBatch(b *Batch, sourceGeneration uint64, explicit, all bool) error {
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
		// It claims completeness through sourceGeneration for every healthy
		// namespace.
		if !explicit || (sourceGeneration == s.source && s.recomputeSafe() >= sourceGeneration) {
			return ErrEmptyBatch
		}
		if err := s.advanceAllCompleteness(sourceGeneration); err != nil {
			return err
		}
		s.source = sourceGeneration
		s.gen++
		s.safeSource.Store(s.recomputeSafe())
		return s.writeManifest()
	}
	if err := s.prevalidate(b.entries, b.changes, b.deletes); err != nil {
		return err
	}
	// If this batch could make some engines durable and others not, record the
	// intended namespaces durably before the first engine can persist.
	risky := s.batchMayPartiallyPersist()
	if risky {
		if err := s.pendingAddSet(); err != nil {
			return err
		}
	}
	s.flushSourceGen = sourceGeneration
	s.flushExplicit = explicit
	applyErr := s.applyLocked(b.entries, b.changes, b.deletes)
	s.flushExplicit = false
	s.flushSourceGen = 0
	if applyErr != nil {
		s.degradeIntended(applyErr)
		return applyErr
	}
	s.source = sourceGeneration
	s.gen++
	if explicit {
		advanced, err := s.advanceTouchedCompleteness(sourceGeneration)
		if err != nil {
			s.degradeIntended(err)
			return err
		}
		if all {
			untouched, err := s.advanceUntouchedCompleteness(sourceGeneration)
			if err != nil {
				s.degradeIntended(err)
				return err
			}
			advanced = advanced || untouched
		}
		if risky {
			if err := s.pendingRemoveSet(); err != nil {
				return err
			}
		}
		if advanced {
			s.safeSource.Store(s.recomputeSafe())
			if err := s.writeManifest(); err != nil {
				return err
			}
		}
	} else if risky {
		// Make the checkpoint-dependent engines durable, then clear the marker,
		// so a successful batch reopens clean.
		if err := s.syncTouchedForCompleteness(); err != nil {
			s.degradeIntended(err)
			return err
		}
		if err := s.pendingRemoveSet(); err != nil {
			return err
		}
	}
	return nil
}

// crashDurableDuringApply reports whether an engine's Apply makes its data
// recoverable without an explicit Checkpoint, under the process-crash (not
// power-loss) contract the engines document. Verified by
// TestEnginePersistenceBoundaries: timeidx synchronizes its COW generation,
// bitmapidx replays its WAL tail, and vectoridx replays its WAL on reopen.
// radixhash and geoidx need an explicit Checkpoint; simhash new-record appends
// need the manifest write, so they are checkpoint-dependent. A simhash *update*
// is journaled and does survive (see pvSimUpdate).
func crashDurableDuringApply(kind schema.NamespaceKind) bool {
	return kind == schema.KindTimestamp || kind == schema.KindVector || kind.IsBitmap()
}

// batchMayPartiallyPersist reports whether a crash mid-batch could leave some
// commit units durable and others not. A single engine batch is atomic, so only
// batches spanning two or more units (distinct user namespaces, or multiple
// non-atomic vector operations in one namespace) with at least one crash-durable
// unit qualify. Liveness lives in the crash-durable bitmap store and shares the
// entity's rebuild, so it is not counted as a separate unit. If every unit is
// checkpoint-dependent, none survives a crash and the state stays consistent.
func (s *Store) batchMayPartiallyPersist() bool {
	units := 0
	anyDurable := false
	for w, word := range s.pvSet {
		for bit := 0; bit < 64; bit++ {
			if word&(1<<uint(bit)) == 0 {
				continue
			}
			units++
			if def, ok := s.schema.Lookup(schema.Namespace(w*64 + bit)); ok && crashDurableDuringApply(def.Kind) {
				anyDurable = true
			}
		}
	}
	// vectoridx applies one operation at a time, so two or more vector
	// operations in one namespace are not a single atomic unit.
	if s.pvVectorOps >= 2 {
		units += s.pvVectorOps - 1
		anyDurable = true
	}
	// A simhash update is journaled and survives a crash even though a new
	// append does not; if this batch updates existing simhash records the
	// simhash unit is durable.
	if s.pvSimUpdate {
		anyDurable = true
	}
	return units >= 2 && anyDurable
}

// pendingAddSet unions the current intended set into the durable recovery
// marker. The caller holds mu.
func (s *Store) pendingAddSet() error {
	if len(s.pendingSet) < len(s.pvSet) {
		s.pendingSet = append(s.pendingSet, make([]uint64, len(s.pvSet)-len(s.pendingSet))...)
	}
	changed := false
	for w, word := range s.pvSet {
		if word == 0 {
			continue
		}
		if s.pendingSet[w]&word != word {
			changed = true
		}
		s.pendingSet[w] |= word
	}
	if !changed {
		return nil
	}
	return s.persistRecovery()
}

// pendingRemoveSet clears the current intended set from the durable recovery
// marker. The caller holds mu.
func (s *Store) pendingRemoveSet() error {
	changed := false
	for w, word := range s.pvSet {
		if w >= len(s.pendingSet) {
			break
		}
		if s.pendingSet[w]&word != 0 {
			changed = true
		}
		s.pendingSet[w] &^= word
	}
	if !changed {
		return nil
	}
	return s.persistRecovery()
}

// advanceTouchedCompleteness establishes a durability barrier for every
// touched namespace, then records that they are complete through gen. The
// engine sync happens before the unified manifest records gen, so a manifest
// claim of completeness can never outrun durable engine data.
func (s *Store) advanceTouchedCompleteness(gen uint64) (bool, error) {
	if err := s.syncTouchedForCompleteness(); err != nil {
		return false, err
	}
	advanced := false
	for w, word := range s.pvSet {
		for bit := 0; bit < 64; bit++ {
			if word&(1<<uint(bit)) == 0 {
				continue
			}
			ns := schema.Namespace(w*64 + bit)
			if gen > s.nsSource[ns] {
				s.nsSource[ns] = gen
				advanced = true
			}
			if def, ok := s.schema.Lookup(ns); ok && def.Kind == schema.KindTimestamp {
				if idx, ok := s.reg.timeIndex(ns); ok {
					if err := applyTimeGeneration(idx, gen); err != nil {
						return advanced, err
					}
				}
			}
		}
	}
	return advanced, nil
}

// advanceUntouchedCompleteness promotes only healthy namespaces not written by
// this batch. Prior index writes are already durable at their last confirmed
// generation; there is no new engine payload to checkpoint here.
func (s *Store) advanceUntouchedCompleteness(gen uint64) (bool, error) {
	advanced := false
	for _, def := range s.defs {
		ns := def.Namespace
		if !s.health[ns].Available || s.isDegraded(ns) || (s.pvSet[ns>>6]&(uint64(1)<<(uint(ns)&63))) != 0 {
			continue
		}
		if def.Kind == schema.KindTimestamp {
			if idx, ok := s.reg.timeIndex(ns); ok {
				if err := applyTimeGeneration(idx, gen); err != nil {
					return advanced, err
				}
			}
		}
		if gen > s.nsSource[ns] {
			s.nsSource[ns] = gen
			advanced = true
		}
	}
	return advanced, nil
}

// advanceAllCompleteness establishes a durability barrier for every healthy
// namespace, then records that all are complete through gen. It is used by an
// empty generation-only batch.
func (s *Store) advanceAllCompleteness(gen uint64) error {
	if err := s.syncAllForCompleteness(); err != nil {
		return err
	}
	for _, def := range s.defs {
		if !s.health[def.Namespace].Available || s.isDegraded(def.Namespace) {
			continue
		}
		if gen > s.nsSource[def.Namespace] {
			s.nsSource[def.Namespace] = gen
		}
		if def.Kind == schema.KindTimestamp {
			if idx, ok := s.reg.timeIndex(def.Namespace); ok {
				if err := applyTimeGeneration(idx, gen); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// syncNamespace makes one healthy namespace durable. Only the
// checkpoint-dependent engines (radixhash, geoidx, simhash) need work; the others'
// Apply already left crash-recoverable state, so re-checkpointing them would be
// redundant write amplification.
func (s *Store) syncNamespace(ns schema.Namespace) error {
	def, ok := s.schema.Lookup(ns)
	if !ok || !s.health[ns].Available || s.isDegraded(ns) {
		return nil
	}
	switch familyOf(def.Kind) {
	case familyGeo:
		if idx, ok := s.reg.geoIndex(ns); ok {
			return idx.Checkpoint()
		}
	case familyRadix:
		if idx, ok := s.reg.radixIndex(ns); ok {
			return idx.Checkpoint()
		}
	case familySim:
		if idx, ok := s.reg.simStore(ns); ok {
			return idx.Checkpoint()
		}
	}
	return nil
}

func (s *Store) syncTouchedForCompleteness() error {
	for w, word := range s.pvSet {
		for bit := 0; bit < 64; bit++ {
			if word&(1<<uint(bit)) == 0 {
				continue
			}
			if err := s.syncNamespace(schema.Namespace(w*64 + bit)); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Store) syncAllForCompleteness() error {
	for _, def := range s.defs {
		if err := s.syncNamespace(def.Namespace); err != nil {
			return err
		}
	}
	return nil
}

// applyTimeGeneration persists a completeness watermark into a timeidx
// namespace through its empty-batch generation path.
func applyTimeGeneration(idx *timeidx.Index, gen uint64) error {
	batch := timeidx.NewBatch(0)
	return idx.ApplyWithGeneration(batch, gen)
}

// applyOne runs a single staged operation under the caller-held write lock.
func (s *Store) applyOne(entry Entry, change *Change, del *DeleteOp) error {
	entries, changes, deletes := entrySlice(entry), changeSlice(change), deleteSlice(del)
	if err := s.prevalidate(entries, changes, deletes); err != nil {
		return err
	}
	risky := s.batchMayPartiallyPersist()
	if risky {
		if err := s.pendingAddSet(); err != nil {
			return err
		}
	}
	if err := s.applyLocked(entries, changes, deletes); err != nil {
		s.degradeIntended(err)
		return err
	}
	if risky {
		if err := s.syncTouchedForCompleteness(); err != nil {
			s.degradeIntended(err)
			return err
		}
		if err := s.pendingRemoveSet(); err != nil {
			return err
		}
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

// applyLocked stages the whole batch into per-engine groups and then flushes
// one commit per affected namespace. Liveness is cleared before value changes
// and set after them, so a later addition of the same entity wins and a partial
// batch never exposes an incomplete entity as live.
func (s *Store) applyLocked(entries []Entry, changes []Change, deletes []DeleteOp) error {
	if len(entries)+len(changes)+len(deletes) == 0 {
		return ErrEmptyBatch
	}
	s.resetStage()
	for i := range deletes {
		if err := s.stageDelete(deletes[i]); err != nil {
			return err
		}
	}
	for i := range changes {
		if err := s.stageChange(changes[i]); err != nil {
			return err
		}
	}
	for i := range entries {
		if err := s.stageEntry(entries[i]); err != nil {
			return err
		}
	}
	return s.flushStage()
}

func (s *Store) routeEntity(ns schema.Namespace) schema.EntityKind {
	rt, _ := s.reg.lookup(ns)
	return rt.entity
}

func (s *Store) stageEntry(e Entry) error {
	for _, v := range e.Times {
		s.stageTime(v, e.ValueID, s.routeEntity(v.Namespace))
	}
	for _, v := range e.Geos {
		s.stageGeo(v, e.ValueID, s.routeEntity(v.Namespace))
	}
	for _, v := range e.Sims {
		s.stageSim(v, e.ValueID, s.routeEntity(v.Namespace))
	}
	for _, v := range e.Hashes {
		s.stageHash(v, e.ValueID, s.routeEntity(v.Namespace))
	}
	for _, v := range e.Vectors {
		// vectoridx Add rejects a live duplicate node id, so an upsert deletes
		// the existing node before re-adding it. Deleting an absent node is a
		// no-op.
		s.stageVectorDelete(v.Namespace, e.ValueID)
		s.stageVector(v, e.ValueID, s.routeEntity(v.Namespace))
	}
	for _, v := range e.Bitmaps {
		s.stageBitmap(v, e.ValueID, s.routeEntity(v.Namespace))
	}
	return nil
}

func (s *Store) stageChange(c Change) error {
	id, err := changeID(c)
	if err != nil {
		return err
	}
	for _, v := range c.After.Times {
		s.stageTime(v, id, s.routeEntity(v.Namespace))
	}
	for _, bv := range c.Before.Times {
		if !hasTimeNS(c.After.Times, bv.Namespace) {
			s.stageTimeDelete(bv.Namespace, id)
		}
	}
	for _, v := range c.After.Geos {
		s.stageGeo(v, id, s.routeEntity(v.Namespace))
	}
	for _, bv := range c.Before.Geos {
		if !hasGeoNS(c.After.Geos, bv.Namespace) {
			s.stageGeoDelete(bv.Namespace, bv.Key, id)
		}
	}
	for _, v := range c.After.Sims {
		s.stageSim(v, id, s.routeEntity(v.Namespace))
	}
	for _, v := range c.After.Hashes {
		s.stageHash(v, id, s.routeEntity(v.Namespace))
	}
	for _, bv := range c.Before.Hashes {
		if !hasHash(c.After.Hashes, bv) {
			s.stageHashDelete(bv, id)
		}
	}
	// Vector replacement is delete-then-add: always stage the old node's
	// deletion first (a no-op if absent), then any new node.
	for _, bv := range c.Before.Vectors {
		s.stageVectorDelete(bv.Namespace, id)
	}
	for _, v := range c.After.Vectors {
		s.stageVector(v, id, s.routeEntity(v.Namespace))
	}
	for _, bv := range c.Before.Bitmaps {
		if !hasBitmap(c.After.Bitmaps, bv) {
			s.stageBitmapDelete(bv, id)
		}
	}
	for _, v := range c.After.Bitmaps {
		s.stageBitmap(v, id, s.routeEntity(v.Namespace))
	}
	return nil
}

func (s *Store) stageDelete(d DeleteOp) error {
	s.stageLiveRemove(d.Entity, d.ID)
	for ns, rt := range s.reg.routes {
		if rt.slot == noSlot || rt.entity != d.Entity {
			continue
		}
		switch familyOf(rt.kind) {
		case familyTime:
			s.stageTimeDelete(schema.Namespace(ns), d.ID)
		case familyVector:
			s.stageVectorDelete(schema.Namespace(ns), d.ID)
		}
	}
	return nil
}

// prevalidate checks the complete batch before any engine is mutated, and
// records every namespace the batch intends to modify. It rejects routing,
// entity, ValueID, vector-dimension and identifier-model errors so a malformed
// batch cannot partially apply. It has no engine side effects.
func (s *Store) prevalidate(entries []Entry, changes []Change, deletes []DeleteOp) error {
	words := (len(s.reg.routes) + 63) / 64
	if cap(s.pvSet) < words {
		s.pvSet = make([]uint64, words)
	}
	s.pvSet = s.pvSet[:words]
	clear(s.pvSet)
	s.pvCursors = s.pvCursors[:0]
	s.pvVectorOps = 0
	s.pvSimUpdate = false

	for i := range deletes {
		d := deletes[i]
		if !d.Entity.Valid() {
			return fmt.Errorf("%w: %d", ErrEntityMismatch, d.Entity)
		}
		if d.ID == 0 {
			return fmt.Errorf("%w: entity id 0", ErrEntityMismatch)
		}
		if _, _, ok := s.reg.liveness(d.Entity); !ok {
			return fmt.Errorf("%w: entity %d", ErrNamespaceNotFound, d.Entity)
		}
		// A delete also touches the entity's ID-authoritative time and vector
		// namespaces, so a failed delete degrades them too.
		for ns, rt := range s.reg.routes {
			if rt.slot == noSlot || rt.entity != d.Entity {
				continue
			}
			if familyOf(rt.kind) == familyTime || familyOf(rt.kind) == familyVector {
				s.pvMark(schema.Namespace(ns))
			}
		}
	}
	for i := range entries {
		if err := s.prevalidateEntry(entries[i]); err != nil {
			return err
		}
	}
	for i := range changes {
		if _, err := changeID(changes[i]); err != nil {
			return err
		}
		if err := s.prevalidateEntry(changes[i].Before); err != nil {
			return err
		}
		if err := s.prevalidateEntry(changes[i].After); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) prevalidateEntry(e Entry) error {
	values := len(e.Times) + len(e.Geos) + len(e.Sims) + len(e.Hashes) + len(e.Vectors) + len(e.Bitmaps)
	if values == 0 {
		return nil
	}
	if e.ValueID == 0 {
		return fmt.Errorf("%w: entity id 0", ErrEntityMismatch)
	}
	for _, v := range e.Times {
		if err := s.pvKind(v.Namespace, schema.KindTimestamp); err != nil {
			return err
		}
	}
	for _, v := range e.Geos {
		if err := s.pvKind(v.Namespace, schema.KindGeo); err != nil {
			return err
		}
	}
	for _, v := range e.Sims {
		if err := s.pvSim(v.Namespace, e.ValueID); err != nil {
			return err
		}
	}
	for _, v := range e.Hashes {
		if err := s.pvKind(v.Namespace, schema.KindHashExact); err != nil {
			return err
		}
	}
	for _, v := range e.Vectors {
		if err := s.pvVector(v, e.ValueID); err != nil {
			return err
		}
		s.pvVectorOps++
	}
	for _, v := range e.Bitmaps {
		if err := s.pvBitmap(v); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) pvKind(ns schema.Namespace, want schema.NamespaceKind) error {
	def, ok := s.schema.Lookup(ns)
	if !ok || def.Kind != want {
		return fmt.Errorf("%w: namespace %d", ErrWrongEngine, ns)
	}
	if s.isDegraded(ns) || int(ns) >= len(s.reg.routes) || !s.reg.routes[ns].available {
		return fmt.Errorf("%w: namespace %d", ErrNamespaceUnavailable, ns)
	}
	s.pvMark(ns)
	return nil
}

func (s *Store) pvBitmap(v BitmapValue) error {
	def, ok := s.schema.Lookup(v.Namespace)
	if !ok || !def.Kind.IsBitmap() || def.Kind != v.Value.Kind {
		return fmt.Errorf("%w: namespace %d", ErrWrongEngine, v.Namespace)
	}
	if s.isDegraded(v.Namespace) || int(v.Namespace) >= len(s.reg.routes) || !s.reg.routes[v.Namespace].available {
		return fmt.Errorf("%w: namespace %d", ErrNamespaceUnavailable, v.Namespace)
	}
	s.pvMark(v.Namespace)
	return nil
}

func (s *Store) pvSim(ns schema.Namespace, id schema.ValueID) error {
	def, ok := s.schema.Lookup(ns)
	if !ok || def.Kind != schema.KindHashPerceptual {
		return fmt.Errorf("%w: namespace %d", ErrWrongEngine, ns)
	}
	if s.isDegraded(ns) {
		return fmt.Errorf("%w: namespace %d", ErrNamespaceUnavailable, ns)
	}
	idx, ok := s.reg.simStore(ns)
	if !ok {
		return fmt.Errorf("%w: namespace %d", ErrNamespaceUnavailable, ns)
	}
	s.pvMark(ns)
	n := idx.Len()
	if id <= n {
		// Updating an existing record is journaled and crash-durable.
		s.pvSimUpdate = true
	}
	for i := range s.pvCursors {
		if s.pvCursors[i].ns == ns {
			switch {
			case id == s.pvCursors[i].next:
				s.pvCursors[i].next++
				return nil
			case id <= n:
				return nil
			default:
				return fmt.Errorf("%w: simhash id %d, next %d", ErrNamespaceIDModel, id, s.pvCursors[i].next)
			}
		}
	}
	switch {
	case id <= n:
		s.pvCursors = append(s.pvCursors, simCursor{ns: ns, next: n + 1})
		return nil
	case id == n+1:
		s.pvCursors = append(s.pvCursors, simCursor{ns: ns, next: n + 2})
		return nil
	default:
		return fmt.Errorf("%w: simhash id %d, next %d", ErrNamespaceIDModel, id, n+1)
	}
}

func (s *Store) pvVector(v VectorValue, id schema.ValueID) error {
	def, ok := s.schema.Lookup(v.Namespace)
	if !ok || def.Kind != schema.KindVector {
		return fmt.Errorf("%w: namespace %d", ErrWrongEngine, v.Namespace)
	}
	if s.isDegraded(v.Namespace) {
		return fmt.Errorf("%w: namespace %d", ErrNamespaceUnavailable, v.Namespace)
	}
	idx, ok := s.reg.vectorIndex(v.Namespace)
	if !ok {
		return fmt.Errorf("%w: namespace %d", ErrNamespaceUnavailable, v.Namespace)
	}
	cfg := idx.Config()
	if v.Element != cfg.ElementType || !vectorLenMatches(v, int(cfg.Dimensions)) {
		return fmt.Errorf("%w: vector element/dimension", ErrWrongEngine)
	}
	if id > math.MaxUint32 {
		return fmt.Errorf("%w: %d", ErrValueIDTooLarge, id)
	}
	s.pvMark(v.Namespace)
	return nil
}

func vectorLenMatches(v VectorValue, dims int) bool {
	switch v.Element {
	case vectoridx.ElementFloat32:
		return len(v.Float32) == dims
	case vectoridx.ElementInt8:
		return len(v.Int8) == dims
	case vectoridx.ElementUint8:
		return len(v.Uint8) == dims
	case vectoridx.ElementInt16:
		return len(v.Int16) == dims
	case vectoridx.ElementUint16:
		return len(v.Uint16) == dims
	default:
		return false
	}
}

func (s *Store) pvMark(ns schema.Namespace) {
	w := int(ns) >> 6
	if w < len(s.pvSet) {
		s.pvSet[w] |= 1 << (uint(ns) & 63)
	}
}

// degradeIntended marks every namespace the failed batch intended to modify as
// stale, so no later query silently uses a possibly incomplete namespace.
func (s *Store) degradeIntended(err error) {
	for w, word := range s.pvSet {
		for bit := 0; bit < 64; bit++ {
			if word&(1<<uint(bit)) != 0 {
				s.markDegraded(schema.Namespace(w*64+bit), err)
			}
		}
	}
}

func changeID(c Change) (schema.ValueID, error) {
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
	// Multi-valued families: diff individual values. Hashes are removed by
	// (hash, id) through radixhash's targeted deletion so a hash update cannot
	// leave a stale association behind; this does not rely on liveness.
	for _, av := range c.After.Hashes {
		if err := s.addHash(av, id, &touched); err != nil {
			return err
		}
	}
	for _, bv := range c.Before.Hashes {
		if !hasHash(c.After.Hashes, bv) {
			if err := s.removeHash(bv, id, &touched); err != nil {
				return err
			}
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
			if err := s.deleteTime(schema.Namespace(ns), d.ID); err != nil {
				return err
			}
		case familyVector:
			if err := s.deleteVector(schema.Namespace(ns), d.ID); err != nil {
				return err
			}
		}
	}
	return nil
}

// entitySet is a bitset over entity kinds, avoiding a map for touched tracking.
type entitySet uint32

func (e *entitySet) add(k schema.EntityKind) { *e |= 1 << uint(k) }

// setLiveness marks the entity live for every touched entity kind.
func (s *Store) setLiveness(id schema.ValueID, touched entitySet) error {
	if id == 0 || touched == 0 {
		return nil
	}
	for k := schema.EntityImage; k.Valid(); k++ {
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

func (s *Store) addTimes(id schema.ValueID, values []TimeValue, touched *entitySet) error {
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

func (s *Store) addGeos(id schema.ValueID, values []GeoValue, touched *entitySet) error {
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

func (s *Store) addSims(id schema.ValueID, values []SimValue, touched *entitySet) error {
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
func (s *Store) addSim(v SimValue, id schema.ValueID, touched *entitySet) error {
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

func (s *Store) addHashes(id schema.ValueID, values []HashValue, touched *entitySet) error {
	for _, v := range values {
		if err := s.addHash(v, id, touched); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) addHash(v HashValue, id schema.ValueID, touched *entitySet) error {
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

// removeHash deletes one (hash, id) association. radixhash supports targeted
// per-association removal, so the old hash stops matching without a rebuild and
// without relying on entity liveness.
func (s *Store) removeHash(v HashValue, id schema.ValueID, touched *entitySet) error {
	idx, rt, err := s.radixFor(v.Namespace)
	if err != nil {
		return err
	}
	if err := idx.Remove([]radixhash.Entry{{Hash: v.Hash, ImageID: id}}); err != nil {
		return err
	}
	touched.add(rt.entity)
	return nil
}

func (s *Store) addVectors(id schema.ValueID, values []VectorValue, touched *entitySet) error {
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

func (s *Store) addBitmaps(id schema.ValueID, values []BitmapValue, touched *entitySet) error {
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

func (s *Store) removeBitmap(v BitmapValue, id schema.ValueID, touched *entitySet) error {
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

func (s *Store) deleteTime(ns schema.Namespace, id schema.ValueID) error {
	idx, _, err := s.timeFor(ns)
	if err != nil {
		return err
	}
	return applyTimeMutation(idx, timeidx.Delete(id, 0))
}

func (s *Store) deleteGeo(ns schema.Namespace, key geoidx.S2Key, id schema.ValueID) error {
	idx, _, err := s.geoFor(ns)
	if err != nil {
		return err
	}
	return idx.Delete(key, id)
}

func (s *Store) deleteVector(ns schema.Namespace, id schema.ValueID) error {
	idx, _, err := s.vectorFor(ns)
	if err != nil {
		return err
	}
	return deleteVectorIndex(idx, id)
}

func deleteVectorIndex(idx *vectoridx.VectorIdx, id schema.ValueID) error {
	if id > math.MaxUint32 {
		return fmt.Errorf("%w: %d", ErrValueIDTooLarge, id)
	}
	if err := idx.Delete(vectoridx.NodeID(id)); err != nil && err != vectoridx.ErrNodeNotFound {
		return err
	}
	return nil
}

// ---- liveness ----

func (s *Store) livenessAdd(entity schema.EntityKind, id schema.ValueID) error {
	idx, value, ok := s.reg.liveness(entity)
	if !ok {
		return fmt.Errorf("%w: entity %d", ErrEntityMismatch, entity)
	}
	return applyBitmapMutation(idx, true, id, value)
}

func (s *Store) clearLiveness(entity schema.EntityKind, id schema.ValueID) error {
	idx, value, ok := s.reg.liveness(entity)
	if !ok {
		return fmt.Errorf("%w: entity %d", ErrEntityMismatch, entity)
	}
	return applyBitmapMutation(idx, false, id, value)
}

func applyBitmapMutation(idx *bitmapidx.Index, add bool, id schema.ValueID, value bitmapidx.Value) error {
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

func (s *Store) timeFor(ns schema.Namespace) (*timeidx.Index, route, error) {
	rt, ok := s.reg.lookup(ns)
	if !ok {
		return nil, route{}, fmt.Errorf("%w: %d", ErrNamespaceNotFound, ns)
	}
	if s.isDegraded(ns) {
		return nil, rt, fmt.Errorf("%w: %d", ErrNamespaceUnavailable, ns)
	}
	idx, ok := s.reg.timeIndex(ns)
	if !ok {
		return nil, rt, fmt.Errorf("%w: %d", ErrNamespaceUnavailable, ns)
	}
	return idx, rt, nil
}

func (s *Store) geoFor(ns schema.Namespace) (*geoidx.Index, route, error) {
	rt, ok := s.reg.lookup(ns)
	if !ok {
		return nil, route{}, fmt.Errorf("%w: %d", ErrNamespaceNotFound, ns)
	}
	if s.isDegraded(ns) {
		return nil, rt, fmt.Errorf("%w: %d", ErrNamespaceUnavailable, ns)
	}
	idx, ok := s.reg.geoIndex(ns)
	if !ok {
		return nil, rt, fmt.Errorf("%w: %d", ErrNamespaceUnavailable, ns)
	}
	return idx, rt, nil
}

func (s *Store) simFor(ns schema.Namespace) (*simhash.Store, route, error) {
	rt, ok := s.reg.lookup(ns)
	if !ok {
		return nil, route{}, fmt.Errorf("%w: %d", ErrNamespaceNotFound, ns)
	}
	if s.isDegraded(ns) {
		return nil, rt, fmt.Errorf("%w: %d", ErrNamespaceUnavailable, ns)
	}
	st, ok := s.reg.simStore(ns)
	if !ok {
		return nil, rt, fmt.Errorf("%w: %d", ErrNamespaceUnavailable, ns)
	}
	return st, rt, nil
}

func (s *Store) radixFor(ns schema.Namespace) (*radixhash.Index, route, error) {
	rt, ok := s.reg.lookup(ns)
	if !ok {
		return nil, route{}, fmt.Errorf("%w: %d", ErrNamespaceNotFound, ns)
	}
	if s.isDegraded(ns) {
		return nil, rt, fmt.Errorf("%w: %d", ErrNamespaceUnavailable, ns)
	}
	idx, ok := s.reg.radixIndex(ns)
	if !ok {
		return nil, rt, fmt.Errorf("%w: %d", ErrNamespaceUnavailable, ns)
	}
	return idx, rt, nil
}

func (s *Store) vectorFor(ns schema.Namespace) (*vectoridx.VectorIdx, route, error) {
	rt, ok := s.reg.lookup(ns)
	if !ok {
		return nil, route{}, fmt.Errorf("%w: %d", ErrNamespaceNotFound, ns)
	}
	if s.isDegraded(ns) {
		return nil, rt, fmt.Errorf("%w: %d", ErrNamespaceUnavailable, ns)
	}
	idx, ok := s.reg.vectorIndex(ns)
	if !ok {
		return nil, rt, fmt.Errorf("%w: %d", ErrNamespaceUnavailable, ns)
	}
	return idx, rt, nil
}

func (s *Store) bitmapFor(ns schema.Namespace) (*bitmapidx.Index, route, error) {
	rt, ok := s.reg.lookup(ns)
	if !ok {
		return nil, route{}, fmt.Errorf("%w: %d", ErrNamespaceNotFound, ns)
	}
	if s.isDegraded(ns) {
		return nil, rt, fmt.Errorf("%w: %d", ErrNamespaceUnavailable, ns)
	}
	idx, ok := s.reg.bitmapIndex(ns)
	if !ok {
		return nil, rt, fmt.Errorf("%w: %d", ErrNamespaceUnavailable, ns)
	}
	return idx, rt, nil
}

// ---- value comparisons for set difference ----

func hasTimeNS(values []TimeValue, ns schema.Namespace) bool {
	for _, v := range values {
		if v.Namespace == ns {
			return true
		}
	}
	return false
}

func hasGeoNS(values []GeoValue, ns schema.Namespace) bool {
	for _, v := range values {
		if v.Namespace == ns {
			return true
		}
	}
	return false
}

func hasVectorNS(values []VectorValue, ns schema.Namespace) bool {
	for _, v := range values {
		if v.Namespace == ns {
			return true
		}
	}
	return false
}

func hasHash(values []HashValue, want HashValue) bool {
	for _, v := range values {
		if v.Namespace == want.Namespace && v.Hash == want.Hash {
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
