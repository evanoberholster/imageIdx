package database

import (
	"encoding/binary"
	"fmt"
	"math"

	schema "github.com/evanoberholster/imageIdx/schema"
)

const (
	canonicalMagic           uint8 = 0xD1
	canonicalVersionInline   uint8 = 1
	canonicalVersionExternal uint8 = 2
	canonicalHeader                = 7 // magic, flags, version, projection length
	databaseConfigVersion          = 1
)

// CanonicalProjectionMode selects how the authoritative canonical value stores
// the application projection needed to derive future journal deltas and rebuild
// indexes. Inline is the compatibility/simple mode (strategy A); External stores
// only the application record and uses ProjectFromRecord/ProjectInto to
// re-derive projection bytes (strategy B).
type CanonicalProjectionMode uint8

const (
	// CanonicalProjectionUnset asks Open to use the durable mode, or inline
	// when creating an unconfigured database.
	CanonicalProjectionUnset CanonicalProjectionMode = iota
	CanonicalProjectionInline
	CanonicalProjectionExternal
)

func (m CanonicalProjectionMode) valid() bool {
	return m == CanonicalProjectionUnset || m == CanonicalProjectionInline || m == CanonicalProjectionExternal
}

func canonicalConfigForMode(mode CanonicalProjectionMode) [6]byte {
	return [6]byte{databaseConfigVersion, byte(mode)}
}

func canonicalModeFromConfig(config [6]byte) (CanonicalProjectionMode, bool, error) {
	if config == ([6]byte{}) {
		return CanonicalProjectionUnset, false, nil
	}
	if config[0] != databaseConfigVersion || config[2] != 0 || config[3] != 0 || config[4] != 0 || config[5] != 0 {
		return CanonicalProjectionUnset, true, ErrCorruptDatabaseConfig
	}
	mode := CanonicalProjectionMode(config[1])
	if mode != CanonicalProjectionInline && mode != CanonicalProjectionExternal {
		return CanonicalProjectionUnset, true, ErrCorruptDatabaseConfig
	}
	return mode, true, nil
}

func ensureCanonicalMode(ids *schema.Schema, options Options) (Options, error) {
	mode, configured, err := canonicalModeFromConfig(ids.Config())
	if err != nil {
		return options, err
	}
	if configured {
		if options.CanonicalProjection != CanonicalProjectionUnset && options.CanonicalProjection != mode {
			return options, fmt.Errorf("%w: persisted %d requested %d", ErrCanonicalModeMismatch, mode, options.CanonicalProjection)
		}
		if mode == CanonicalProjectionExternal && options.ProjectFromRecord == nil && options.ProjectInto == nil {
			return options, ErrProjectionUnavailable
		}
		options.CanonicalProjection = mode
		return options, nil
	}
	if options.CanonicalProjection == CanonicalProjectionUnset {
		options.CanonicalProjection = CanonicalProjectionInline
	}
	if options.CanonicalProjection == CanonicalProjectionExternal && options.ProjectFromRecord == nil && options.ProjectInto == nil {
		return options, ErrProjectionUnavailable
	}
	if err := ids.SetConfig(canonicalConfigForMode(options.CanonicalProjection)); err != nil {
		return options, err
	}
	return options, nil
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
	return db.projectRecord(value.record, nil)
}

func (db *DB) projectRecord(record, dst []byte) ([]byte, error) {
	if db.projectInto != nil {
		return db.projectInto(dst, record)
	}
	if db.projectFromRecord == nil {
		return nil, ErrProjectionUnavailable
	}
	projection, err := db.projectFromRecord(record)
	if err != nil {
		return nil, err
	}
	if dst == nil {
		return projection, nil
	}
	return append(dst[:0], projection...), nil
}

func canonicalVersionForMode(mode CanonicalProjectionMode) uint8 {
	if mode == CanonicalProjectionExternal {
		return canonicalVersionExternal
	}
	return canonicalVersionInline
}
