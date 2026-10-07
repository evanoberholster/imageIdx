// Package geoidx is a persistent, mmap-oriented geographic index for images.
//
// # Model
//
// Each ImageID has zero or one geographic location. Many images may share the
// same location. The authoritative representation is an S2 cell key; latitude
// and longitude are never stored, and there is no ImageID -> key reverse index
// (reverse lookup belongs to imagemetadata).
//
// # Cell level
//
// Keys are S2 cells at a single fixed level, S2Level (19). At level 19 an S2
// cell has an average edge of about 17.7 m and a maximum edge of about 20.7 m
// (average area about 309 m^2), so every cell is on the order of 20 m and the
// whole cell stays within the requested 20-30 m precision band. The previous
// bitmapidx encoding used level 16 (~142 m average edge), far coarser than the
// target; level 18 (~35 m average, ~41 m maximum) exceeds 30 m.
//
// A key is the full 64-bit S2 cell ID shifted right so the trailing zero bits of
// a level-19 cell are dropped:
//
//	key = uint64(cellID) >> 22
//
// Keys are therefore contiguous uint64s, and the descendants of any S2 cell
// occupy a contiguous key interval. That single fact lets queries be answered by
// binary search plus contiguous scans instead of a spatial tree.
//
// # Representation
//
// The immutable base is two parallel arrays sorted by (S2Key, ImageID):
//
//	keys     []S2Key
//	imageIDs []ImageID
//
// Repeated keys represent several images at the same location. The arrays are
// structure-of-arrays, not per-entry structs, so a range scan walks one
// contiguous key array and reads ImageIDs only for the matching span.
//
// # Queries
//
// A query cover is normalized into sorted, non-overlapping key ranges, and each
// range is answered by binary searching keys and scanning the covered ImageIDs.
// Overlapping and adjacent cover cells merge before scanning. There is no
// per-cell postings table, no hash table, and no per-level materialization: S2
// already provides the hierarchy, and descendant intervals are key ranges.
//
// Search returns results in ascending ImageID order so the higher-level search
// planner can intersect them with bitmap predicates. Distance-aware results
// (nearest, radius) carry an approximate great-circle distance derived from the
// cell center, since coordinates are not stored.
//
// # Mutation
//
// One serialized writer maintains a small sorted mutation delta (inserts and
// deletes) alongside the immutable base. Readers merge base and delta
// lock-free. Compact folds the delta into a new immutable base with a streaming
// ordered merge; the old base is reclaimed after a reader grace period. Batches
// are expected to be small (<= 64 images).
//
// # Persistence
//
// A file-backed index stores the base in a generation-numbered mmap file
// (base.<gen>.dat) behind a dual-slot CRC32C superblock, and a store manifest
// holding the durable generation. Persistence is checkpoint-based: mutations are
// runtime state and become durable on Checkpoint/Compact. Close does not
// checkpoint. Nothing about the layout changes between the in-memory and
// file-backed forms.
package geoidx
