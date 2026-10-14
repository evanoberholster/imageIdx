package store

import (
	"encoding/binary"
	"errors"

	"github.com/cockroachdb/pebble/v2"
	schema "github.com/evanoberholster/imageIdx/schema"
)

// Batch is an exclusively owned atomic write group. A successful Commit syncs
// all its operations together; Close discards any uncommitted operations.
type Batch struct {
	store *Store
	batch *pebble.Batch
}

func (s *Store) NewBatch() *Batch { return &Batch{store: s, batch: s.db.NewBatch()} }

func (b *Batch) Set(key Key, value []byte) error {
	if err := b.store.check(key); err != nil {
		return err
	}
	k := EncodeKey(key)
	return b.batch.Set(k[:], value, nil)
}

func (b *Batch) Delete(key Key) error {
	if err := b.store.check(key); err != nil {
		return err
	}
	k := EncodeKey(key)
	return b.batch.Delete(k[:], nil)
}

// DeleteRange removes [start, end), including all flags for IDs in the range.
// end must be greater than start; the operation cannot cross namespaces.
func (b *Batch) DeleteRange(ns schema.Namespace, start, end uint64) error {
	if err := b.store.check(Key{Namespace: ns}); err != nil {
		return err
	}
	if end <= start {
		return ErrInvalidKey
	}
	a := EncodeKey(Key{Namespace: ns, ID: start})
	z := EncodeKey(Key{Namespace: ns, ID: end})
	return b.batch.DeleteRange(a[:], z[:], nil)
}

func (b *Batch) Commit() error { return b.batch.Commit(pebble.Sync) }
func (b *Batch) Close() error  { return b.batch.Close() }

// ScanFrom visits keys starting at start, stopping when visit returns false.
// Values are borrowed until the next iterator step; do not retain them.
func (s *Store) ScanFrom(ns schema.Namespace, start uint64, visit func(Key, []byte) (bool, error)) error {
	if err := s.check(Key{Namespace: ns}); err != nil {
		return err
	}
	lower := EncodeKey(Key{Namespace: ns, ID: start})
	options := &pebble.IterOptions{LowerBound: lower[:]}
	if ns < schema.Namespace(^uint16(0)) {
		var upper [2]byte
		binary.BigEndian.PutUint16(upper[:], uint16(ns)+1)
		options.UpperBound = upper[:]
	}
	it, err := s.db.NewIter(options)
	if err != nil {
		return err
	}
	for valid := it.First(); valid; valid = it.Next() {
		k, err := DecodeKey(it.Key())
		if err != nil {
			return errors.Join(err, it.Close())
		}
		if k.Namespace != ns {
			break
		}
		v, err := it.ValueAndErr()
		if err != nil {
			return errors.Join(err, it.Close())
		}
		more, err := visit(k, v)
		if err != nil {
			return errors.Join(err, it.Close())
		}
		if !more {
			break
		}
	}
	return errors.Join(it.Error(), it.Close())
}
