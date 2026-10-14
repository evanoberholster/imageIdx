package store

// This file exists only to migrate databases written by the former Pebble
// entity allocator. New ID allocation is exclusively owned by schema.Definitions.

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"

	"github.com/cockroachdb/pebble/v2"
	schema "github.com/evanoberholster/imageIdx/schema"
)

var legacyPrefix = [...]byte{0, 0, 'i', 'd', 'a', 'l', 'l', 'o', 'c', '/'}
var legacyWitness = EncodeKey(Key{Namespace: 0, ID: 1})
var schemaMigrationKey = EncodeKey(Key{Namespace: 0, ID: 2})

func legacyKey(kind schema.EntityKind) [11]byte {
	var key [11]byte
	copy(key[:], legacyPrefix[:])
	key[10] = byte(kind)
	return key
}

func (s *Store) SchemaMigrationComplete() (bool, error) {
	v, closer, err := s.db.Get(schemaMigrationKey[:])
	if errors.Is(err, pebble.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	valid := bytes.Equal(v, []byte("schema-v1"))
	if err := closer.Close(); err != nil {
		return false, err
	}
	if !valid {
		return false, errors.New("store: corrupt schema migration marker")
	}
	return true, nil
}

// LegacyReservations reads *durable reservation high-water marks*, never
// surviving record maxima alone. Any partially present legacy metadata is an
// error; resetting a watermark would risk reissuing an uncommitted ID.
func (s *Store) LegacyReservations() (high [schema.EntityKindCount + 1]uint64, present bool, err error) {
	witness, closer, err := s.db.Get(legacyWitness[:])
	if errors.Is(err, pebble.ErrNotFound) {
		marker := legacyKey(schema.EntityInvalid)
		if _, c, e := s.db.Get(marker[:]); e == nil {
			_ = c.Close()
			return high, false, errors.New("store: legacy marker without witness")
		} else if !errors.Is(e, pebble.ErrNotFound) {
			return high, false, e
		}
		for n := 1; n <= schema.EntityKindCount; n++ {
			key := legacyKey(schema.EntityKind(n))
			if _, c, e := s.db.Get(key[:]); e == nil {
				_ = c.Close()
				return high, false, errors.New("store: legacy watermark without witness")
			} else if !errors.Is(e, pebble.ErrNotFound) {
				return high, false, e
			}
		}
		return high, false, nil
	}
	if err != nil {
		return high, false, err
	}
	if len(witness) != 1 || witness[0] < 1 || witness[0] > 2 {
		_ = closer.Close()
		return high, true, errors.New("store: corrupt legacy witness")
	}
	version := witness[0]
	if err := closer.Close(); err != nil {
		return high, true, err
	}
	marker := legacyKey(schema.EntityInvalid)
	v, c, err := s.db.Get(marker[:])
	if err != nil {
		return high, true, fmt.Errorf("store: missing legacy allocator marker: %w", err)
	}
	count := schema.EntityKindCount
	if version == 1 {
		count = 10
	}
	valid := len(v) == 2 && v[0] == version && int(v[1]) == count
	if err := c.Close(); err != nil {
		return high, true, err
	}
	if !valid {
		return high, true, errors.New("store: corrupt legacy allocator marker")
	}
	for n := 1; n <= count; n++ {
		key := legacyKey(schema.EntityKind(n))
		v, c, err := s.db.Get(key[:])
		if err != nil {
			return high, true, fmt.Errorf("store: missing legacy watermark for %d: %w", n, err)
		}
		valid := len(v) == 14 && v[0] == version && v[1] == byte(n) &&
			binary.BigEndian.Uint32(v[10:]) == crc32.ChecksumIEEE(v[:10])
		if valid {
			high[n] = binary.BigEndian.Uint64(v[2:10])
		}
		if err := c.Close(); err != nil {
			return high, true, err
		}
		if !valid {
			return high, true, fmt.Errorf("store: corrupt legacy watermark for %d", n)
		}
	}
	if version == 1 {
		for n := count + 1; n <= schema.EntityKindCount; n++ {
			key := legacyKey(schema.EntityKind(n))
			if _, c, e := s.db.Get(key[:]); e == nil {
				_ = c.Close()
				return high, true, errors.New("store: unexpected new-kind watermark in legacy format")
			} else if !errors.Is(e, pebble.ErrNotFound) {
				return high, true, e
			}
		}
	}
	return high, true, nil
}

// CompleteSchemaMigration publishes the authoritative migration marker and
// removes obsolete legacy metadata in one synced Pebble batch. Call only after
// every high-water/floor has been durably transferred to schema.Definitions.
func (s *Store) CompleteSchemaMigration() error {
	b := s.db.NewBatch()
	defer b.Close()
	if err := b.Set(schemaMigrationKey[:], []byte("schema-v1"), nil); err != nil {
		return err
	}
	key := legacyKey(schema.EntityInvalid)
	if err := b.Delete(key[:], nil); err != nil {
		return err
	}
	if err := b.Delete(legacyWitness[:], nil); err != nil {
		return err
	}
	for n := 1; n <= schema.EntityKindCount; n++ {
		key := legacyKey(schema.EntityKind(n))
		if err := b.Delete(key[:], nil); err != nil {
			return err
		}
	}
	return b.Commit(pebble.Sync)
}
