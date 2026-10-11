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
// The immutable base is a directory of size-bounded shards. Each shard holds two
// parallel arrays sorted by (S2Key, ImageID):
//
//	keys     []S2Key
//	imageIDs []ImageID
//
// A shard is one contiguous interval of the global (S2Key, ImageID) order. Shard
// boundaries are chosen by data size (TargetShardBytes/MaxShardBytes), not by an
// S2 prefix, so a repeated S2Key may span several shards and the directory is
// typically only tens of entries even at 20-30M images. Repeated keys represent
// several images at the same location. Within a shard the arrays are
// structure-of-arrays, not per-entry structs, so a range scan walks one
// contiguous key array and reads ImageIDs only for the matching span. A single
// entry-level mutation never rewrites a shard; compaction replaces shards
// wholesale.
//
// # Queries
//
// The fundamental primitive is a native-order scan: it walks the shard directory
// in global key order, binary-searches the first key of each normalized range
// inside each shard, walks the contiguous keys, and reads ImageIDs in (S2Key,
// ImageID) order. A range that spans shards continues into the next shard rather
// than restarting, and a repeated key whose run spans shards is visited in every
// shard that holds part of it. It imposes no ImageID ordering. Overlapping and
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
// One serialized writer maintains a single global pending insert/delete overlay
// in geometrically merged, sorted layers alongside the immutable shard
// directory. Reads are ImageID-authoritative: every mutation names an ImageID,
// and a delete or move suppresses the old record wherever it lives even when the
// caller supplies a stale S2Key, so no reverse index is needed. When the active
// layer reaches TargetDelta it is frozen into a new layer, so ordinary writes
// stay O(TargetDelta). When the pending delta reaches the compaction trigger (at
// least base/8, so the per-mutation rewrite ratio is unchanged), a background
// worker runs an explicit prepare → commit → publish → reclaim lifecycle:
//
//   - prepare (off-lock) selects the affected shards and rebuilds only those.
//     Adds are routed to a shard by (key, id); a shard is also affected when any
//     of its base ImageIDs is named by the delta, which is how a delete or move
//     reaches a shard it does not name by key. Each shard carries conservative
//     MinImageID/MaxImageID bounds so the delta's ImageID range prunes shards
//     whose range does not overlap, which is the common case for monotonically
//     increasing or geographically clustered IDs. Unaffected shards are kept by
//     reference. A rebuilt shard larger than MaxShardBytes is split near the
//     median; adjacent underutilized plans are coalesced with hysteresis so
//     split/merge cannot cycle. A fully empty shard leaves the directory.
//   - commit (locked) writes the inactive superblock slot: the durable
//     linearization point.
//   - publish (locked) swaps in the new directory.
//   - reclaim (off-lock) drains readers and releases superseded mappings; replaced
//     extents become reusable once the generation that referenced them is no
//     longer a recoverable fallback.
//
// The writer is not blocked by a rebuild and continues accepting mutations into
// fresh layers. Frozen and active mutations are never lost: a failure before the
// superblock commit retains the snapshot and a later trigger retries, and once
// the slot commits the new directory is always published (a slot that committed
// despite a reported sync error is reconciled). When the pending delta reaches
// MaxDelta the writer waits for the running compaction (backpressure) rather
// than growing without bound. Compact and Checkpoint fold everything
// synchronously; replaced extents are reclaimed after a reader grace period.
//
// # Persistence
//
// A file-backed index stores everything in one mmap file (geoidx.dat): two
// alternating CRC32C superblock slots and a pagealloc-managed arena holding every
// shard extent and the shard directory. A shard is one independently replaceable
// in-file extent, so compaction rewrites only the affected extents and reuses
// free space. The active superblock is the durable commit point and references
// the live directory; the inactive slot holds the previous generation.
//
// Allocator state is reconstructed rather than stored as a second database: the
// committed directory (plus the one previous generation, retained for fallback)
// is the authoritative live-extent set, and recovery rebuilds the free-space map
// as its complement. An extent is reusable only after it is unreachable from the
// two recoverable generations and the reader grace period has passed; an
// uncommitted allocation is free after a crash because no committed directory
// references it.
//
// The arena is mapped once at a fixed capacity (Config.MaxFileBytes). When it
// must grow, the file is extended and a new mapping is established; the old
// mapping is kept until no published shard references it and prior readers have
// drained, then unmapped. Existing mappings are never re-mapped in place, so
// reader slices stay valid.
//
// Persistence is checkpoint-based: mutations are runtime state and become durable
// on Checkpoint/Compact. Close does not checkpoint. A pre-shard monolithic or
// multi-file directory, or an older single-file layout, is rejected with
// ErrLegacyFormat rather than reinterpreted; rebuild by re-inserting from
// canonical image metadata.
package geoidx
