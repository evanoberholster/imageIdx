package database

import (
	"encoding/binary"
	"hash/crc32"
	"math"

	schema "github.com/evanoberholster/imageIdx/schema"
)

// Journal payload v1: [version:1][op:1][entity:1][id:8][beforeLen:4]
// [afterLen:4][before][after][crc32:4]. The ordered sequence is in the
// store.Key (namespace 65533, big-endian uint64 ID, flags 0), not the value.
type journalEntry struct {
	entity        schema.EntityKind
	id            schema.ValueID
	deleted       bool
	before, after []byte
}

const journalHeader = 19

func encodeMutation(m Mutation, before []byte) ([]byte, error) {
	if len(before) > math.MaxUint32 || len(m.Projection) > math.MaxUint32 {
		return nil, ErrInvalidMutation
	}
	if m.Delete {
		m.Projection = nil
	}
	b := make([]byte, journalHeader+len(before)+len(m.Projection)+4)
	b[0] = 1
	if m.Delete {
		b[1] = 1
	}
	b[2] = byte(m.Entity)
	binary.BigEndian.PutUint64(b[3:11], m.ValueID)
	binary.BigEndian.PutUint32(b[11:15], uint32(len(before)))
	binary.BigEndian.PutUint32(b[15:19], uint32(len(m.Projection)))
	copy(b[journalHeader:], before)
	copy(b[journalHeader+len(before):], m.Projection)
	binary.BigEndian.PutUint32(b[len(b)-4:], crc32.ChecksumIEEE(b[:len(b)-4]))
	return b, nil
}

func decodeMutation(b []byte) (journalEntry, error) {
	if len(b) < journalHeader+4 || b[0] != 1 || b[1] > 1 ||
		binary.BigEndian.Uint32(b[len(b)-4:]) != crc32.ChecksumIEEE(b[:len(b)-4]) {
		return journalEntry{}, ErrCorruptJournal
	}
	entity := schema.EntityKind(b[2])
	id := binary.BigEndian.Uint64(b[3:11])
	before, after := uint64(binary.BigEndian.Uint32(b[11:15])), uint64(binary.BigEndian.Uint32(b[15:19]))
	if !entity.Valid() || id == 0 || before+after != uint64(len(b)-journalHeader-4) || (b[1] == 1 && after != 0) || (b[1] == 0 && after == 0) {
		return journalEntry{}, ErrCorruptJournal
	}
	return journalEntry{entity: entity, id: id, deleted: b[1] == 1,
		before: b[journalHeader : journalHeader+int(before)],
		after:  b[journalHeader+int(before) : len(b)-4]}, nil
}
