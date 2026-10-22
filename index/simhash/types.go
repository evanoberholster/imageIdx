package simhash

import (
	"errors"
	"unsafe"

	hashkernel "github.com/evanoberholster/imageIdx/index/simhash/internal/hashkernel"
	schema "github.com/evanoberholster/imageIdx/schema"
)

var (
	// ErrInvalidConfig reports an unusable configuration.
	ErrInvalidConfig = errors.New("simhash: invalid configuration")
	// ErrInvalidID reports the reserved zero ImageID or an ID beyond the
	// committed high-water mark.
	ErrInvalidID = errors.New("simhash: invalid image id")
	// ErrClosed reports an operation on a closed store.
	ErrClosed = errors.New("simhash: store is closed")
	// ErrCorrupt reports a corrupt manifest, shard, or superblock.
	ErrCorrupt = errors.New("simhash: corrupt store")
	// ErrBatchTooLarge reports an update batch above maxBatchRecords.
	ErrBatchTooLarge = errors.New("simhash: update batch too large")
	// ErrOverflow reports an arithmetic overflow that would wrap the committed
	// high-water mark or generation.
	ErrOverflow = errors.New("simhash: arithmetic overflow")
	// ErrUnsupportedSparseFormat reports the retired experimental sparse file
	// layout. The index must be rebuilt into the segmented sparse format.
	ErrUnsupportedSparseFormat = errors.New("simhash: unsupported legacy sparse format")
)

// PDQHash is a 256-bit PDQ perceptual hash. The persistent representation is
// exactly these 32 bytes; within a process the bytes are reinterpreted as four
// host-endian uint64 words for the XOR + population-count kernel.
type PDQHash [32]byte

// Record is one image's canonical similarity tuple.
type Record struct {
	// PHash is the 64-bit perceptual hash.
	PHash uint64
	// PDQ is the 256-bit PDQ hash.
	PDQ PDQHash
	// Quality is the PDQ quality byte.
	Quality uint8
}

// Update is one point update of an existing record. Batch updates are the
// expected workload.
type Update struct {
	ImageID schema.ImageID
	Record  Record
}

// Result is one exact similarity-search match. Distance is the exact Hamming
// distance: 0..64 for a pHash search and 0..MaxPDQSearchDistance (128) for a PDQ
// search. The caller knows which search produced the result, so no hash
// discriminator is stored.
type Result struct {
	ImageID  schema.ImageID
	Distance uint8
}

// MaxPDQSearchDistance bounds the supported PDQ similarity-search radius.
//
// PDQ is 256 bits, so the mathematical Hamming distance is 0..256, but useful
// similarity searches stay far below half the hash. The public PDQ search API
// therefore supports 0..128 and clamps a larger threshold to
// MaxPDQSearchDistance; an excessive threshold is not an error. Every returned
// PDQ distance is exact and, because distance <= threshold <= 128, fits in
// uint8 with no sentinel or saturation.
//
// This is a search radius, not the metric: DistancePDQ still returns the exact
// mathematical 0..256.
const MaxPDQSearchDistance uint8 = 128

// Config describes one store. The zero value of every field selects a default.
type Config struct {
	// MaxShardBytes caps each shard file. Zero selects 4 GiB.
	MaxShardBytes uint64
	// SeqBlockRecords is the seqlock granularity: records per sequence block.
	// It must be a power of two dividing 65,536. Zero selects 1024.
	SeqBlockRecords int
	// Durability controls whether a commit flushes the shard and manifest.
	Durability schema.Durability
}

func (c Config) withDefaults() (Config, error) {
	if c.MaxShardBytes == 0 {
		c.MaxShardBytes = defaultMaxShardBytes
	}
	if c.MaxShardBytes < minimumShardBytes {
		return Config{}, ErrInvalidConfig
	}
	if c.SeqBlockRecords == 0 {
		c.SeqBlockRecords = defaultSeqBlockRecords
	}
	if !validSeqBlockRecords(c.SeqBlockRecords) {
		return Config{}, ErrInvalidConfig
	}
	if !c.Durability.Valid() {
		return Config{}, ErrInvalidConfig
	}
	return c, nil
}

// hashWords reinterprets a PDQHash as four host-endian uint64 words.
func hashWords(h *PDQHash) *[4]uint64 { return (*[4]uint64)(unsafe.Pointer(h)) }

// DistancePHash returns the exact 64-bit Hamming distance between two pHashes.
// The range is 0..64, so uint8 holds it exactly.
func DistancePHash(a, b uint64) uint8 { return uint8(hashkernel.PHashDistance(a, b)) }

// DistancePDQ returns the exact 256-bit Hamming distance between two PDQ
// hashes. The mathematical range is 0..256, so the primitive stays uint16; the
// search API instead uses uint8 thresholds in 0..MaxPDQSearchDistance and
// returns exact uint8 Result distances.
func DistancePDQ(a, b *PDQHash) uint16 {
	return hashkernel.PDQDistance((*[32]byte)(unsafe.Pointer(a)), (*[32]byte)(unsafe.Pointer(b)))
}
