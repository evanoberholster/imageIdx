// Package bitmapidx implements immutable, copy-on-write namespace indexes over
// database-wide image identifiers (common.ImageID).
//
// A NamespaceIndex owns all postings for one typed namespace. Values are
// canonicalized according to the persisted namespace definition and may be enum,
// bool, text, continuous, or geographic values. The dictionary maps hashed
// values to collision slots; postings map each value to the images holding
// it. A namespaceView is one atomically published snapshot of base postings,
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

	common "github.com/evanoberholster/imageIdx/common"
	"github.com/evanoberholster/imageIdx/internal/storage"
)

// ImageID is the database-wide identifier type shared by all index packages.
type ImageID = common.ImageID

// UserID is the database-wide user principal type shared by all index
// packages.
type UserID = common.UserID

// GroupID is the database-wide group principal type shared by all index
// packages.
type GroupID = common.GroupID

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
type Durability = common.Durability

const (
	DurabilityOS   = common.DurabilityOS
	DurabilitySync = common.DurabilitySync
)

// storeLockName is the never-unlinked lock file inside a store directory;
// every opener must lock the same inode.
const storeLockName = ".bitmapidx.lock"

var (
	// ErrStoreExists reports creating a store in a non-empty directory.
	ErrStoreExists = storage.ErrExists
	// ErrStoreBusy reports another process holding the store lock.
	ErrStoreBusy = storage.ErrBusy
	// ErrOwnedIndex reports Close on a store-owned NamespaceIndex. Close the
	// owning Index instead; it owns all child and mmap lifetimes.
	ErrOwnedIndex = errors.New("bitmapidx: close the owning store instead")
	// ErrRecoveryRequired reports a WAL failure that leaves the store
	// failed until recovery. It aliases storage.ErrWALFailed.
	ErrRecoveryRequired = storage.ErrWALFailed
)
