package bitmapidx

import (
	"sort"
	"sync"
	"sync/atomic"
)

type directorySnapshot struct {
	byKey map[BitmapKey]*BitmapIndex
	byID  map[uint64]*BitmapIndex
	order []*BitmapIndex
}

// BitmapDirectory publishes immutable lookup maps, keeping reads lock-free.
type BitmapDirectory struct {
	view     atomic.Pointer[directorySnapshot]
	writerMu sync.Mutex
}

func NewBitmapDirectory(indexes ...*BitmapIndex) (*BitmapDirectory, error) {
	directory := &BitmapDirectory{}
	directory.view.Store(&directorySnapshot{
		byKey: make(map[BitmapKey]*BitmapIndex),
		byID:  make(map[uint64]*BitmapIndex),
	})
	if len(indexes) != 0 {
		if err := directory.Register(indexes...); err != nil {
			return nil, err
		}
	}
	return directory, nil
}

func (d *BitmapDirectory) Lookup(key BitmapKey) (*BitmapIndex, bool) {
	index, ok := d.view.Load().byKey[key]
	return index, ok
}

func (d *BitmapDirectory) LookupID(indexID uint64) (*BitmapIndex, bool) {
	index, ok := d.view.Load().byID[indexID]
	return index, ok
}

func (d *BitmapDirectory) Len() int { return len(d.view.Load().order) }

// Indexes returns indexes in physical storage order: IndexType, ValueID.
func (d *BitmapDirectory) Indexes() []*BitmapIndex {
	return append([]*BitmapIndex(nil), d.view.Load().order...)
}

func (d *BitmapDirectory) Register(indexes ...*BitmapIndex) error {
	d.writerMu.Lock()
	defer d.writerMu.Unlock()
	old := d.view.Load()
	next := &directorySnapshot{
		byKey: make(map[BitmapKey]*BitmapIndex, len(old.byKey)+len(indexes)),
		byID:  make(map[uint64]*BitmapIndex, len(old.byID)+len(indexes)),
		order: append([]*BitmapIndex(nil), old.order...),
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
		key := BitmapKey{Type: view.IndexType, ValueID: view.ValueID}
		if _, exists := next.byKey[key]; exists {
			return ErrDuplicateKey
		}
		if _, exists := next.byID[view.IndexID]; exists {
			return ErrDuplicateID
		}
		next.byKey[key] = index
		next.byID[view.IndexID] = index
		next.order = append(next.order, index)
	}
	sort.Slice(next.order, func(i, j int) bool {
		a, b := next.order[i].view.Load(), next.order[j].view.Load()
		if a.IndexType != b.IndexType {
			return a.IndexType < b.IndexType
		}
		if a.ValueID != b.ValueID {
			return a.ValueID < b.ValueID
		}
		return a.IndexID < b.IndexID
	})
	d.view.Store(next)
	return nil
}
