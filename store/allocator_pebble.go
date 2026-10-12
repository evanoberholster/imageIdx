package store

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"sync"

	"github.com/cockroachdb/pebble/v2"
	"github.com/evanoberholster/imageIdx/common"
	"github.com/evanoberholster/imageIdx/index/idalloc"
)

// Namespace zero is reserved for internal metadata. These 11-byte keys are
// disjoint from both 12-byte records and the journal (namespace 65533), and
// never appear in scans of any registered user namespace, including 65535.
var metadataPrefix = [...]byte{0, 0, 'i', 'd', 'a', 'l', 'l', 'o', 'c', '/'}

// A second marker in the pre-existing internal namespace distinguishes a
// legacy store from an initialized store whose allocator keys were erased.
var initializationWitness = EncodeKey(Key{Namespace: 0, ID: 1})

const (
	metadataVersion       byte = 2
	legacyMetadataVersion byte = 1
	legacyEntityCount          = 10
)

func metadataKey(kind common.EntityKind) [11]byte {
	var key [11]byte
	copy(key[:], metadataPrefix[:])
	key[10] = byte(kind)
	return key
}

func encodeWatermark(kind common.EntityKind, high uint64) [14]byte {
	var value [14]byte
	value[0] = metadataVersion
	value[1] = byte(kind)
	binary.BigEndian.PutUint64(value[2:10], high)
	binary.BigEndian.PutUint32(value[10:], crc32.ChecksumIEEE(value[:10]))
	return value
}

func decodeWatermark(kind common.EntityKind, value []byte) (uint64, error) {
	if len(value) != 14 || value[0] != metadataVersion || value[1] != byte(kind) ||
		binary.BigEndian.Uint32(value[10:]) != crc32.ChecksumIEEE(value[:10]) {
		return 0, fmt.Errorf("idalloc: corrupt watermark for %s", kind)
	}
	return binary.BigEndian.Uint64(value[2:10]), nil
}

var (
	ErrUninitializedReservations = errors.New("idalloc: allocator metadata missing; explicit migration required")
	ErrLegacyReservations        = errors.New("idalloc: allocator entity schema requires explicit upgrade")
)

// ReservationStore adapts a caller-owned, exclusively opened Pebble database.
// InitializeReservations must have completed before opening this adapter. The
// database must remain open until Allocator.Close returns.
type ReservationStore struct {
	db *pebble.DB
	mu sync.Mutex
}

// InitializeReservations explicitly initializes a NEW database or completes an
// externally verified legacy migration. The caller must scan canonical AND
// tombstone keys to supply the per-entity maxima, while holding exclusive
// ownership of the database. It refuses to reset existing metadata. The
// complete set of watermarks and format marker commits in one synced batch.
// An empty/new database can pass a zero-filled maxima array.
func (s *Store) InitializeReservations(maxima [common.EntityKindCount + 1]uint64) error {
	db := s.db
	if db == nil {
		return errors.New("idalloc: nil Pebble database")
	}
	_, witnessCloser, witnessErr := db.Get(initializationWitness[:])
	if witnessErr == nil {
		_ = witnessCloser.Close()
		return errors.New("idalloc: allocator already initialized")
	}
	if !errors.Is(witnessErr, pebble.ErrNotFound) {
		return witnessErr
	}
	marker := metadataKey(common.EntityInvalid)
	_, closer, err := db.Get(marker[:])
	if err == nil {
		_ = closer.Close()
		return errors.New("idalloc: metadata already initialized")
	}
	if !errors.Is(err, pebble.ErrNotFound) {
		return err
	}
	// Partial metadata without the marker is corruption, never a new store.
	for n := 1; n <= common.EntityKindCount; n++ {
		key := metadataKey(common.EntityKind(n))
		_, closer, err := db.Get(key[:])
		if err == nil {
			_ = closer.Close()
			return errors.New("idalloc: partial allocator metadata")
		}
		if !errors.Is(err, pebble.ErrNotFound) {
			return err
		}
	}
	batch := db.NewBatch()
	defer batch.Close()
	for n := 1; n <= common.EntityKindCount; n++ {
		kind := common.EntityKind(n)
		key := metadataKey(kind)
		watermark := encodeWatermark(kind, maxima[n])
		if err := batch.Set(key[:], watermark[:], nil); err != nil {
			return err
		}
	}
	if err := batch.Set(marker[:], []byte{metadataVersion, byte(common.EntityKindCount)}, nil); err != nil {
		return err
	}
	if err := batch.Set(initializationWitness[:], []byte{metadataVersion}, nil); err != nil {
		return err
	}
	return batch.Commit(pebble.Sync)
}

func (s *Store) OpenReservations() (*ReservationStore, error) {
	db := s.db
	if db == nil {
		return nil, errors.New("idalloc: nil Pebble database")
	}
	marker := metadataKey(common.EntityInvalid)
	witness, witnessCloser, witnessErr := db.Get(initializationWitness[:])
	if errors.Is(witnessErr, pebble.ErrNotFound) {
		return nil, ErrUninitializedReservations
	}
	if witnessErr != nil {
		return nil, witnessErr
	}
	witnessValid := len(witness) == 1 && (witness[0] == metadataVersion || witness[0] == legacyMetadataVersion)
	witnessVersion := byte(0)
	if witnessValid {
		witnessVersion = witness[0]
	}
	if err := witnessCloser.Close(); err != nil {
		return nil, err
	}
	if !witnessValid {
		return nil, errors.New("idalloc: corrupt initialization witness")
	}
	value, closer, err := db.Get(marker[:])
	if errors.Is(err, pebble.ErrNotFound) {
		return nil, errors.New("idalloc: missing allocator format marker")
	}
	if err != nil {
		return nil, err
	}
	valid := len(value) == 2 && value[0] == metadataVersion && value[1] == byte(common.EntityKindCount) && witnessVersion == metadataVersion
	legacy := len(value) == 2 && value[0] == legacyMetadataVersion && value[1] == legacyEntityCount && witnessVersion == legacyMetadataVersion
	if err := closer.Close(); err != nil {
		return nil, err
	}
	if !valid {
		if legacy {
			return nil, ErrLegacyReservations
		}
		return nil, errors.New("idalloc: unsupported or corrupt allocator format")
	}
	reservations := &ReservationStore{db: db}
	for n := 1; n <= common.EntityKindCount; n++ {
		if _, err := reservations.LoadReservedThrough(common.EntityKind(n)); err != nil {
			return nil, err
		}
	}
	return reservations, nil
}

// UpgradeReservations performs the explicit v1 (ten kinds) to v2 (thirteen
// kinds) upgrade. maxima must include *all* canonical keys and tombstones for
// each kind. The old reservations remain authoritative even when larger than
// the surviving records. One synced batch publishes all keys and the marker.
func (s *Store) UpgradeReservations(maxima [common.EntityKindCount + 1]uint64) error {
	marker := metadataKey(common.EntityInvalid)
	value, closer, err := s.db.Get(marker[:])
	if err != nil {
		return err
	}
	legacy := len(value) == 2 && value[0] == legacyMetadataVersion && value[1] == legacyEntityCount
	if err := closer.Close(); err != nil {
		return err
	}
	if !legacy {
		return ErrLegacyReservations
	}
	batch := s.db.NewBatch()
	defer batch.Close()
	for n := 1; n <= common.EntityKindCount; n++ {
		kind := common.EntityKind(n)
		key := metadataKey(kind)
		high := maxima[n]
		if n <= legacyEntityCount {
			v, c, err := s.db.Get(key[:])
			if err != nil {
				return fmt.Errorf("idalloc: missing legacy watermark for %s: %w", kind, err)
			}
			old, decodeErr := decodeLegacyWatermark(kind, v)
			closeErr := c.Close()
			if decodeErr != nil {
				return decodeErr
			}
			if closeErr != nil {
				return closeErr
			}
			if old > high {
				high = old
			}
		} else {
			_, c, err := s.db.Get(key[:])
			if err == nil {
				_ = c.Close()
				return errors.New("idalloc: unexpected new-kind watermark in legacy format")
			}
			if !errors.Is(err, pebble.ErrNotFound) {
				return err
			}
		}
		encoded := encodeWatermark(kind, high)
		if err := batch.Set(key[:], encoded[:], nil); err != nil {
			return err
		}
	}
	if err := batch.Set(marker[:], []byte{metadataVersion, byte(common.EntityKindCount)}, nil); err != nil {
		return err
	}
	if err := batch.Set(initializationWitness[:], []byte{metadataVersion}, nil); err != nil {
		return err
	}
	return batch.Commit(pebble.Sync)
}

func decodeLegacyWatermark(kind common.EntityKind, value []byte) (uint64, error) {
	if len(value) != 14 || value[0] != legacyMetadataVersion || value[1] != byte(kind) ||
		binary.BigEndian.Uint32(value[10:]) != crc32.ChecksumIEEE(value[:10]) {
		return 0, fmt.Errorf("idalloc: corrupt legacy watermark for %s", kind)
	}
	return binary.BigEndian.Uint64(value[2:10]), nil
}

func (s *ReservationStore) LoadReservedThrough(kind common.EntityKind) (uint64, error) {
	if !kind.Valid() {
		return 0, idalloc.ErrKind
	}
	key := metadataKey(kind)
	value, closer, err := s.db.Get(key[:])
	if errors.Is(err, pebble.ErrNotFound) {
		return 0, fmt.Errorf("idalloc: missing watermark for %s", kind)
	}
	if err != nil {
		return 0, err
	}
	high, decodeErr := decodeWatermark(kind, value)
	closeErr := closer.Close()
	if decodeErr != nil {
		return 0, decodeErr
	}
	return high, closeErr
}

// ReserveThrough is monotone, idempotent, and commits with Pebble.Sync.
func (s *ReservationStore) ReserveThrough(kind common.EntityKind, newHigh uint64) error {
	if !kind.Valid() {
		return idalloc.ErrKind
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	old, err := s.LoadReservedThrough(kind)
	if err != nil {
		return err
	}
	if newHigh < old {
		return fmt.Errorf("idalloc: cannot lower %s watermark from %d to %d", kind, old, newHigh)
	}
	if newHigh == old {
		return nil
	}
	key := metadataKey(kind)
	value := encodeWatermark(kind, newHigh)
	return s.db.Set(key[:], value[:], pebble.Sync)
}
