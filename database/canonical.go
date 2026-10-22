package database

import (
	"encoding/binary"
	"math"
)

const (
	canonicalMagic           uint8 = 0xD1
	canonicalVersionInline   uint8 = 1
	canonicalVersionExternal uint8 = 2
	canonicalHeader                = 7 // magic, flags, version, projection length
)

// CanonicalProjectionMode selects how the authoritative canonical value stores
// the application projection needed to derive future journal deltas and rebuild
// indexes. Inline is the compatibility/simple mode (strategy A); External stores
// only the application record and uses ProjectFromRecord to re-derive projection
// bytes (strategy B).
type CanonicalProjectionMode uint8

const (
	CanonicalProjectionInline CanonicalProjectionMode = iota
	CanonicalProjectionExternal
)

func (m CanonicalProjectionMode) valid() bool {
	return m == CanonicalProjectionInline || m == CanonicalProjectionExternal
}

type canonicalValue struct {
	tombstone  bool
	version    uint8
	projection []byte
	record     []byte
}

func encodeCanonicalValue(value canonicalValue) ([]byte, error) {
	if len(value.projection) > math.MaxUint32 {
		return nil, ErrInvalidMutation
	}
	flags := byte(0)
	if value.tombstone {
		flags = 1
	}
	out := make([]byte, canonicalHeader+len(value.projection)+len(value.record))
	out[0] = canonicalMagic
	out[1] = flags
	out[2] = value.version
	binary.BigEndian.PutUint32(out[3:7], uint32(len(value.projection)))
	copy(out[canonicalHeader:], value.projection)
	copy(out[canonicalHeader+len(value.projection):], value.record)
	return out, nil
}

func decodeCanonicalValue(raw []byte) (canonicalValue, error) {
	if len(raw) < canonicalHeader || raw[0] != canonicalMagic {
		return canonicalValue{}, ErrUnsupportedCanonicalVersion
	}
	flags, version := raw[1], raw[2]
	if flags > 1 {
		return canonicalValue{}, ErrCorruptJournal
	}
	if version != canonicalVersionInline && version != canonicalVersionExternal {
		return canonicalValue{}, ErrUnsupportedCanonicalVersion
	}
	n := uint64(binary.BigEndian.Uint32(raw[3:7]))
	if n > uint64(len(raw)-canonicalHeader) {
		return canonicalValue{}, ErrCorruptJournal
	}
	if version == canonicalVersionExternal && n != 0 {
		return canonicalValue{}, ErrCorruptJournal
	}
	value := canonicalValue{
		tombstone:  flags == 1,
		version:    version,
		projection: raw[canonicalHeader : canonicalHeader+int(n)],
		record:     raw[canonicalHeader+int(n):],
	}
	return value, nil
}

func (db *DB) projectionForValue(value canonicalValue) ([]byte, error) {
	if value.version == canonicalVersionInline {
		return value.projection, nil
	}
	if db.projectFromRecord == nil {
		return nil, ErrProjectionUnavailable
	}
	return db.projectFromRecord(value.record)
}

func canonicalVersionForMode(mode CanonicalProjectionMode) uint8 {
	if mode == CanonicalProjectionExternal {
		return canonicalVersionExternal
	}
	return canonicalVersionInline
}
