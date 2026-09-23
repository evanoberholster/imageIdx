package bitmapidx

import (
	"encoding/binary"
	"errors"
)

// ErrInvalidPrincipal reports the reserved zero user or group ID.
var ErrInvalidPrincipal = errors.New("bitmapidx: invalid principal")

// ACL principals come in two kinds, each with its own KindEnum field:
// IndexTypeUserACL holds user IDs, IndexTypeGroupACL holds group IDs. The
// field IS the namespace and the stored key IS the uint64 ID in fixed
// 8-byte little-endian form — no prefixes, no separators, no variable
// lengths. Hash domain separation over the field type keeps equal IDs in
// different fields (or in ordinary enum fields) fully distinct.
//
// A search-time ACL check unions one user's posting with the postings for
// the user's authenticated groups. Each value's posting — the bitmap named
// by one principal — holds exactly the ImageIDs granted to that principal.
// Callers must authenticate group membership before supplying group IDs;
// this package stores image grants, not user-to-group membership.
// UserACL is one user principal for the IndexTypeUserACL field: a KindEnum
// field whose values are uint64 user IDs, each naming the images one user
// may see. The zero value is invalid; construct with NewUserACL.
//
// Direct user grants live in this field. Group grants live separately in
// GroupACL and are unioned at search time using authenticated group IDs.
type UserACL = principal[UserID]

// NewUserACL validates id and returns its principal. Only the reserved
// zero ID is rejected, mirroring the reserved ImageID zero.
func NewUserACL(id UserID) (UserACL, error) { return newPrincipal(id) }

// GroupACL is one group principal for the IndexTypeGroupACL field. Its
// posting records images granted to the group; search unions it with the
// direct user posting for authenticated groups. It also supports grant
// audits via ValueIDs. The zero value is invalid; construct with NewGroupACL.
type GroupACL = principal[GroupID]

// NewGroupACL validates id and returns its principal, under the same rule
// as NewUserACL.
func NewGroupACL(id GroupID) (GroupACL, error) { return newPrincipal(id) }

// ParseUserACL recovers the principal from canonical dictionary bytes, as
// returned by LookupValue or Iterate. Anything but exactly 8 bytes
// reports false; within a dedicated user field every surviving value is
// a principal by construction.
func ParseUserACL(canonical []byte) (UserACL, bool) { return parsePrincipal[UserID](canonical) }

// ParseGroupACL recovers the principal from canonical dictionary bytes,
// mirroring ParseUserACL.
func ParseGroupACL(canonical []byte) (GroupACL, bool) { return parsePrincipal[GroupID](canonical) }

// principal is the shared implementation behind UserACL and GroupACL. The
// two principals differ only in ID type and documentation, so one generic
// core carries validation, encoding, and parsing once.
type principal[T ~uint64] struct {
	id T
}

// newPrincipal validates id and returns its principal. Only the reserved
// zero is rejected; fixed-width numeric IDs have no separators to forbid
// and never approach MaxValueBytes.
func newPrincipal[T ~uint64](id T) (principal[T], error) {
	if id == 0 {
		return principal[T]{}, ErrInvalidPrincipal
	}
	return principal[T]{id: id}, nil
}

// ID returns the validated identifier, equal to the stored key.
func (p principal[T]) ID() T { return p.id }

// Value returns the enum value for membership writes and presence checks:
// the ID in fixed 8-byte little-endian form. Like StringValue it copies
// into a fresh slice.
func (p principal[T]) Value() Value {
	var key [8]byte
	binary.LittleEndian.PutUint64(key[:], uint64(p.id))
	return Value{Kind: KindEnum, Bytes: key[:]}
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
