// Package bitmapidx implements immutable, copy-on-write field indexes over
// database-wide image identifiers (imageidx.ImageID).
//
// A FieldIndex owns all postings for one typed field. Values are
// canonicalized according to the persisted field definition and may be enum,
// bool, text, continuous, or geographic values. The dictionary maps hashed
// values to collision slots; postings map each value to the images holding
// it. A fieldView is one atomically published snapshot of base postings,
// frozen/active deltas, and the dictionary.
//
// Concurrency: readers load one published view and take no locks; writers
// serialize through one mutex per index (plus one WAL mutex per store).
// Persistence: the Index checkpoints immutable segments over mmap and
// replays a WAL tail. Reads stay lock-free across checkpoints.
//
// Engineering priorities, in order: correctness, predictable low allocation,
// query/read performance, simple architecture, storage efficiency. Prefer
// shared immutable structures, pooled query scratch, and zero-copy mmap
// reads on the hot path.
package bitmapidx

import (
	"errors"

	imageidx "github.com/evanoberholster/imageIdx"
)

// ImageID is the database-wide identifier type shared by all index packages.
type ImageID = imageidx.ImageID

// IndexType identifies a database field (location, person, album, and so
// on). The concrete registry is database-defined. Zero is reserved and
// never a valid field.
type IndexType uint16

const (
	IndexTypeLocation IndexType = iota + 1
	IndexTypePerson
	IndexTypeAlbum
	IndexTypeTag
	IndexTypeObject
	IndexTypeEvent

	// v3 additions
	IndexTypeRating
	IndexTypeColorLabel
	IndexTypeFlag
)

var (
	// ErrClosed reports a write or maintenance operation on a closed index
	// or store. Reads against a closed index report empty results.
	ErrClosed = errors.New("bitmapidx: index is closed")
	// ErrCorruptSegment reports a segment that fails magic, version,
	// checksum, ordering, or bounds validation.
	ErrCorruptSegment = errors.New("bitmapidx: corrupt segment")
	// ErrCorruptManifest reports a manifest that fails magic, version, or
	// checksum validation.
	ErrCorruptManifest = errors.New("bitmapidx: corrupt manifest")
	// ErrCorruptWAL reports a WAL frame that fails magic, generation,
	// offset, count, or checksum validation.
	ErrCorruptWAL = errors.New("bitmapidx: corrupt write-ahead log")
	// ErrInvalidConfig reports a negative, inverted, or otherwise
	// unusable runtime configuration.
	ErrInvalidConfig = errors.New("bitmapidx: invalid configuration")
)

// Durability controls persistent WAL synchronization. DurabilityOS returns
// once the frame is visible to the OS; DurabilitySync fsyncs before
// returning. It is a store-wide policy, not part of the persisted format.
type Durability = imageidx.Durability

const (
	DurabilityOS   = imageidx.DurabilityOS
	DurabilitySync = imageidx.DurabilitySync
)
