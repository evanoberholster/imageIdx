// Package imageidx defines domain primitives shared by the specialized image
// index packages.
package imageidx

// ImageID identifies an image across every index and persisted format. It is
// an alias so existing uint64 storage and batch APIs remain zero-conversion.
//
// ImageID 0 is reserved and never represents an image; the roar bitmap
// backend uses it as its sentinel.
type ImageID = uint64

// Durability controls when a write is acknowledged relative to filesystem
// synchronization.
type Durability uint8

const (
	DurabilityOS Durability = iota
	DurabilitySync
)

// Valid reports whether d is a recognized durability level.
func (d Durability) Valid() bool { return d <= DurabilitySync }
