package bitmapidx

import (
	"encoding/binary"
	"errors"
	"hash/crc32"
	"slices"

	"github.com/evanoberholster/imageIdx/internal/persist"
)

const (
	fieldWALMagic      uint32 = 0x424d4657
	fieldWALHeaderSize        = 64
	// MaxFieldBatchUpdates counts canonical (value, ImageID) memberships,
	// not images. A move between two values counts twice.
	MaxFieldBatchUpdates = 4096
	// MaxFieldBatchBytes bounds encoded canonical values plus memberships.
	MaxFieldBatchBytes = 16 << 20
)

// ErrBatchTooLarge reports a batch exceeding MaxFieldBatchUpdates
// memberships or MaxFieldBatchBytes encoded bytes.
var ErrBatchTooLarge = errors.New("bitmapidx: atomic field batch exceeds persistence limits")

// WAL groups store canonical bytes, not analyzer source or reverse
// values, so replay never re-runs the analyzer. The group header is 24
// bytes: hash u64, value length u32, add count u32, remove count u32,
// reserved u32. Canonical bytes and ID columns follow, padded to 8 bytes.
// ID columns are sorted ascending with remove-wins already applied, so
// decode can validate ordering without re-sorting.
//
// The 64-byte frame header is: magic u32, version u16, field type u16,
// payload length u64, group count u32, update count u32, generation u64,
// offset u64, payload CRC u32, store ID [16]byte, header CRC u32 over the
// preceding 60 bytes. The store ID binds frames to the owning store so
// files mixed between a backup and restore are rejected.
func encodeFieldChanges(changes []stagedChange) ([]byte, uint32, error) {
	var payload []byte
	updates := 0
	for _, change := range changes {
		adds := dedupeSortedIDs(change.add)
		removes := dedupeSortedIDs(change.remove)
		adds = subtractSortedIDs(adds, removes)
		updates += len(adds) + len(removes)
		if updates > MaxFieldBatchUpdates {
			return nil, 0, ErrBatchTooLarge
		}
		size := 24 + len(change.canonical) + (len(adds)+len(removes))*8
		size = (size + 7) &^ 7
		if size > MaxFieldBatchBytes-len(payload) {
			return nil, 0, ErrBatchTooLarge
		}
		start := len(payload)
		payload = append(payload, make([]byte, size)...)
		group := payload[start:]
		binary.LittleEndian.PutUint64(group[0:8], change.base.Hash)
		binary.LittleEndian.PutUint32(group[8:12], uint32(len(change.canonical)))
		binary.LittleEndian.PutUint32(group[12:16], uint32(len(adds)))
		binary.LittleEndian.PutUint32(group[16:20], uint32(len(removes)))
		copy(group[24:], change.canonical)
		pos := 24 + len(change.canonical)
		for _, ids := range [][]ImageID{adds, removes} {
			for _, id := range ids {
				binary.LittleEndian.PutUint64(group[pos:pos+8], id)
				pos += 8
			}
		}
	}
	return payload, uint32(updates), nil
}

// append frames payload with magic, version, field type, byte/group/update
// counts, generation, offset, and CRCs, then appends it. Zero updates
// skip the WAL. DurabilitySync fsyncs per frame; other modes leave it to
// the OS and checkpoint Sync. Failures mark the store failed so later
// writes require recovery.
func (s *Index) append(index *FieldIndex, payload []byte, groups, updates uint32) error {
	if updates == 0 {
		return nil
	}
	start, err := s.wal.Offset()
	if err != nil {
		s.failed = errors.Join(ErrRecoveryRequired, err)
		return s.failed
	}
	var header [fieldWALHeaderSize]byte
	binary.LittleEndian.PutUint32(header[0:4], fieldWALMagic)
	binary.LittleEndian.PutUint16(header[4:6], 2)
	binary.LittleEndian.PutUint16(header[6:8], uint16(index.field().Type))
	binary.LittleEndian.PutUint64(header[8:16], uint64(len(payload)))
	binary.LittleEndian.PutUint32(header[16:20], groups)
	binary.LittleEndian.PutUint32(header[20:24], updates)
	binary.LittleEndian.PutUint64(header[24:32], s.generation)
	binary.LittleEndian.PutUint64(header[32:40], uint64(start))
	binary.LittleEndian.PutUint32(header[40:44], crc32.ChecksumIEEE(payload))
	copy(header[44:60], s.storeID[:])
	binary.LittleEndian.PutUint32(header[60:64], crc32.ChecksumIEEE(header[:60]))
	if err := s.wal.AppendFrame(header[:], payload, s.durability == DurabilitySync); err != nil {
		s.failed = errors.Join(ErrRecoveryRequired, err)
		return s.failed
	}
	return nil
}

// replay replays WAL frames into their fields and returns the valid tail
// end for RepairTail. It validates header CRCs, generation, offsets,
// counts, and payload CRCs. Changes apply with idempotent set semantics
// and no live geo preconditions, because the WAL already validated once;
// text needs no re-analysis since groups carry canonical bytes.
func (s *Index) replay(path string) (int64, error) {
	position := uint64(0)
	return persist.ReplayEnd(path, 0, persist.FrameSpec{HeaderSize: fieldWALHeaderSize, Magic: fieldWALMagic, Version: 2, Corrupt: ErrCorruptWAL, MaxPayload: MaxFieldBatchBytes}, func(header []byte) (uint64, error) {
		if crc32.ChecksumIEEE(header[:60]) != binary.LittleEndian.Uint32(header[60:64]) ||
			binary.LittleEndian.Uint64(header[24:32]) != s.generation || binary.LittleEndian.Uint64(header[32:40]) != position ||
			[16]byte(header[44:60]) != s.storeID {
			return 0, ErrCorruptWAL
		}
		updates := binary.LittleEndian.Uint32(header[20:24])
		groups := binary.LittleEndian.Uint32(header[16:20])
		if updates == 0 || updates > MaxFieldBatchUpdates || groups > MaxFieldBatchUpdates {
			return 0, ErrCorruptWAL
		}
		return binary.LittleEndian.Uint64(header[8:16]), nil
	}, func(header, payload []byte) error {
		if crc32.ChecksumIEEE(payload) != binary.LittleEndian.Uint32(header[40:44]) {
			return ErrCorruptWAL
		}
		t := IndexType(binary.LittleEndian.Uint16(header[6:8]))
		index, ok := s.fields[t]
		if !ok {
			return ErrCorruptWAL
		}
		changes, updates, err := decodeFieldChanges(index.field(), payload, binary.LittleEndian.Uint32(header[16:20]))
		if err != nil || updates != binary.LittleEndian.Uint32(header[20:24]) {
			return ErrCorruptWAL
		}
		// Prepared, canonical memberships are replayed without re-analyzing
		// text and without live geo preconditions (idempotent set semantics).
		if err := index.applyChangesReplay(changes); err != nil {
			return errors.Join(ErrCorruptWAL, err)
		}
		position += uint64(fieldWALHeaderSize + len(payload))
		return nil
	})
}

// decodeFieldChanges validates and decodes a WAL payload into staged
// changes. It checks group bounds, value lengths, hash agreement for
// hashed kinds, geo key validity, strictly increasing non-zero IDs, no
// add/remove overlap, and exact payload consumption.
func decodeFieldChanges(field Field, payload []byte, groups uint32) ([]stagedChange, uint32, error) {
	if uint64(groups)*24 > uint64(len(payload)) {
		return nil, 0, ErrCorruptWAL
	}
	changes := make([]stagedChange, 0, groups)
	var updates uint32
	for range groups {
		if len(payload) < 24 {
			return nil, 0, ErrCorruptWAL
		}
		hash := binary.LittleEndian.Uint64(payload[:8])
		n := uint64(binary.LittleEndian.Uint32(payload[8:12]))
		a := uint64(binary.LittleEndian.Uint32(payload[12:16]))
		r := uint64(binary.LittleEndian.Uint32(payload[16:20]))
		size := (24 + n + (a+r)*8 + 7) &^ uint64(7)
		if n > MaxValueBytes || a+r > MaxFieldBatchUpdates || size > uint64(len(payload)) {
			return nil, 0, ErrCorruptWAL
		}
		change := stagedChange{base: FieldKey{Type: field.Type, Hash: hash}, next: -1}
		if field.Kind == KindContinuous || field.Kind == KindGeo {
			if n != 0 {
				return nil, 0, ErrCorruptWAL
			}
			if field.Kind == KindGeo {
				if _, err := CellIDFromKey(hash); err != nil {
					return nil, 0, ErrCorruptWAL
				}
			}
		} else {
			change.canonical = append([]byte{}, payload[24:24+n]...)
			if hashCanonical(field.Type, field.Kind, change.canonical).Hash != hash {
				return nil, 0, ErrCorruptWAL
			}
		}
		pos := 24 + n
		for side, count := range []uint64{a, r} {
			ids := make([]ImageID, count)
			for j := range ids {
				ids[j] = binary.LittleEndian.Uint64(payload[pos : pos+8])
				pos += 8
				if ids[j] == 0 || (j > 0 && ids[j] <= ids[j-1]) {
					return nil, 0, ErrCorruptWAL
				}
			}
			if side == 0 {
				change.add = ids
			} else {
				change.remove = ids
			}
		}
		for _, id := range change.add {
			if _, ok := slices.BinarySearch(change.remove, id); ok {
				return nil, 0, ErrCorruptWAL
			}
		}
		updates += uint32(a + r)
		if updates > MaxFieldBatchUpdates {
			return nil, 0, ErrCorruptWAL
		}
		changes = append(changes, change)
		payload = payload[size:]
	}
	if len(payload) != 0 {
		return nil, 0, ErrCorruptWAL
	}
	return changes, updates, nil
}
