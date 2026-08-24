package bitmapidx

import (
	"sort"
	"sync"
	"sync/atomic"
)

// directorySnapshot is one immutable generation of the lookup maps. Reads load
// the snapshot once and access the maps without locking; writes build a new
// snapshot and publish it atomically.
type directorySnapshot struct {
	byKey map[Key]*Index
	byID  map[uint64]*Index
	order []*Index
}

// Directory maps predicate Keys and stable IndexIDs to Indexes. Lookups are
// lock-free: they load one atomically published snapshot and read an immutable
// map. Only Register takes the writer lock.
type Directory struct {
	view     atomic.Pointer[directorySnapshot]
	writerMu sync.Mutex
}

// NewDirectory constructs an empty lock-free lookup directory, optionally
// pre-populated with indexes.
func NewDirectory(indexes ...*Index) (*Directory, error) {
	directory := &Directory{}
	directory.view.Store(&directorySnapshot{
		byKey: make(map[Key]*Index),
		byID:  make(map[uint64]*Index),
	})
	if len(indexes) != 0 {
		if err := directory.Register(indexes...); err != nil {
			return nil, err
		}
	}
	return directory, nil
}

// Lookup returns the index registered under key.
func (d *Directory) Lookup(key Key) (*Index, bool) {
	index, ok := d.view.Load().byKey[key]
	return index, ok
}

// LookupID returns the index registered under a stable numeric identifier.
func (d *Directory) LookupID(indexID uint64) (*Index, bool) {
	index, ok := d.view.Load().byID[indexID]
	return index, ok
}

// Len returns the number of registered indexes.
func (d *Directory) Len() int { return len(d.view.Load().order) }

// Indexes returns a copy of the indexes in physical storage order:
// IndexType, ValueID.
func (d *Directory) Indexes() []*Index {
	return append([]*Index(nil), d.view.Load().order...)
}

// Register adds indexes to the directory. It copy-on-writes the snapshot maps,
// so concurrent readers keep using the previous generation until the new one
// is published atomically.
func (d *Directory) Register(indexes ...*Index) error {
	d.writerMu.Lock()
	defer d.writerMu.Unlock()
	old := d.view.Load()
	next := &directorySnapshot{
		byKey: make(map[Key]*Index, len(old.byKey)+len(indexes)),
		byID:  make(map[uint64]*Index, len(old.byID)+len(indexes)),
		order: append([]*Index(nil), old.order...),
	}
	for key, index := range old.byKey {
		next.byKey[key] = index
	}
	for id, index := range old.byID {
		next.byID[id] = index
	}
	for _, index := range indexes {
		if index == nil {
			return ErrInvalidIndex
		}
		view := index.view.Load()
		key := Key{Type: view.indexType, ValueID: view.valueID}
		if _, exists := next.byKey[key]; exists {
			return ErrDuplicateKey
		}
		if _, exists := next.byID[view.indexID]; exists {
			return ErrDuplicateID
		}
		next.byKey[key] = index
		next.byID[view.indexID] = index
		next.order = append(next.order, index)
	}
	sort.Slice(next.order, func(i, j int) bool {
		a, b := next.order[i].view.Load(), next.order[j].view.Load()
		if a.indexType != b.indexType {
			return a.indexType < b.indexType
		}
		if a.valueID != b.valueID {
			return a.valueID < b.valueID
		}
		return a.indexID < b.indexID
	})
	d.view.Store(next)
	return nil
}
