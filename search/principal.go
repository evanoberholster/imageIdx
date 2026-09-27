package search

import (
	"encoding/binary"
	"errors"

	common "github.com/evanoberholster/imageIdx/common"
	"github.com/evanoberholster/imageIdx/index/bitmapidx"
)

// ErrInvalidPrincipal reports the reserved zero user or group ID.
var ErrInvalidPrincipal = errors.New("search: invalid principal")

// ACL principals come in two kinds, each with its own KindEnum namespace:
// common.NamespaceUserACL holds user IDs, common.NamespaceGroupACL
// holds group IDs. The namespace IS the stored key domain and the stored
// key IS the uint64 ID in fixed 8-byte little-endian form — no prefixes,
// no separators, no variable lengths. Hash domain separation over the
// namespace keeps equal IDs in different namespaces (or in ordinary enum
// namespaces) fully distinct.
//
// A search-time ACL check unions one user's posting with the postings for
// the user's authenticated groups. Each value's posting — the bitmap named
// by one principal — holds exactly the ImageIDs granted to that principal.
// Callers must authenticate group membership before supplying group IDs;
// this package stores image grants, not user-to-group membership.
// UserACL is one user principal for the common.NamespaceUserACL namespace:
// a KindEnum namespace whose values are uint64 user IDs, each naming the
// images one user may see. The zero value is invalid; construct with
// NewUserACL.
//
// Direct user grants live in this namespace. Group grants live separately in
// GroupACL and are unioned at search time using authenticated group IDs.
type UserACL = principal[common.UserID]

// NewUserACL validates id and returns its principal. Only the reserved
// zero ID is rejected, mirroring the reserved ImageID zero.
func NewUserACL(id common.UserID) (UserACL, error) { return newPrincipal(id) }

// GroupACL is one group principal for the common.NamespaceGroupACL
// namespace. Its posting records images granted to the group; search unions
// it with the direct user posting for authenticated groups. It also
// supports grant audits via ValueIDs. The zero value is invalid; construct
// with NewGroupACL.
type GroupACL = principal[common.GroupID]

// NewGroupACL validates id and returns its principal, under the same rule
// as NewUserACL.
func NewGroupACL(id common.GroupID) (GroupACL, error) { return newPrincipal(id) }

// ParseUserACL recovers the principal from canonical dictionary bytes, as
// returned by LookupValue or Iterate. Anything but exactly 8 bytes
// reports false; within a dedicated user namespace every surviving value
// is a principal by construction.
func ParseUserACL(canonical []byte) (UserACL, bool) {
	return parsePrincipal[common.UserID](canonical)
}

// ParseGroupACL recovers the principal from canonical dictionary bytes,
// mirroring ParseUserACL.
func ParseGroupACL(canonical []byte) (GroupACL, bool) {
	return parsePrincipal[common.GroupID](canonical)
}

// principal is the shared implementation behind UserACL and GroupACL. The
// two principals differ only in ID type and documentation, so one generic
// core carries validation, encoding, and parsing once.
type principal[T ~uint64] struct {
	id T
}

// newPrincipal validates id and returns its principal. Only the reserved
// zero is rejected; fixed-width numeric IDs have no separators to forbid
// and never approach bitmapidx MaxValueBytes.
func newPrincipal[T ~uint64](id T) (principal[T], error) {
	if id == 0 {
		return principal[T]{}, ErrInvalidPrincipal
	}
	return principal[T]{id: id}, nil
}

// ID returns the validated identifier, equal to the stored key.
func (p principal[T]) ID() T { return p.id }

// principalKey encodes id in fixed 8-byte little-endian form. The array
// is returned by value, so stack-resident callers allocate nothing.
func principalKey(id uint64) [8]byte {
	var key [8]byte
	binary.LittleEndian.PutUint64(key[:], id)
	return key
}

// Value returns the enum value for membership writes and presence checks:
// the ID in fixed 8-byte little-endian form. Like bitmapidx.StringValue it
// copies into a fresh slice.
func (p principal[T]) Value() bitmapidx.Value {
	key := principalKey(uint64(p.id))
	return bitmapidx.Value{Kind: bitmapidx.KindEnum, Bytes: key[:]}
}

// parsePrincipal recovers a principal from canonical dictionary bytes,
// rejecting anything but exactly 8 bytes (all-zero decodes to the
// reserved zero, which newPrincipal rejects).
func parsePrincipal[T ~uint64](canonical []byte) (principal[T], bool) {
	if len(canonical) != 8 {
		return principal[T]{}, false
	}
	p, err := newPrincipal(T(binary.LittleEndian.Uint64(canonical)))
	if err != nil {
		return principal[T]{}, false
	}
	return p, true
}
