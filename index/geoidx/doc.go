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
// The fundamental primitive is a native-order scan: it binary-searches the first
// key of each normalized range, walks the contiguous keys, and reads ImageIDs in
// (S2Key, ImageID) order. It imposes no ImageID ordering. Overlapping and
// adjacent cover cells merge before scanning. There is no per-cell postings
// table, no hash table, and no per-level materialization: S2 already provides
// the hierarchy, and descendant intervals are key ranges.
//
// Two query modes build on that scan:
//
//   - Initial queries (SearchRanges, SearchCell, SearchRect, SearchPolygon,
//     SearchRadius) return ascending []ImageID for the search planner's ordered
//     merges/intersections. The appended suffix is sorted only if needed, using
//     a bounded dense bitset sort when the ImageID domain is compact, an LSD
//     radix sort otherwise, and a comparison sort only for small results.
//   - FilterBy queries (FilterRanges, FilterCell, FilterRect, FilterPolygon,
//     FilterRadius) already hold a candidate roar.Bitmap. They scan in native S2
//     order, test candidates.Contains(ImageID), and add matches directly to the
//     destination bitmap: no sorted slice and no global sort. CollectRanges and
//     its cover variants are the geo -> bitmap counterpart.
//
// Distance-aware results (nearest, radius) carry a great-circle distance from
// the query to the authoritative S2 cell center, since coordinates are not
// stored. Radius results require the cell-center distance to be within the
// radius (the cover is only a candidate superset); Nearest expands a cap cover
// until the k-th cell-center distance is inside the covered radius, which proves
// no unvisited cell can be closer.
//
// # Mutation
//
// One serialized writer maintains pending inserts and deletes in geometrically
// merged, sorted layers alongside the immutable mmap base. Readers merge
// base + layers lock-free; the effective delta is computed once per published
// view and shared. When the active layer reaches TargetDelta it is frozen into a
// new layer, so ordinary writes stay O(TargetDelta). When the pending delta
// reaches the compaction trigger (at least base/8, so write amplification stays
// bounded), a background worker runs an explicit prepare → commit → publish →
// reclaim lifecycle: it writes and syncs a new generation off the writer lock,
// commits the manifest (the durable linearization point) under the lock, swaps
// in the new base, then drains readers and reclaims the old generation without
// the lock. The writer is not blocked by the rewrite and continues accepting
// mutations into fresh layers. Frozen and active mutations are never lost: a
// failure before the manifest commit retains the snapshot and a later trigger
// retries, and once the manifest commits the new generation is always published
// (a manifest that committed despite a reported sync error is reconciled). When
// the pending delta reaches MaxDelta the writer waits for the running compaction
// (backpressure) rather than growing without bound. Compact and Checkpoint fold
// everything synchronously; the old base is reclaimed after a reader grace
// period.
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
