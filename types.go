// Package imageidx defines domain primitives shared by the specialized image
// index packages.
package imageidx

// ImageID identifies an image across every index and persisted format. It is
// an alias so existing uint64 storage and batch APIs remain zero-conversion.
type ImageID = uint64

// Durability controls when a write is acknowledged relative to filesystem
// synchronization.
type Durability uint8

const (
	DurabilityOS Durability = iota
	DurabilitySync
)
