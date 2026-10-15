# schema: definitions and entity IDs (file format v3)

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
Format v3 retains the v2 slot and directory record layouts and keys directories
by **entity** instead of namespace. Each superblock also checksums a serialized
definitions snapshot adjacent to its directory. Existing definitions and their
entity relationships must match on reopen; new definitions can be appended in
registration order. Removed, renamed, or reassigned definitions fail to open.
Version 2 files remain readable as ACTIVE. Version 3 uses one formerly reserved
superblock payload byte for PREPARED/ACTIVE. A PREPARED superblock holds the
complete target definitions while its predecessor remains the active snapshot.
`Open` can recover the target without resupplying definitions;
`PreparedDefinitions` returns a copy, while `Definitions` and `Lookup` continue
to expose only the old active definitions. `database.Open` verifies and rebuilds
required index resources before calling `Activate`; no DB is exposed while
PREPARED. Another version-3 payload byte records completion of the one-time
legacy schema-copy cleanup, avoiding legacy reads on normal reopen.
An allocator file from v1 is rejected with `ErrVersion`: reinterpreting its
namespace counters as entity counters would permit ID reuse. No automatic v1
allocator-file conversion is attempted. Legacy KV/index definition blobs are
compared to the central snapshot and retired after `schema.dat` is durable.
Format v1 appeared on the unmerged PR branch and no repository releases
contain it; use outside that branch cannot be ruled out, so any deployment
that used it requires a separate offline migration before adopting v3.

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
their original numbers. Database namespace additions use a coordinated startup
migration; mismatched legacy copies fail closed until retired.
