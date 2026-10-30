# Database on-disk formats (0.5)

This document is the 0.5 compatibility contract for the database-owned durable
data. It covers `schema.dat`, canonical values, the mutation journal, fixed-width
store keys, and the derived-index boundary.

The authoritative schema layout is documented separately in
[`schema/FORMAT.md`](../schema/FORMAT.md). Derived index formats are described
in [`index/README.md`](../index/README.md) and are rebuildable.

## Root layout

`database.Open(root, ...)` uses two fixed subdirectories:

```text
<root>/data/    canonical Pebble store, schema.dat, mutation journal
<root>/index/   every derived index family, including vector-*.idx
```

`database.OpenDirs(dataDir, indexDir, ...)` preserves custom layouts. The
compatibility contract applies to the files inside those directories; moving a
database requires moving both trees together.

## Supported 0.5 formats

| Artifact | 0.5 version | Read | Write | Notes |
|---|---:|---|---|---|
| `schema.dat` container | 3 | yes | yes | 0.5 baseline; v2 is read as ACTIVE and upgraded on publication |
| Definitions blob (`NSPC`) | 3 | yes | yes | Includes `EntityKind` and `ProjectionVersion`; v1/v2 are read |
| Canonical value — inline | 1 | yes | yes | Projection bytes stored beside the application record |
| Canonical value — external | 2 | yes | yes | Only the application record is stored; projector required |
| Mutation journal | 2 | yes | yes | Ordered, CRC-protected before/after delta envelope |
| `store.bin` (derived index manifest) | 2 | yes | yes | Rebuildable cache; not a permanent compatibility promise |
| Journal v1 / canonical unknown / index unknown | — | no | no | Fail closed; never reinterpret |

Pre-0.5 experimental databases are not promised to open. 0.5 must reopen 0.5
databases and reject unsupported or corrupt state explicitly.

## Pebble namespace layout

`store.Key` is exactly 12 bytes:

```text
namespace:uint16 BE | valueID:uint64 BE | flags:uint16 BE
```

The database owns these reserved namespaces:

| Namespace | Name | Purpose |
|---:|---|---|
| `65533` | `mutation-journal` | Ordered journal records keyed by sequence |
| `65534` | `database-generations` | Committed, indexed, and in-flight generation metadata |
| `0xFFF0` | liveness | Internal unified-index liveness namespace |

Canonical KV namespaces are registered in `schema.dat` as `KindKV` definitions.
Their numbers are persisted; they are not assumed to equal `EntityKind` ordinals.
`ValueID` zero is reserved.

Database-owned keys use `flags = 0`. The 2-byte field is preserved in the
12-byte key representation for the generic `store` API; non-zero flags are not
part of the 0.5 database format.

### Generation metadata

Generation values are 8-byte big-endian `uint64` values in namespace `65534`:

| Key ID | Meaning |
|---:|---|
| `1` | Last committed canonical/journal sequence |
| `2` | Last durably acknowledged/indexed sequence |
| `3` | In-flight intent sequence; `0` when idle |
| `4` | Recovery-only canonical schema backup |

`schema.dat` remains the authoritative schema. Key `4` is a legacy and recovery
fallback, not a competing runtime registry.

## Canonical value envelope

A canonical value is stored in the entity's persisted `KindKV` canonical
namespace. The envelope is:

```text
0        magic       u8  = 0xD1
1        flags       u8  bit 0 = tombstone; other bits reserved
2        version     u8  1 = inline, 2 = external
3..7     projection  u32 BE byte length
7..      projection bytes (inline only; length must be 0 for external)
...      application record bytes (rest of value)
```

Properties:

- The envelope has no separate CRC. Pebble provides the durable checksum for
  the stored value.
- Inline mode stores projection bytes plus the application record.
- External mode stores a zero projection length and the application record; the
  configured `ProjectFromRecord` or `ProjectInto` callback re-derives projections.
- A tombstone (`flags & 1`) marks a logically deleted entity. Inline tombstones
  retain the prior projection; external tombstones retain the prior application
  record. This allows reinsertion to remove the old indexed associations.
- Invalid magic, unsupported version, non-zero reserved flags, an external
  projection length other than zero, or a projection length beyond the value
  fails closed.
- The selected projection mode is persisted in `schema.dat` config bytes. A
  reopen that requests a conflicting mode fails with
  `ErrCanonicalModeMismatch`.

## Mutation journal v2

Journal records live at:

```text
store.Key{Namespace: 65533, ID: sequence, Flags: 0}
```

Payload:

```text
0        version       u8  = 2
1        flags         u8  bit 0 = delete, bit 1 = insert,
                          bit 2 = reinsert over a tombstone
2        entity        u8  schema.EntityKind
3..10    valueID       u64 BE
11..12   beforeCount   u16 BE
13..14   afterCount    u16 BE
15..18   beforeLen     u32 BE
19..22   afterLen      u32 BE
23..     before associations
...      after associations
...      crc32 IEEE of all preceding bytes (u32 BE)
```

Each association is:

```text
family:u8 | namespace:u16 BE | family payload
```

Family tags:

| Tag | Family |
|---:|---|
| `1` | timestamp |
| `2` | geography |
| `3` | perceptual hash |
| `4` | exact hash |
| `5` | vector |
| `6` | bitmap |

The before list is the set removed by the mutation; the after list is the set
added. An insert is flagged explicitly so an empty final projection is not
confused with a no-op update. A reinsert over a tombstone carries the old
projection in the before list and the new projection in the after list; replay
must restore entity liveness while removing associations absent from the new
projection. Delete entries carry no association payload. Journal version 1 and
unknown versions fail closed.

The journal sequence must be contiguous within the acknowledged range. Gaps,
bad CRCs, trailing bytes, invalid entity/namespace associations, and unsupported
versions are corruption, never skipped.

## Startup reconciliation

The durable order is:

1. canonical value and journal entry are committed in one synced Pebble batch;
2. the coordinator applies the journal interval to the derived index;
3. the index reaches the required source generation;
4. Pebble acknowledges the indexed generation;
5. journal cleanup removes only acknowledged records.

If the index watermark is ahead of the database acknowledgment, the already
durable prefix can be acknowledged safely. If the acknowledgment is ahead of a
recoverable index, startup returns `ErrRecoveryRequired` and preserves the
journal.

## Derived-index recovery boundary

Derived index files are rebuildable and are not covered by the canonical 0.5
binary compatibility promise. `RecoveryAutomatic` may replace the complete
unified index directory from canonical projections using:

```text
<indexDir>.rebuild       staging directory
<indexDir>.activation    checksummed BUILDING/VERIFIED marker
<indexDir>.previous      previous directory during activation
```

The staging index is verified, synced, and activated through directory renames.
The previous index is retired only after the replacement is verified. A crash at
any activation boundary is restart-recoverable; an unowned staging/previous
directory or unresolved activation marker fails closed.

Selective namespace/family recovery is deferred. The database uses the full
unified-index rebuild as the safe recovery unit because bitmap namespaces share
one physical store and time namespaces share one physical store.

## Golden and negative tests

The 0.5 formats are exercised by:

- `schema/testdata/` and `schema/golden_test.go` for `schema.dat`;
- `database/journal_test.go` for journal v2 roundtrip, golden prefix, CRC,
  version, flags, truncation, and association failures;
- `database/canonical_test.go`, `database/canonical_format_test.go`,
  `database/database_test.go`, and `database/journal_test.go` for canonical
  inline/external, tombstone, projection-mode, unsupported-version, and
  malformed-envelope behavior;
- crash and restart tests under `database/*_crash_test.go` and
  `schema/recovery_test.go`.

## 0.5 change policy

- Changing `schema.dat`, canonical record meaning, or journal semantics requires
  a new format version and an explicit migration or fail-closed rejection.
- Adding fields without changing meaning may remain version-compatible only when
  the decoder rejects malformed values and tests pin the bytes.
- Derived index formats may evolve with explicit version detection and a safe
  rebuild route. They are never silently reinterpreted.
- APIs follow the documented 0.5 public surface; breaking changes remain possible
  before 1.0. Vector APIs and file formats remain experimental.
