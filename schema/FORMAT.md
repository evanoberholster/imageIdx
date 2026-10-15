# schema.dat persistent format contract (V1)

This document is the compatibility contract for the single authoritative schema
and allocator file. Any change to the byte layout, the accepted versions, or the
recovery rules is a format decision and requires a new version plus a migration
plan. The committed fixtures under `schema/testdata/` pin the current bytes.

## Supported versions

| Artifact | Version | Meaning | Read | Write |
|---|---|---|---|---|
| `schema.dat` container | 3 | **V1 baseline**: ACTIVE/PREPARED generation and legacy-retirement state | yes | yes |
| `schema.dat` container | 2 | prior release: same slot/directory layout, no PREPARED state | yes (read as ACTIVE) | upgraded to 3 on the next directory publication |
| definitions blob (`NSPC`) | 2 | per-definition `EntityKind` | yes | yes |
| definitions blob (`NSPC`) | 1 | pre-entity, `EntityKind` byte reserved zero | yes (Entity is `EntityInvalid`) | rewritten as 2 on the next publication |

Everything else fails closed with `schema.ErrVersion` for containers and
`schema.ErrCorruptSchema` for definition blobs. Unknown versions are never
reinterpreted.

## Unsupported development formats

These existed only on unreleased branches and are rejected, not migrated:

- `schema.dat` container version 1 (pre-entity, namespace-keyed directories).
- Allocator format v1 whose counters were namespace-scoped. Reinterpreting
  namespace counters as entity counters would permit ID reuse, so `Open`
  returns `schema.ErrVersion`. No automatic conversion exists.
- Any definitions blob version other than 1 or 2.

## Container layout (version 3)

- Two alternating 512-byte checksummed superblocks at offsets 0 and 512.
  Generation 1 is written to both; every later generation writes the inactive
  slot, so the previous state is always recoverable.
- Superblock payload: directory offset `u64`, directory length `u64`, directory
  CRC32C `u32`, entity count `u16`, reserved `u16`, end of committed file `u64`,
  definitions offset `u64`, definitions length `u64`, definitions CRC32C `u32`,
  `state:u8` (0 ACTIVE, 1 PREPARED), `legacyRetired:u8`, `reserved:10`.
  Version 2 requires the three trailing bytes to be zero and is read as ACTIVE
  with `legacyRetired=false`.
- Per-entity directory entry: `id:u16, nameLen:u16, reserve:u64, slotsOffset:u64,
  name:nameLen`, sorted by entity ordinal, terminated by a CRC32C.
- Two 64-byte checksummed allocation slots per entity: `generation:u64,
  reservedThrough:u64, lastIssued:u64, clean:u8, zero padding, CRC32C`.
- Directories and definitions are append-only; an append publishes the inactive
  superblock after the new bytes are synced. Uncommitted crash tails are never
  reused because offsets extend from the physical EOF.

Directory identifiers are `EntityKind` ordinals, not namespace numbers. The
definitions blob maps namespaces to entities and is the only authority for
namespace identity.

## Recovery rules

- A clean allocation slot checkpoints `lastIssued`; reopening durably
  invalidates that checkpoint before issuing again and continues at
  `lastIssued + 1`. An unclean slot resumes at `reservedThrough + 1`, skipping
  the unused tail of the reserved block. Under no circumstance does a high-water
  mark decrease or an acknowledged ID get reused.
- Both superblock slots must validate. A damaged newer slot fails with
  `ErrCorrupt` rather than silently rolling back to an older valid slot.
- A PREPARED superblock holds the complete additive target while its immediate
  predecessor remains the active definitions. Recovery loads the target from
  the file without caller input. `Definitions()` and `Lookup` expose only active
  definitions; `PreparedDefinitions()` exposes the target.
- `Activate` and `MarkLegacyRetired` publish via a single synced superblock
  write; a failure poisons the session and the caller must reopen.
- Failed opens release the file mapping, file descriptor, and lock.

## Golden fixtures

`schema/testdata/` contains byte-stable artifacts; `TestGoldenFixtureChecksums`
pins their CRC32. Regenerate deliberately with:

```
IMAGEIDX_UPDATE_GOLDEN=1 go test ./schema -run TestUpdateGoldenFixtures
```

| Fixture | State |
|---|---|
| `v3-clean.dat` | version 3, clean checkpoint, image issued 1..5, person 1 |
| `v3-unclean.dat` | version 3, no clean checkpoint (resumes at reserved+1) |
| `v3-prepared.dat` | version 3, additive index namespace PREPARED, root ACTIVE |
| `v3-active-after-prepare.dat` | version 3, the PREPARED target activated |
| `v2-active.dat` | version 2, prior release, read as ACTIVE |
| `v3-unsupported-version.dat` | container version 99, must return `ErrVersion` |

## Upgrade behavior

- A version 2 file that only has IDs allocated keeps its version until the next
  directory publication; then the container is written as version 3.
- Adding definitions (KV or index) writes a new version 3 directory. KV-only
  additions activate immediately; index additions publish PREPARED first.
- Legacy Pebble and `index/schema.bin` schema copies are retired once, and the
  `legacyRetired` byte prevents any later read of them.
