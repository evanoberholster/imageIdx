package hashidx

import (
	"crypto/sha256"
	"errors"
	"io"
	"math/bits"

	imageidx "github.com/evanoberholster/imageIdx"
	"github.com/evanoberholster/imageIdx/internal/persist"
	"github.com/zeebo/blake3"
)

// ImageID is the database-wide identifier shared by all index packages.
type ImageID = imageidx.ImageID

// Durability controls when a write is acknowledged relative to filesystem
// synchronization.
type Durability = imageidx.Durability

const (
	DurabilityOS   = imageidx.DurabilityOS
	DurabilitySync = imageidx.DurabilitySync
)

var (
	ErrInvalidConfig = errors.New("hashidx: invalid configuration")
	ErrInvalidEntry  = errors.New("hashidx: invalid entry")
	ErrClosed        = errors.New("hashidx: index is closed")
	ErrCorrupt       = errors.New("hashidx: corrupt index")
	ErrCorruptWAL    = errors.New("hashidx: corrupt write-ahead log")
	ErrUnsupported   = errors.New("hashidx: unsupported format")
	// ErrRecoveryRequired reports a WAL failure that latched the index:
	// every later write and maintenance call fails with it until the
	// index is closed and reopened. It aliases persist.ErrWALFailed.
	// Recovery is close plus reopen: replay restores the last durable
	// prefix and reports a torn tail as a clean stop or ErrCorruptWAL.
	ErrRecoveryRequired = persist.ErrWALFailed
)

// Ordinal is an internal physical position in the shared SoA space. Ordinals
// are not stable across Vacuum and are never persisted as logical identity.
type Ordinal uint64

// PDQHash is a 256-bit PDQ perceptual hash stored as four little-endian
// uint64 words. Persistent order is W0, W1, W2, W3 and never depends on Go
// struct layout.
type PDQHash [4]uint64

// Entry is one image's canonical perceptual-hash record.
type Entry struct {
	ImageID ImageID
	PHash64 uint64
	PDQ     PDQHash
}

// Result is one search match.
type Result struct {
	ImageID  ImageID
	Distance uint16
}

// CandidateSet admits records at search time. Implementations commonly wrap a
// live/candidate bitmap. A nil CandidateSet means "no filtering". Because
// imageidx.ImageID is an alias of uint64, a roar.Bitmap satisfies this
// interface directly.
type CandidateSet interface {
	Contains(id ImageID) bool
}

// DefaultRecordsPerPage is the default number of 48-byte records in one
// logical page (8192 records, 384 KiB of SoA planes).
const DefaultRecordsPerPage uint32 = 8192

// DefaultMaxApplyEntries bounds one perceptual Apply batch by default:
// 65536 records frame to ~3 MiB of WAL payload and publish once. It is
// larger than the exact default because page appends scale better per
// entry than block rewrites. Provisional: tuned on Apple Silicon; see
// BenchmarkApply10000 before changing it.
const DefaultMaxApplyEntries uint32 = 65536

// maxBatchEntriesLimit caps configured batch sizes so one framed WAL
// record stays below maxWALFrameBytes on every platform, including
// 32-bit int arithmetic in the append paths.
const maxBatchEntriesLimit = 1 << 20

const (
	minRecordsPerPage = 8
	maxRecordsPerPage = 1 << 20
)

// Config controls the logical page size and write batching. The zero
// value selects defaults. RecordsPerPage is a persistent format property:
// the store records it in the perceptual manifest, and OpenStore adopts
// the persisted size. MaxBatchEntries is runtime policy only and is never
// persisted: it bounds one Apply batch so a single call cannot frame an
// unbounded WAL record and stall publication.
type Config struct {
	RecordsPerPage  uint32
	MaxBatchEntries uint32
}

// Metadata is a copied summary of the current published view. PageCount
// counts sealed pages plus an in-progress final page when one is active.
type Metadata struct {
	Generation     uint64
	RecordCount    uint64
	PageCount      uint64
	RecordsPerPage uint32
}

func (c Config) withDefaults() (Config, error) {
	if c.RecordsPerPage == 0 {
		c.RecordsPerPage = DefaultRecordsPerPage
	}
	// The multiple-of-8 rule keeps every plane's full-page span a multiple
	// of 64 bytes (8 uint64 words), preserving the 64-byte page alignment
	// the persisted layout promises.
	if c.RecordsPerPage < minRecordsPerPage || c.RecordsPerPage > maxRecordsPerPage || c.RecordsPerPage%8 != 0 {
		return Config{}, ErrInvalidConfig
	}
	if c.MaxBatchEntries == 0 {
		c.MaxBatchEntries = DefaultMaxApplyEntries
	}
	// Bound batches so one framed WAL record (entries × 48 bytes) stays
	// far below allocation-panic territory; the append path enforces the
	// byte-exact limit.
	if c.MaxBatchEntries > maxBatchEntriesLimit {
		return Config{}, ErrInvalidConfig
	}
	return c, nil
}

// pHashDistance is the scalar reference for exact 64-bit Hamming distance.
func pHashDistance(a, b uint64) uint8 {
	return uint8(bits.OnesCount64(a ^ b))
}

// pdqDistance is the scalar reference for exact 256-bit Hamming distance.
func pdqDistance(a, b PDQHash) uint16 {
	return uint16(bits.OnesCount64(a[0]^b[0]) +
		bits.OnesCount64(a[1]^b[1]) +
		bits.OnesCount64(a[2]^b[2]) +
		bits.OnesCount64(a[3]^b[3]))
}

// Algorithm identifies which 256-bit digest function an ExactIndex holds. One
// ExactIndex contains exactly one algorithm; it is persisted in the manifest
// and generation header and is authoritative on open.
type Algorithm uint8

const (
	AlgorithmSHA256 Algorithm = iota + 1
	AlgorithmBLAKE3
)

// Valid reports whether a is a recognized algorithm.
func (a Algorithm) Valid() bool { return a == AlgorithmSHA256 || a == AlgorithmBLAKE3 }

// String returns a short human-readable algorithm name.
func (a Algorithm) String() string {
	switch a {
	case AlgorithmSHA256:
		return "sha256"
	case AlgorithmBLAKE3:
		return "blake3"
	default:
		return "unknown"
	}
}

// Hash is a 256-bit content digest. The persistent representation is exactly
// these 32 bytes; hashes are never stored as strings or hex and digest words
// are never byte-swapped for ordering. Sort order is lexicographic byte
// order, matching bytes.Compare, and is stable across architectures.
type Hash [32]byte

// ExactEntry is one logical (Hash, ImageID) pair.
type ExactEntry struct {
	Hash    Hash
	ImageID ImageID
}

// ExactSpec describes an index's stable identity, algorithm, and initial entries.
type ExactSpec struct {
	IndexID   uint64
	Flags     uint32
	Algorithm Algorithm
	Entries   []ExactEntry
}

// ExactMetadata is a copied summary of the current generation. BaseCount
// counts base block records and deliberately excludes staged overlay
// corrections; OverlayCount counts staged entries not yet folded. The
// effective live count is BaseCount plus staged adds minus staged
// removes (see LiveCount).
type ExactMetadata struct {
	IndexID      uint64
	Flags        uint32
	Algorithm    Algorithm
	Generation   uint64
	BaseCount    uint64
	BlockCount   uint32
	OverlayCount uint64
}

// ExactConfig controls block sizing and write batching. The zero value selects
// benchmarked defaults.
type ExactConfig struct {
	// BlockSize is the nominal block allocation in bytes, including the
	// 32-byte block header. It determines the maximum records per block.
	BlockSize uint32
	// TargetFillPct is the post-split fill target in percent (1-100).
	TargetFillPct uint8
	// MaxBatchEntries bounds the number of logical entries (adds plus
	// removes) accepted by one Apply call.
	MaxBatchEntries uint32
	// OverlayMaxEntries bounds staged overlay entries (adds plus
	// removes) before a publish folds into the blocks. Small publishes
	// stage and share the block slice; past this bound the batch folds
	// synchronously, amortizing the O(#blocks) directory copy. It is
	// runtime policy only and is never persisted.
	OverlayMaxEntries uint32
}

const (
	// DefaultBlockSize is the baseline nominal block size (16 KiB).
	DefaultBlockSize = 16 * 1024
	// DefaultTargetFillPct is the post-split occupancy target.
	DefaultTargetFillPct = 80
	// DefaultMaxBatchEntries bounds one Apply batch by default. It is
	// smaller than the perceptual default because exact publishes rewrite
	// blocks per affected key range rather than appending pages.
	DefaultMaxBatchEntries = 8_192
	// DefaultOverlayMaxEntries stages up to 512 entries (~20 KiB) before
	// folding. Overlay probes stay within a 9-step binary search, keeping
	// steady-state reads near base cost while small publishes avoid the
	// directory copy entirely. Provisional: tuned on Apple Silicon; see
	// BenchmarkPublishScalingVsBlocks before changing it.
	DefaultOverlayMaxEntries = 512
)

var (
	ErrInvalidAlgorithm = errors.New("hashidx: invalid algorithm")
	ErrDuplicateEntry   = errors.New("hashidx: duplicate entry")
	ErrBatchTooLarge    = errors.New("hashidx: write batch exceeds limit")
)

func (c ExactConfig) withDefaults() (ExactConfig, error) {
	if c.BlockSize == 0 {
		c.BlockSize = DefaultBlockSize
	}
	if c.TargetFillPct == 0 {
		c.TargetFillPct = DefaultTargetFillPct
	}
	if c.MaxBatchEntries == 0 {
		c.MaxBatchEntries = DefaultMaxBatchEntries
	}
	if c.OverlayMaxEntries == 0 {
		c.OverlayMaxEntries = DefaultOverlayMaxEntries
	}
	if c.MaxBatchEntries > maxBatchEntriesLimit {
		return ExactConfig{}, ErrInvalidConfig
	}
	if c.BlockSize < minBlockSize || c.BlockSize > maxBlockSize {
		return ExactConfig{}, ErrInvalidConfig
	}
	if c.TargetFillPct < 50 || c.TargetFillPct > 100 {
		return ExactConfig{}, ErrInvalidConfig
	}
	if maxRecordsPerBlock(c.BlockSize) < 2 {
		return ExactConfig{}, ErrInvalidConfig
	}
	if targetRecordsPerBlock(c.BlockSize, c.TargetFillPct) < 1 {
		return ExactConfig{}, ErrInvalidConfig
	}
	return c, nil
}

const (
	minBlockSize = 128
	maxBlockSize = 1 << 20
)

// SumSHA256 hashes data with SHA-256.
func SumSHA256(data []byte) Hash { return Hash(sha256.Sum256(data)) }

// SumBLAKE3 hashes data with BLAKE3-256.
func SumBLAKE3(data []byte) Hash { return Hash(blake3.Sum256(data)) }

// HashReaderSHA256 streams r through SHA-256.
func HashReaderSHA256(r io.Reader) (Hash, error) {
	h := sha256.New()
	if _, err := io.Copy(h, r); err != nil {
		return Hash{}, err
	}
	var out Hash
	copy(out[:], h.Sum(nil))
	return out, nil
}

// HashReaderBLAKE3 streams r through BLAKE3-256.
func HashReaderBLAKE3(r io.Reader) (Hash, error) {
	h := blake3.New()
	if _, err := io.Copy(h, r); err != nil {
		return Hash{}, err
	}
	var out Hash
	copy(out[:], h.Sum(nil))
	return out, nil
}
