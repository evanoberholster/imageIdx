// Package radixhash implements a persistent, mmap-oriented exact hash index
// for fixed-width 256-bit hashes such as BLAKE3 and SHA-256.
//
// The index maps a 256-bit hash to one or more schema.ImageID values and allows
// duplicate hashes, because exact-content duplicates are a normal part of an
// image library. The design prioritizes SIMD-friendly lookup, lock-free
// readers, batched writes, copy-on-write updates, lazy allocation, and low
// write amplification.
//
// # Byte-oriented radix path
//
// The tree consumes one hash byte per level. Each radix node is exactly
//
//	type RadixNode [256]uint32
//
// which is 1024 bytes and therefore two 512-byte pagealloc blocks. A child
// value of zero means the child does not exist; otherwise the low bit tags the
// child as a leaf (0) or another radix node (1), and the remaining bits are the
// pagealloc block reference.
//
//	Hash[0] -> root radix
//	Hash[1] -> second-level radix (lazily allocated)
//	...
//	        -> leaf
//
// The root is a single 256-entry node, not a preallocated 65,536-entry
// directory, and second-level nodes are allocated only for hash[0] buckets that
// contain data.
//
// # Structure-of-arrays leaves
//
// The consumed radix prefix is implicit in the path, so a leaf stores only the
// unconsumed tail and the ImageIDs:
//
//	HashTail[i] = Hash[depth:32]
//	IndexID[i]  = uint64
//
// Slots are sorted by (tail, ImageID) so equal hashes stay contiguous and their
// IDs ascend. The header records the radix depth, so lookup knows how many
// leading bytes the path consumed.
//
// # Adaptive promotion
//
// When a leaf grows past Config.PromotionBytes it is replaced, in the same
// copy-on-write batch, by a radix node indexed by the next unconsumed byte and
// its records are distributed into child leaves. Further levels are promoted
// the same way if a child still exceeds the threshold. Promotion is per leaf,
// never global.
//
// # Copy-on-write publication
//
// A batch groups mutations by radix path and rewrites each affected leaf at
// most once. An affected leaf is merged directly from its encoded SoA form into
// the replacement extent, and only a promotion falls back to decoding. Every
// replacement leaf and interior node is built in a first phase; only then is
// the new root published and the replaced pages retired. A failure while
// building frees the fresh extents and leaves the published view untouched, so
// a mid-batch out-of-space can never retire a page the live view still
// references. Readers load the atomic view and only touch pages belonging to
// it.
//
// # Deletion
//
// Deletion is by ImageID and is not represented in the tree. Callers supply a
// liveness predicate (typically a roaring bitmap of deleted IDs) to
// LookupFiltered; physical records are removed by rebuilding the index.
//
// # Scope
//
// This package implements the allocator plus the byte-oriented radix engine,
// adaptive promotion, COW batch commits, and generation-pinned lock-free
// readers, a dual-superblock mmap arena with reopen/checkpoint, and NEON/AVX2
// tail kernels behind a scalar reference. WAL durability modes, shard
// rollover, and the unified index.Store integration are follow-ups described
// in AGENTS/ARCHITECTURE.md.
package radixhash

import (
	"errors"

	schema "github.com/evanoberholster/imageIdx/schema"
)

const (
	// hashBytes is the fixed hash width. Only 256-bit cryptographic hashes
	// are supported by this format.
	hashBytes = 32
	// radixFanout is the number of children in every radix node.
	radixFanout = 256
	// radixNodeBytes is the exact size of a radix node: 256 * uint32. It is
	// two pagealloc MinBlockSize blocks.
	radixNodeBytes = radixFanout * 4
	// maxDepth is the deepest radix path: every hash byte consumed.
	maxDepth = hashBytes
	// defaultPromotionBytes is the starting leaf promotion threshold. The
	// design calls for 64-128 KiB and requires benchmarking before it is
	// treated as a format constant.
	defaultPromotionBytes uint32 = 96 << 10
)

// Hash is a 256-bit content or perceptual digest.
type Hash [32]byte

// ImageID is the database-wide image identity returned by lookups.
type ImageID = schema.ImageID

// Entry is one logical (hash, image) pair.
type Entry struct {
	Hash    Hash
	ImageID ImageID
}

// Config controls runtime tree policy. PromotionBytes is the leaf size at
// which an individual leaf is promoted to another radix level; it is a runtime
// tuning value rather than a persistent format constant. MaxLeafSlots, when
// nonzero, is an additional promotion cap by record count so scan and rewrite
// cost stay bounded regardless of row width. AutoReclaimBytes, when nonzero,
// bounds retired space: after a publish whose retired bytes reach the threshold
// the writer drains prior readers and reclaims automatically.
type Config struct {
	PromotionBytes   uint32
	MaxLeafSlots     uint32
	AutoReclaimBytes uint64

	// forceRootRadix keeps the pre-sparse shape for layout comparison: every
	// populated Hash[0] bucket owns a Hash[1] radix node instead of a direct
	// Root->Leaf bucket. It is unexported and only used by benchmarks.
	forceRootRadix bool
}

// withDefaults fills zero values with the documented defaults.
func (c Config) withDefaults() Config {
	if c.PromotionBytes == 0 {
		c.PromotionBytes = defaultPromotionBytes
	}
	return c
}

// Metadata is a copied summary of the current published view.
type Metadata struct {
	// Generation increments by one on every published batch.
	Generation uint64
	// Entries is the number of live (hash, image) pairs.
	Entries uint64
	// RootBuckets is the number of non-empty root radix entries.
	RootBuckets uint64
	// Depth is the deepest leaf depth (number of consumed radix bytes).
	Depth uint8
	// ArenaBytes is the usable arena size after block rounding.
	ArenaBytes uint64
	// FreeBytes is the currently free arena capacity.
	FreeBytes uint64
	// RetiredExtents is the number of replaced extents awaiting reclamation.
	RetiredExtents int
}

// Sentinel errors.
var (
	// ErrClosed reports an operation on a closed index.
	ErrClosed = errors.New("radixhash: index is closed")
	// ErrInvalidEntry reports an entry with a reserved ImageID.
	ErrInvalidEntry = errors.New("radixhash: invalid entry")
	// ErrCorrupt reports a page whose header fails magic, version, depth, or
	// bounds validation.
	ErrCorrupt = errors.New("radixhash: corrupt page")
)
