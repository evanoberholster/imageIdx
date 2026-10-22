# schema: definitions and entity IDs (schema.dat v3, V1 baseline)

The persistent-format compatibility contract, the supported version matrix, the
unsupported development formats, and the golden fixtures are documented in
[`FORMAT.md`](FORMAT.md). `schema.dat` v3 is the proposed V1 baseline; v2 is read
as ACTIVE and upgraded on the next directory publication; container v1 and
allocator format v1 are rejected with `ErrVersion`.

`schema.Definitions` registers namespaces, their collection kinds, and owning
entities. `schema.Schema` owns the persistent, immutable snapshot and allocates
IDs through `NextID(entity)`, `NextIDs(entity, count)`, and
`EnsureMinimum(entity, minimum)`. Every namespace belonging to an entity uses
the same ValueID sequence. Different entities may use the same numeric ID.
`Open(path, Options{Definitions: defs, Entities: []EntityConfig{...}})` copies
the definitions and locks the file until `Close`. Registration-order binary
encoding is `NSPC` v3 with per-namespace `ProjectionVersion`; v1/v2 definitions
still decode (v2 defaults each projection version to 1); v1 definitions decode
with an invalid
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

Additive registration is an open-time operation only. `schema.Open` with added
`KindKV` namespaces activates them immediately, because KV collections own no
dependent storage. Added non-KV (index) namespaces are persisted as PREPARED
and stay invisible until the caller validates its engines and calls `Activate`,
so a standalone opener never silently marks an index generation active.
`Options{Prepare: true}` forces the PREPARED path for any addition, which the
database coordinator uses. A PREPARED target is recovered on the next open
without resupplying definitions. There is no runtime registration API; the
active schema is immutable once opened.
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

## Exported API

Normal operation:

- `Open(path, Options)` opens or creates the file; `Options{Definitions, Entities, Prepare, Backend}`.
- `NextID`, `NextIDs`, `EnsureMinimum` allocate and reconcile entity IDs.
- `Definitions`, `Lookup`, `LookupIdentity`, `Len` read the immutable active
  model. `LookupIdentity` omits `Config` and allocates nothing.
- `Close` checkpoints and releases the lock; a second call is a no-op.

Migration/startup only (documented as such on each method): `PreparedDefinitions`,
`Generation`, `ReservationSize`, `Activate`, `LegacyRetired`, `MarkLegacyRetired`.

There is deliberately no runtime `RegisterNamespace`. `Definitions.Register` is
for building configuration before `Open`; once opened, the active model is
immutable and no method mutates it. `Activate` and `MarkLegacyRetired` are the
only post-open writers, and only during startup coordination.

Definition blobs, names, kinds, and entity ownership are validated by
`Definitions.Register` and `UnmarshalBinary`. Errors are `errors.Is`-compatible
sentinels: `ErrNamespace`, `ErrNamespaceDuplicate`, `ErrNamespaceName`,
`ErrNamespaceInvalid`, `ErrNamespaceKind`, `ErrNamespaceEntity`, `ErrVersion`,
`ErrCorrupt`, `ErrCorruptSchema`, `ErrExhausted`, `ErrLocked`, `ErrClosed`, and
`ErrCount`; messages include the offending namespace number.
