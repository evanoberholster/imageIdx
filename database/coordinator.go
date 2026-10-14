package database

import (
	"errors"
	"fmt"
	"math"

	"github.com/evanoberholster/imageIdx/index"
	schema "github.com/evanoberholster/imageIdx/schema"
	"github.com/evanoberholster/imageIdx/store"
)

type queued struct {
	sequence uint64
	entry    journalEntry
}

func (db *DB) run() {
	defer close(db.done)
	batch := db.indexes.NewBatch(db.options.BatchSize)
	entries := make([]queued, 0, db.options.BatchSize)
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
			entries = entries[:0]
			err := db.values.ScanFrom(journalNS, next, func(key store.Key, raw []byte) (bool, error) {
				if key.ID > last || len(entries) >= db.options.BatchSize {
					return false, nil
				}
				if key.Flags != 0 || key.ID != next+uint64(len(entries)) {
					return false, ErrCorruptJournal
				}
				entry, err := decodeMutation(raw)
				if err != nil {
					return false, err
				}
				// index.Batch groups operations by kind. Preserve mutation order
				// when a key appears twice by ending the batch before its reuse.
				for i := range entries {
					if entries[i].entry.entity == entry.entity && entries[i].entry.id == entry.id {
						return false, nil
					}
				}
				entry.before = append([]byte(nil), entry.before...)
				entry.after = append([]byte(nil), entry.after...)
				entries = append(entries, queued{key.ID, entry})
				return true, nil
			})
			if err == nil && len(entries) == 0 {
				err = ErrCorruptJournal
			}
			if err == nil {
				err = db.apply(entries, batch)
			}
			if err != nil {
				db.mu.Lock()
				db.err = err
				db.notify()
				db.mu.Unlock()
				return
			}
		}
	}
}

func (db *DB) decodeEntry(raw []byte, entity schema.EntityKind, id schema.ValueID) (index.Entry, error) {
	entry, err := db.decode(raw)
	if err != nil {
		return index.Entry{}, fmt.Errorf("%w: %v", ErrCorruptJournal, err)
	}
	if entry.ValueID != id {
		return index.Entry{}, ErrCorruptJournal
	}
	for _, v := range entry.Times {
		if !db.owns(v.Namespace, entity) {
			return index.Entry{}, ErrCorruptJournal
		}
	}
	for _, v := range entry.Geos {
		if !db.owns(v.Namespace, entity) {
			return index.Entry{}, ErrCorruptJournal
		}
	}
	for _, v := range entry.Sims {
		if !db.owns(v.Namespace, entity) {
			return index.Entry{}, ErrCorruptJournal
		}
	}
	for _, v := range entry.Hashes {
		if !db.owns(v.Namespace, entity) {
			return index.Entry{}, ErrCorruptJournal
		}
	}
	for _, v := range entry.Vectors {
		if !db.owns(v.Namespace, entity) {
			return index.Entry{}, ErrCorruptJournal
		}
	}
	for _, v := range entry.Bitmaps {
		if !db.owns(v.Namespace, entity) {
			return index.Entry{}, ErrCorruptJournal
		}
	}
	return entry, nil
}

func (db *DB) owns(ns schema.Namespace, entity schema.EntityKind) bool {
	owner, ok := db.indexes.EntityOf(ns)
	return ok && owner == entity
}

func (db *DB) apply(entries []queued, batch *index.Batch) error {
	if err := batch.Reset(); err != nil {
		return err
	}
	for _, item := range entries {
		e := item.entry
		var before, after index.Entry
		var err error
		if len(e.before) != 0 {
			before, err = db.decodeEntry(e.before, e.entity, e.id)
			if err != nil {
				return err
			}
		}
		if !e.deleted {
			after, err = db.decodeEntry(e.after, e.entity, e.id)
			if err != nil {
				return err
			}
		}
		switch {
		case e.deleted:
			err = batch.Delete(e.entity, e.id)
		case len(e.before) == 0:
			err = batch.Add(after)
		default:
			before, after = changedProjection(before, after)
			if len(before.Times)+len(after.Times)+len(before.Geos)+len(after.Geos)+
				len(before.Sims)+len(after.Sims)+len(before.Hashes)+len(after.Hashes)+
				len(before.Vectors)+len(after.Vectors)+len(before.Bitmaps)+len(after.Bitmaps) != 0 {
				err = batch.Update(index.Change{Before: before, After: after})
			}
		}
		if err != nil {
			return err
		}
	}
	last := entries[len(entries)-1].sequence
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
	if db.options.boundary != nil {
		db.options.boundary("before index apply")
	}
	if err := db.indexes.ApplyWithGenerationAll(batch, last); err != nil {
		return fmt.Errorf("%w: %v", ErrRecoveryRequired, err)
	}
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
	return errors.Join(err, b.Close())
}
