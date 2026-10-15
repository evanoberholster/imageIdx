# schema: definitions and entity IDs (allocator format v2)

`schema.Definitions` registers namespaces, their collection kinds, and owning
entities. `schema.Schema` owns the persistent, immutable snapshot and allocates
IDs through `NextID(entity)`, `NextIDs(entity, count)`, and
`EnsureMinimum(entity, minimum)`. Every namespace belonging to an entity uses
the same ValueID sequence. Different entities may use the same numeric ID.
`Open(path, Options{Definitions: defs, Entities: []EntityConfig{...}})` copies
the definitions and locks the file until `Close`. Registration-order binary
encoding (`NSPC` v2) is unchanged; v1 definitions still decode with an invalid
entity, which must be assigned before allocating IDs.
`Open(path, Options{})` opens only an existing file and loads its persisted
definitions under the exclusive lock. Lookup returns owned Config bytes;
`LookupIdentity` omits Config and allocates nothing.

The single file has two alternating checksummed 512-byte superblocks, two
checksummed 64-byte reservation slots per entity, and append-only directories.
Format v2 retains the v1 slot and directory record layouts but keys directories
by **entity** instead of namespace. Each superblock also checksums a serialized
definitions snapshot adjacent to its directory. Existing definitions and their
entity relationships must match on reopen; new definitions can be appended in
registration order. Removed, renamed, or reassigned definitions fail to open.
An allocator file from v1 is rejected with `ErrVersion`: reinterpreting its
namespace counters as entity counters would permit ID reuse. No automatic v1
allocator-file conversion is attempted. Existing KV/index definition blobs use
their original serialization and are compared to the central snapshot on open.
Format v1 appeared on the unmerged PR branch and no repository releases
contain it; use outside that branch cannot be ruled out, so any deployment
that used it requires a separate offline migration before adopting v2.

`BackendReadWrite` (default) uses `ReadAt`/`WriteAt`/`Sync`;
`BackendMmap` uses the same format through `internal/storage.MappedFile`.
Fast-path allocations use atomic range reservation without heap allocation;
slow-path reservations sync before publishing an in-memory limit. Clean close
records the last issued ID. Reopening a clean file invalidates that checkpoint
durably before returning IDs and continues at lastIssued + 1; unclean recovery
skips to reservedThrough + 1. Failed or uncertain writes poison the session.
Both slots must validate rather than rolling back to a possibly stale one.
Durability assumes the storage device honors acknowledged syncs.

`database` coordinates opening and closing the schema; `store` owns persisted
canonical values; index packages own secondary indexes. A database with
duplicate numeric namespaces across KV and index definitions fails explicitly
instead of silently renumbering durable identifiers.
The canonical KV namespace is an explicit entity-to-namespace relationship;
its number need not equal the entity ordinal. Historical assignments retain
their original numbers. Database namespace additions require a coordinated
startup migration; mismatched compatibility copies currently fail closed.
