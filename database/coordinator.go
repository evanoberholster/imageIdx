package database

import (
	"errors"
	"fmt"
	"math"

	"github.com/evanoberholster/imageIdx/index"
	schema "github.com/evanoberholster/imageIdx/schema"
	"github.com/evanoberholster/imageIdx/store"
)

func (db *DB) run() {
	defer close(db.done)
	batch := db.indexes.NewBatch(db.options.BatchSize)
	ws := newCoordinatorWorkspace(db.options.MaxProjectionBytes, db.options.OversizedRetentionBytes)
	for range db.wake {
		for {
			db.mu.Lock()
			closed, indexed, last := db.closed, db.indexed, db.committed
			db.mu.Unlock()
			if closed {
				return
			}
			if indexed >= last {
				break
			}
			next := indexed + 1
			ws.reset()
			err := db.values.ScanFrom(journalNS, next, func(key store.Key, raw []byte) (bool, error) {
				if key.ID > last || ws.count >= db.options.BatchSize {
					return false, nil
				}
				if key.Flags != 0 || key.ID != next+uint64(ws.count) {
					return false, ErrCorruptJournal
				}
				entry, err := decodeMutation(raw)
				if err != nil {
					return false, err
				}
				if !ws.appendMutation(key.ID, raw, entry) {
					return false, nil
				}
				return true, nil
			})
			if err == nil && ws.count == 0 {
				err = ErrCorruptJournal
			}
			if err == nil {
				err = db.applyWorkspace(ws, batch)
			}
			if err != nil {
				db.mu.Lock()
				db.err = err
				db.notify()
				db.mu.Unlock()
				return
			}
			ws.reset()
		}
	}
}

func (db *DB) decodeProjection(raw []byte) (index.Entry, error) {
	if db.decodeInto != nil {
		var entry index.Entry
		if err := db.decodeInto(raw, &entry, nil); err != nil {
			return index.Entry{}, err
		}
		return entry, nil
	}
	return db.decode(raw)
}

func (db *DB) decodeEntry(raw []byte, entity schema.EntityKind, id schema.ValueID) (index.Entry, error) {
	var entry index.Entry
	var err error
	if db.decodeInto != nil {
		err = db.decodeInto(raw, &entry, nil)
	} else {
		entry, err = db.decode(raw)
	}
	if err != nil {
		return index.Entry{}, fmt.Errorf("%w: %v", ErrCorruptJournal, err)
	}
	if err := db.validateDecodedEntry(&entry, entity, id); err != nil {
		return index.Entry{}, err
	}
	return entry, nil
}

func (db *DB) decodeEntryInto(raw []byte, entity schema.EntityKind, id schema.ValueID, dst *index.Entry, scratch *DecodeScratch) error {
	if db.decodeInto != nil {
		resetEntry(dst)
		if scratch != nil {
			scratch.Bytes = scratch.Bytes[:0]
		}
		if err := db.decodeInto(raw, dst, scratch); err != nil {
			return fmt.Errorf("%w: %v", ErrCorruptJournal, err)
		}
	} else {
		entry, err := db.decode(raw)
		if err != nil {
			return fmt.Errorf("%w: %v", ErrCorruptJournal, err)
		}
		*dst = entry
	}
	return db.validateDecodedEntry(dst, entity, id)
}

func (db *DB) validateDecodedEntry(entry *index.Entry, entity schema.EntityKind, id schema.ValueID) error {
	if entry.ValueID != id {
		return ErrCorruptJournal
	}
	for _, v := range entry.Times {
		if !db.owns(v.Namespace, entity) {
			return ErrCorruptJournal
		}
	}
	for _, v := range entry.Geos {
		if !db.owns(v.Namespace, entity) {
			return ErrCorruptJournal
		}
	}
	for _, v := range entry.Sims {
		if !db.owns(v.Namespace, entity) {
			return ErrCorruptJournal
		}
	}
	for _, v := range entry.Hashes {
		if !db.owns(v.Namespace, entity) {
			return ErrCorruptJournal
		}
	}
	for _, v := range entry.Vectors {
		if !db.owns(v.Namespace, entity) {
			return ErrCorruptJournal
		}
	}
	for _, v := range entry.Bitmaps {
		if !db.owns(v.Namespace, entity) {
			return ErrCorruptJournal
		}
	}
	return nil
}

func (db *DB) owns(ns schema.Namespace, entity schema.EntityKind) bool {
	owner, ok := db.indexes.EntityOf(ns)
	return ok && owner == entity
}

func (db *DB) decodeWorkspace(ws *coordinatorWorkspace) error {
	for i := 0; i < ws.count; i++ {
		if err := db.decodeEvent(ws, i); err != nil {
			return err
		}
	}
	return nil
}

func (db *DB) decodeEvent(ws *coordinatorWorkspace, i int) error {
	m := &ws.mutations[i]
	event := &ws.decoded[i]
	if m.before.valid {
		if err := db.decodeEntryInto(ws.refBytes(m.before), m.entity, m.id, &event.before, &ws.scratch[i]); err != nil {
			return err
		}
	}
	if !m.deleted {
		if err := db.decodeEntryInto(ws.refBytes(m.after), m.entity, m.id, &event.after, &ws.scratch[i]); err != nil {
			return err
		}
	}
	return nil
}

func (db *DB) applyRange(ws *coordinatorWorkspace, start, end int, batch *index.Batch) error {
	if end <= start {
		return nil
	}
	if err := batch.Reset(); err != nil {
		return err
	}
	for i := start; i < end; i++ {
		if err := db.stageEvent(ws, i, batch); err != nil {
			return err
		}
	}
	last := ws.mutations[end-1].sequence
	return db.commitAndApply(batch, last)
}

func (db *DB) stageEvent(ws *coordinatorWorkspace, i int, batch *index.Batch) error {
	m := &ws.mutations[i]
	event := &ws.decoded[i]
	switch {
	case m.deleted:
		return batch.Delete(m.entity, m.id)
	case !m.before.valid:
		return batch.Add(event.after)
	default:
		before, after := changedProjectionScratch(event.before, event.after, ws.diff(i))
		if entryAssociations(before)+entryAssociations(after) == 0 {
			return nil
		}
		return batch.Update(index.Change{Before: before, After: after})
	}
}

func entryAssociations(entry index.Entry) int {
	return len(entry.Times) + len(entry.Geos) + len(entry.Sims) + len(entry.Hashes) +
		len(entry.Vectors) + len(entry.Bitmaps)
}

func (db *DB) commitAndApply(batch *index.Batch, last uint64) error {
	// Persist intent before touching any index engine. On reopen, an
	// incomplete intent is reported as recovery-required, not replayed over
	// unknown partial engine state (notably dense simhash appends).
	b := db.values.NewBatch()
	if err := putGeneration(b, 3, last); err != nil {
		_ = b.Close()
		return err
	}
	err := b.Commit()
	_ = b.Close()
	if err != nil {
		return err
	}
	db.stats.generationIntentCommits.Add(1)
	if db.options.boundary != nil {
		db.options.boundary("before index apply")
	}
	if err := db.indexes.ApplyWithGenerationAll(batch, last); err != nil {
		return fmt.Errorf("%w: %v", ErrRecoveryRequired, err)
	}
	db.stats.indexApplies.Add(1)
	if db.indexes.SourceGeneration() < last {
		return ErrRecoveryRequired
	}
	if db.options.boundary != nil {
		db.options.boundary("after durable index")
	}
	return db.acknowledge(last)
}

// acknowledge commits the watermark before trying cleanup. Cleanup is
// independently retryable; its failure cannot invalidate an acknowledgment.
func (db *DB) acknowledge(last uint64) error {
	db.mu.Lock()
	start := db.indexed
	db.mu.Unlock()
	if last <= start {
		return nil
	}
	b := db.values.NewBatch()
	if err := putGeneration(b, 2, last); err != nil {
		_ = b.Close()
		return err
	}
	if err := putGeneration(b, 3, 0); err != nil {
		_ = b.Close()
		return err
	}
	err := b.Commit()
	_ = b.Close()
	if err != nil {
		return err
	}
	db.stats.acknowledgmentCommits.Add(1)
	if db.options.boundary != nil {
		db.options.boundary("after acknowledgment")
	}
	db.mu.Lock()
	db.indexed = last
	db.inflight = 0
	db.notify()
	db.mu.Unlock()
	// DeleteRange uses one Pebble range tombstone rather than one delete per
	// image; a crash here leaves only already-acknowledged journal records.
	return db.cleanup(start, last)
}

func (db *DB) cleanup(start, last uint64) error {
	if last <= start {
		return nil
	}
	if db.options.boundary != nil {
		db.options.boundary("during cleanup")
	}
	b := db.values.NewBatch()
	var err error
	if last == math.MaxUint64 {
		if start+1 < last {
			err = b.DeleteRange(journalNS, start+1, last)
		}
		if err == nil {
			err = b.Delete(store.Key{Namespace: journalNS, ID: last})
		}
	} else {
		err = b.DeleteRange(journalNS, start+1, last+1)
	}
	if err == nil {
		err = b.Commit()
	}
	if err == nil {
		db.stats.cleanupCommits.Add(1)
	}
	return errors.Join(err, b.Close())
}
