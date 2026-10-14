# Namespace allocator (format v1)

`schema.Open(path, Options{Namespaces: ...})` owns a standalone allocation file.
It reuses `common.Namespace` and `common.ValueID`, but deliberately does **not**
replace `common.Schema` or the current entity-scoped database allocator. A
namespace-scoped ID is not interchangeable with an entity ID shared by multiple
namespaces. Integration into record creation requires an explicit choice of ID
ownership; this package makes no changes to index or database APIs.

`BackendReadWrite` (default) uses `ReadAt`/`WriteAt`/`Sync`; `BackendMmap` uses
the same format through `internal/storage.MappedFile` (`msync` followed by file
sync). The backing file is locked exclusively via a sibling `.lock` file for
its entire lifetime. Mmap is unavailable on platforms where the existing
storage package does not support writable mappings.

## File layout

All integers are little-endian; no Go struct is persisted by pointer.

| Offset / record | Contents |
|---|---|
| 0, 512 | Alternating 512-byte CRC32C superblocks (magic `IDSC`, format 1, generation, directory offset/length/CRC, count, committed end) |
| 1024 onward | Two 64-byte CRC32C allocation slots per namespace, plus append-only namespace directories |
| Directory | count:u16; sorted entries (`namespace:u16`, `nameLength:u16`, `reserveSize:u64`, `slotsOffset:u64`, name bytes); CRC32C:u32 |
| Allocation slot | generation:u64, reservedThrough:u64, lastIssued:u64, clean:u8, zero padding, CRC32C:u32 |

Directory additions and reservation-size changes append new metadata (for
added namespaces) and a new directory, sync the file, then sync the inactive
superblock. The directory is immutable while the Schema is open. Normal
reservation updates write only the inactive 64-byte namespace slot, sync it,
and *then* publish the in-memory limit. Checksums detect damaged records; a
valid older slot is **not** used when the other nonempty slot is damaged,
because it might roll back a previously acknowledged reservation.
Both metadata slots and both superblocks start as identical generation-1
records. A missing/invalid slot is never treated as an intentionally empty
genesis slot after the file has been published.

## Allocation and recovery

A sorted namespace directory is searched without allocations. Each namespace
has an atomic `issued` ticket and an atomically published `reservedThrough`.
`NextID` / `NextIDs` CAS an entire range only if it fits under that boundary.
The slow path holds one namespace lock, persists enough new blocks, then
publishes the boundary. `EnsureMinimum` persists any required extension first
and linearizes at the CAS that advances `issued`: a concurrent allocation whose
CAS precedes that operation may still return below the minimum.

Clean `Close` rejects new operations, waits for admitted calls, and records
the last issued ticket. Reopening a clean file synchronously invalidates the
reusable checkpoint *before* exposing allocation, then resumes at lastIssued
+ 1. Reopening an unclean file skips to reservedThrough + 1, including IDs
issued without a canonical record. Failed or uncertain writes poison the
session. If either committed slot may have been lost or overwritten, opening
fails closed rather than guessing a smaller watermark. Durability assumes the
OS and storage honor successful syncs; arbitrary rollback after acknowledged
syncs is not detectable by checksums. Future format versions are rejected.
