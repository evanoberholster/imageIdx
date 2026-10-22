package database

import (
	"github.com/evanoberholster/imageIdx/index"
	schema "github.com/evanoberholster/imageIdx/schema"
)

// buildGroups finds repeated (entity, value ID) events in one bounded journal
// interval. It is deliberately a linear scan over at most 64 entries; no map is
// allocated. safe is false when a group touches an engine whose interleaved
// semantics have not been proven for entity-level coalescing.
func (db *DB) buildGroups(ws *coordinatorWorkspace) ([]mutationGroup, bool) {
	ws.groupN = 0
	safe := true
	for i := 0; i < ws.count; i++ {
		m := &ws.mutations[i]
		event := &ws.decoded[i]
		// SimHash dense appends and vector graph updates have engine-local
		// ordering/precondition rules. Fall back to ordered sub-batches rather
		// than guessing at a merged representation.
		if len(event.before.Sims) != 0 || len(event.after.Sims) != 0 ||
			len(event.before.Vectors) != 0 || len(event.after.Vectors) != 0 {
			safe = false
		}

		group := -1
		for g := 0; g < ws.groupN; g++ {
			if ws.groups[g].entity == m.entity && ws.groups[g].id == m.id {
				group = g
				break
			}
		}
		if group < 0 {
			group = ws.groupN
			ws.groupN++
			ws.groups[group] = mutationGroup{entity: m.entity, id: m.id, first: i, last: i}
		}
		ws.groups[group].last = i
		if m.deleted {
			ws.groups[group].sawDelete = true
		}
	}
	for g := 0; g < ws.groupN; g++ {
		group := &ws.groups[g]
		if group.sawDelete && !ws.mutations[group.last].deleted {
			// A delete/reinsert toggles liveness at an intermediate publication.
			// It is not safe to replace that sequence with a projection diff that
			// may have no association changes at all.
			safe = false
		}
		if group.last != group.first && !ws.mutations[group.last].deleted &&
			entryAssociations(ws.decoded[group.last].after) == 0 {
			// An empty final projection cannot carry liveness through the
			// entity-level Add path; preserve the original events.
			safe = false
		}
	}
	return ws.groups[:ws.groupN], safe
}

func (db *DB) applyWorkspace(ws *coordinatorWorkspace, batch *index.Batch) error {
	if !ws.hasRepeatedKey() {
		return db.applySequentialDirect(ws, batch)
	}
	if err := db.decodeWorkspace(ws); err != nil {
		return err
	}
	groups, safe := db.buildGroups(ws)
	if !safe {
		return db.applySequentialGroups(ws, batch)
	}
	return db.applyCoalescedGroups(ws, groups, batch)
}

// applySequentialDirect is the common no-repeat path: decode and stage one
// event at a time, exactly in journal order, without retaining a second decoded
// copy of the whole interval.
func (db *DB) applySequentialDirect(ws *coordinatorWorkspace, batch *index.Batch) error {
	if err := batch.Reset(); err != nil {
		return err
	}
	for i := 0; i < ws.count; i++ {
		if err := db.decodeEvent(ws, i); err != nil {
			return err
		}
		if err := db.stageEvent(ws, i, batch); err != nil {
			return err
		}
	}
	last := ws.mutations[ws.count-1].sequence
	return db.commitAndApply(batch, last)
}

// applySequentialGroups is the conservative fallback: preserve original order
// and split only where an entity/ID would otherwise appear twice in one
// index.Batch. Each sub-batch is acknowledged at its own last sequence.
func (db *DB) applySequentialGroups(ws *coordinatorWorkspace, batch *index.Batch) error {
	for start := 0; start < ws.count; {
		end := start
		for end < ws.count {
			if hasKeyInRange(ws, start, end, ws.mutations[end].entity, ws.mutations[end].id) {
				break
			}
			end++
		}
		if end == start {
			end++
		}
		if err := db.applyRange(ws, start, end, batch); err != nil {
			return err
		}
		start = end
	}
	return nil
}

func hasKeyInRange(ws *coordinatorWorkspace, start, end int, entity schema.EntityKind, id schema.ValueID) bool {
	for i := start; i < end; i++ {
		if ws.mutations[i].entity == entity && ws.mutations[i].id == id {
			return true
		}
	}
	return false
}

func (db *DB) applyCoalescedGroups(ws *coordinatorWorkspace, groups []mutationGroup, batch *index.Batch) error {
	if err := batch.Reset(); err != nil {
		return err
	}
	for g := range groups {
		group := &groups[g]
		last := &ws.mutations[group.last]
		if last.deleted {
			if err := batch.Delete(group.entity, group.id); err != nil {
				return err
			}
			continue
		}

		scratch := ws.diff(g)
		first := &ws.decoded[group.first]
		scratch.timesBefore = append(scratch.timesBefore[:0], first.before.Times...)
		scratch.timesAfter = append(scratch.timesAfter[:0], first.after.Times...)
		scratch.geosBefore = append(scratch.geosBefore[:0], first.before.Geos...)
		scratch.geosAfter = append(scratch.geosAfter[:0], first.after.Geos...)
		scratch.simsBefore = append(scratch.simsBefore[:0], first.before.Sims...)
		scratch.simsAfter = append(scratch.simsAfter[:0], first.after.Sims...)
		scratch.hashesBefore = append(scratch.hashesBefore[:0], first.before.Hashes...)
		scratch.hashesAfter = append(scratch.hashesAfter[:0], first.after.Hashes...)
		scratch.vectorsBefore = append(scratch.vectorsBefore[:0], first.before.Vectors...)
		scratch.vectorsAfter = append(scratch.vectorsAfter[:0], first.after.Vectors...)
		scratch.bitmapsBefore = append(scratch.bitmapsBefore[:0], first.before.Bitmaps...)
		scratch.bitmapsAfter = append(scratch.bitmapsAfter[:0], first.after.Bitmaps...)

		for i := group.first + 1; i <= group.last; i++ {
			event := &ws.decoded[i]
			scratch.timesBefore, scratch.timesAfter = mergeDeltaSet(scratch.timesBefore, scratch.timesAfter, event.before.Times, sameTime)
			scratch.timesBefore, scratch.timesAfter = mergeAddedSet(scratch.timesBefore, scratch.timesAfter, event.after.Times, sameTime)
			scratch.geosBefore, scratch.geosAfter = mergeDeltaSet(scratch.geosBefore, scratch.geosAfter, event.before.Geos, sameGeo)
			scratch.geosBefore, scratch.geosAfter = mergeAddedSet(scratch.geosBefore, scratch.geosAfter, event.after.Geos, sameGeo)
			scratch.simsBefore, scratch.simsAfter = mergeDeltaSet(scratch.simsBefore, scratch.simsAfter, event.before.Sims, sameSim)
			scratch.simsBefore, scratch.simsAfter = mergeAddedSet(scratch.simsBefore, scratch.simsAfter, event.after.Sims, sameSim)
			scratch.hashesBefore, scratch.hashesAfter = mergeDeltaSet(scratch.hashesBefore, scratch.hashesAfter, event.before.Hashes, sameHash)
			scratch.hashesBefore, scratch.hashesAfter = mergeAddedSet(scratch.hashesBefore, scratch.hashesAfter, event.after.Hashes, sameHash)
			scratch.vectorsBefore, scratch.vectorsAfter = mergeDeltaSet(scratch.vectorsBefore, scratch.vectorsAfter, event.before.Vectors, sameVector)
			scratch.vectorsBefore, scratch.vectorsAfter = mergeAddedSet(scratch.vectorsBefore, scratch.vectorsAfter, event.after.Vectors, sameVector)
			scratch.bitmapsBefore, scratch.bitmapsAfter = mergeDeltaSet(scratch.bitmapsBefore, scratch.bitmapsAfter, event.before.Bitmaps, sameBitmap)
			scratch.bitmapsBefore, scratch.bitmapsAfter = mergeAddedSet(scratch.bitmapsBefore, scratch.bitmapsAfter, event.after.Bitmaps, sameBitmap)
		}

		after := netEntryAfter(group.id, scratch)
		if ws.mutations[group.first].insert {
			if err := batch.Add(after); err != nil {
				return err
			}
			continue
		}
		before := netEntryBefore(group.id, scratch)
		if entryAssociations(before)+entryAssociations(after) == 0 {
			if !ws.mutations[group.first].insert {
				if err := batch.Touch(group.entity, group.id); err != nil {
					return err
				}
			}
			continue
		}
		if err := batch.Update(index.Change{Before: before, After: after}); err != nil {
			return err
		}
	}
	last := ws.mutations[ws.count-1].sequence
	return db.commitAndApply(batch, last)
}

func mergeDeltaSet[T any](removed, added, event []T, equal func(T, T) bool) ([]T, []T) {
	for _, value := range event {
		if index := indexOfEqual(added, value, equal); index >= 0 {
			added = removeAt(added, index)
			continue
		}
		if indexOfEqual(removed, value, equal) < 0 {
			removed = append(removed, value)
		}
	}
	return removed, added
}

func mergeAddedSet[T any](removed, added, event []T, equal func(T, T) bool) ([]T, []T) {
	for _, value := range event {
		if index := indexOfEqual(removed, value, equal); index >= 0 {
			removed = removeAt(removed, index)
			continue
		}
		if indexOfEqual(added, value, equal) < 0 {
			added = append(added, value)
		}
	}
	return removed, added
}

func indexOfEqual[T any](values []T, target T, equal func(T, T) bool) int {
	for i := range values {
		if equal(values[i], target) {
			return i
		}
	}
	return -1
}

func removeAt[T any](values []T, index int) []T {
	return append(values[:index], values[index+1:]...)
}

func netEntryBefore(id schema.ValueID, s *projectionScratch) index.Entry {
	return index.Entry{
		ValueID: id,
		Times:   s.timesBefore,
		Geos:    s.geosBefore,
		Sims:    s.simsBefore,
		Hashes:  s.hashesBefore,
		Vectors: s.vectorsBefore,
		Bitmaps: s.bitmapsBefore,
	}
}

func netEntryAfter(id schema.ValueID, s *projectionScratch) index.Entry {
	return index.Entry{
		ValueID: id,
		Times:   s.timesAfter,
		Geos:    s.geosAfter,
		Sims:    s.simsAfter,
		Hashes:  s.hashesAfter,
		Vectors: s.vectorsAfter,
		Bitmaps: s.bitmapsAfter,
	}
}
