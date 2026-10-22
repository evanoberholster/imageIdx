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
		first := &ws.mutations[group.first]
		last := &ws.mutations[group.last]
		firstEvent := &ws.decoded[group.first]
		lastEvent := &ws.decoded[group.last]

		switch {
		case last.deleted:
			if err := batch.Delete(group.entity, group.id); err != nil {
				return err
			}
		case !first.before.valid && !group.sawDelete:
			// A pure insert (no intervening delete) is already a complete
			// final-state upsert.
			if err := batch.Add(lastEvent.after); err != nil {
				return err
			}
		default:
			initial := firstEvent.before
			if !first.before.valid {
				initial = firstEvent.after
			}
			before, after := changedProjectionScratch(initial, lastEvent.after, ws.diff(g))
			if entryAssociations(before)+entryAssociations(after) == 0 {
				continue
			}
			if err := batch.Update(index.Change{Before: before, After: after}); err != nil {
				return err
			}
		}
	}
	last := ws.mutations[ws.count-1].sequence
	return db.commitAndApply(batch, last)
}
