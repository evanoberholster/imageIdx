// Package schema defines entity identity, namespace definitions, and durable
// entity-scoped ValueID allocation shared across imageIdx packages.
//
// Namespaces are the single identity for an indexed collection across all
// index kinds (bitmap namespaces, timestamp indexes, hash indexes, and
// vector spaces). Numbers and names are unique within a Definitions registry.
package schema

// ImageID identifies an image across every index and persisted format. It is
// an alias so existing uint64 storage and batch APIs remain zero-conversion.
//
// ImageID 0 is reserved and never represents an image; the roar bitmap
// backend uses it as its sentinel.
type ImageID = uint64

// ValueID identifies a value across every index and persisted format. It is
// an alias so existing uint64 storage and batch APIs remain zero-conversion.
//
// ValueID 0 is reserved and never represents a value; the roar bitmap
// backend uses it as its sentinel.
type ValueID = uint64

// UserID identifies a user principal across every index package. Unlike
// ImageID it is a defined type rather than an alias, so user, group, and
// image IDs never mix implicitly; convert explicitly at trust boundaries.
//
// UserID 0 is reserved and never represents a user, mirroring ImageID 0.
type UserID uint64

// GroupID identifies a group principal across every index package. Like
// UserID it is a defined type so the three ID kinds stay distinct; see
// UserID for the rationale.
//
// GroupID 0 is reserved and never represents a group.
type GroupID uint64

// Durability controls when a write is acknowledged relative to filesystem
// synchronization.
type Durability uint8

const (
	DurabilityOS Durability = iota
	DurabilitySync
)

// Valid reports whether d is a recognized durability level.
func (d Durability) Valid() bool { return d <= DurabilitySync }
