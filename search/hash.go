package search

import "github.com/evanoberholster/imageIdx/index/radixhash"

// HashAlgorithm names the 256-bit digest family an exact-hash clause searches.
// A radixhash index holds exactly one algorithm, so the value is plan metadata
// and a validation tag rather than an index selector: pass the index built for
// that family.
type HashAlgorithm uint8

const (
	// HashUnknown is the zero value and is rejected by ExactHash compilation.
	HashUnknown HashAlgorithm = iota
	// HashSHA256 selects SHA-256 digests.
	HashSHA256
	// HashBLAKE3 selects BLAKE3-256 digests.
	HashBLAKE3
)

// String returns the lowercase algorithm name used in plan predicates.
func (a HashAlgorithm) String() string {
	switch a {
	case HashSHA256:
		return "sha256"
	case HashBLAKE3:
		return "blake3"
	default:
		return "unknown"
	}
}

// Valid reports whether a is a supported digest family.
func (a HashAlgorithm) Valid() bool { return a == HashSHA256 || a == HashBLAKE3 }

// hashClause is one exact 256-bit digest restriction.
type hashClause struct {
	index     *radixhash.Index
	algorithm HashAlgorithm
	hash      radixhash.Hash
}

func (hashClause) searchClause() {}

// ExactHash restricts results to images whose exact 256-bit digest is present
// in index, an exact-content radixhash index. algorithm names the digest family
// (HashSHA256 or HashBLAKE3) and must match the algorithm the index was built
// with; the index itself is one radixhash shard.
//
// Compute the digest with crypto/sha256 or a BLAKE3 implementation and pass the
// raw 32 bytes. Matching is exact: a one-bit difference is a miss. Deletions are
// caller-managed, so intersect with a live/ACL clause when rows are tombstoned.
//
//	digest := sha256.Sum256(content)
//	plan, err := search.Compile(
//		search.ExactHash(sha, search.HashSHA256, digest),
//		search.AllValues(tags, bitmapidx.EnumStringValue("favorite")),
//	)
func ExactHash(index *radixhash.Index, algorithm HashAlgorithm, digest [32]byte) Clause {
	return hashClause{index: index, algorithm: algorithm, hash: radixhash.Hash(digest)}
}
