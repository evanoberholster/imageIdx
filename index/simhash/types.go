package simhash

import (
	"errors"

	common "github.com/evanoberholster/imageIdx/common"
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

// PHashMatch is one pHash-similar result.
type PHashMatch struct {
	ImageID  common.ImageID
	Distance int
}

// PDQMatch is one PDQ-similar result.
type PDQMatch struct {
	ImageID  common.ImageID
	Distance uint16
}

// CombinedMatch is one result of an explicit pHash AND PDQ search.
type CombinedMatch struct {
	ImageID       common.ImageID
	PHashDistance int
	PDQDistance   uint16
}

// Order selects the evaluation order of a combined search.
type Order uint8

const (
	// PHashFirst filters on pHash, then verifies PDQ.
	PHashFirst Order = iota
	// PDQFirst filters on PDQ, then verifies pHash.
	PDQFirst
)

// Config describes one store. The zero value of every field selects a default.
type Config struct {
	// MaxShardBytes caps each shard file. Zero selects 4 GiB.
	MaxShardBytes uint64
	// SeqBlockRecords is the seqlock granularity: records per sequence block.
	// It must be a power of two dividing 65,536. Zero selects 1024.
	SeqBlockRecords int
	// Durability controls whether a commit flushes the shard and manifest.
	Durability common.Durability
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
