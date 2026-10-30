# database: authoritative records and derived indexes (0.5)

`database.DB` is the primary 0.5 entry point for applications. It coordinates:

- the authoritative `schema.dat` file;
- Pebble canonical records and the ordered mutation journal;
- the derived unified index and its recovery state;
- the asynchronous coordinator that applies journal entries to indexes and
  acknowledges completed generations.

Canonical commits are durable before indexing completes. Search is eventually
consistent unless the caller waits for the returned generation.

## Package direction

```text
schema + database + index + store + search
```

`database` owns `schema.Schema`, a canonical `store.Store`, and an
`index.Store`. `database` does not import `search`; applications bind search with:

```go
engine, err := search.New(db.Index())
```

`DB.Index()` is an advanced accessor. Do not write to or close the returned
store while the coordinator is running.

## Create and open

`database.Open` takes one root directory. Canonical records live in
`<root>/data`; every derived index family, including vectors, shares
`<root>/index`:

```go
defs := schema.NewDefinitions()
tag, err := index.BitmapDef(bitmapidx.NamespaceSpec{
    Namespace: schema.NamespaceTag,
    Kind:      schema.KindBitmapEnum,
}, "tags", schema.EntityImage)
if err != nil { return err }
if err := defs.Register(tag); err != nil { return err }

db, err := database.Open(root, decodeProjection, database.Options{
    Definitions: defs,
    BatchSize:   64,
})
if err != nil { return err }
defer db.Close()
```

On a new root, `database.Open` creates the shared index tree from
`Options.Definitions`, writes `schema.dat`, and initializes Pebble. Later opens
pass no definitions; the persisted schema and existing indexes are reused.

Applications that intentionally keep data and indexes at separate custom paths
can use `database.OpenDirs(dataDir, indexDir, decode, options)`; that form also
accepts a pre-created legacy index.

An existing 0.5 database rejects missing `schema.dat`, incompatible index
definitions, unsupported formats, and unrecoverable journal state. See
[`FORMAT.md`](FORMAT.md) for the exact compatibility matrix.

## Projection contract

The application owns the meaning of canonical records and index projections.
`database` never interprets application-specific record bytes.

`Decoder` receives durable projection bytes and returns a complete
`index.Entry`. The projection must contain every association required to remove
or replace the previous indexed state. It must be deterministic across process
restarts.

```go
func decodeProjection(p []byte) (index.Entry, error) {
    if len(p) != 9 {
        return index.Entry{}, database.ErrCorruptJournal
    }
    return index.Entry{
        ValueID: binary.BigEndian.Uint64(p[:8]),
        Bitmaps: []index.BitmapValue{{
            Namespace: schema.NamespaceTag,
            Value:     bitmapidx.EnumBytesValue(p[8:9]),
        }},
    }, nil
}
```

`Options.DecodeInto` is the allocation-conscious form. It takes precedence over
`Decoder`; decoded projections may borrow the supplied scratch only until the
coordinator finishes the current journal batch.

### Canonical projection modes

- `CanonicalProjectionInline` stores projection bytes with the application
  record. It is the default for a fresh database.
- `CanonicalProjectionExternal` stores only the application record and requires
  `ProjectFromRecord` or `ProjectInto` to reconstruct projections.
- The mode is persisted in `schema.dat`. Reopening with a different explicit
  mode fails with `ErrCanonicalModeMismatch`.
- Projection-version migrations require external mode and a projector callback.

## Commit and indexing

```go
generation, err := db.Commit(database.Mutation{
    Entity:     schema.EntityImage,
    ValueID:    id,
    Record:     canonicalRecord,
    Projection: encodedIndexEntry,
})
if err != nil { return err }
if err := db.WaitIndexed(ctx, generation); err != nil { return err }
```

`Commit` atomically writes the canonical value, the journal entry, and the next
committed generation in one synced Pebble batch. A failed commit consumes no
sequence. Duplicate `(Entity, ValueID)` entries in one call are rejected; submit
revisions separately.

`WaitIndexed(ctx, generation)` waits until the derived index is durable and
visible through `generation`. `Sync(ctx)` waits for every generation committed
before the call. Ordinary searches may lag the latest canonical commit until
the wait completes.

`DB.Checkpoint()` checkpoints derived indexes and truncates engine journals
where supported. Canonical Pebble data is already durable. Run it during idle
periods; it does not replace `WaitIndexed`.

### Delete and reinsert

A delete writes a canonical tombstone and a journal delete. The tombstone keeps
the old inline projection or external canonical record so a later reinsertion can
remove the previous indexed associations correctly.

```go
if _, err := db.Commit(database.Mutation{
    Entity: schema.EntityImage, ValueID: id, Delete: true,
}); err != nil { return err }

if _, err := db.Commit(database.Mutation{
    Entity: schema.EntityImage, ValueID: id,
    Record: record, Projection: projection,
}); err != nil { return err }
```

### Schema changes

Namespace additions are startup-only. Close the database, supply the complete
target through `Options.Definitions` on the next open, and let `database`
publish PREPARED/ACTIVE schema state and rebuild index resources. Removed,
renamed, retyped, reconfigured, or reassigned existing namespaces are rejected.
Projection-version changes require the explicit `ProjectionMigration` path and
an external projector.

## Read canonical records

```go
record, err := db.Get(schema.EntityImage, id)
```

`Get` returns caller-owned bytes without projection metadata. A tombstoned or
missing ID returns `store.ErrNotFound`.

## Search integration

Search remains a separate package so applications can keep explicit control over
plans, workspaces, destination buffers, and execution mode:

```go
engine, err := search.New(db.Index())
if err != nil { return err }

clause, err := engine.Values(schema.NamespaceTag, bitmapidx.EnumStringValue("favorite"))
if err != nil { return err }

plan, err := engine.Compile(schema.EntityImage, clause)
if err != nil { return err }

workspace := search.NewWorkspace(1024)
ids, err := plan.AppendIDs(ctx, nil, workspace)
```

- Every `Engine` plan execution pins one coherent published snapshot across its
  participating indexes.
- Search does not fetch canonical records. Call `db.Get` only after selecting
  the IDs that require materialization.
- `search.Executor` pools workspaces for services; the caller still owns plans,
  destinations, and result buffers.
- Direct `search.Compile` plans are lower-level and read live engine views.
  Keep their indexes open.
- For text-to-image vector search with an external encoder, including vector
  projections and `Plan.RankVector`, see the root
  [Text search with external embeddings](../README.md#text-search-with-external-embeddings)
  guide.

## Ownership, concurrency, and consistency

- `DB` is safe for concurrent callers except where a method documents exclusive
  ownership of a batch, workspace, or report.
- `Mutation.Record` and `Mutation.Projection` are caller-owned and may be reused
  after `Commit` returns.
- `DB.Get` returns caller-owned bytes.
- `DB.Index()` returns the live derived store. Treat it as read-only and keep it
  open while plans or executors reference it.
- `DB.Close` rejects new operations, drains the coordinator, closes the index,
  and releases database locks. It may wait for active query plans to release
  their pinned snapshots; drain long-running reads before close.
- A search plan sees one published generation for the duration of one
  execution. Separate pages or separate executions may observe newer writes.
- Cancellation is reported through the context. `WaitIndexed` does not poll and
  returns the coordinator error if indexing fails permanently.

### Error families

Exported sentinels are suitable for `errors.Is`:

| Error | Meaning |
|---|---|
| `ErrInvalidMutation` | Invalid entity, ID, projection length, or option |
| `ErrClosed` | Database already closed |
| `ErrJournalFull` | `MaxPending` limit reached |
| `ErrSequenceOverflow` | Generation space exhausted |
| `ErrRecoveryRequired` | Startup reconciliation or index recovery required |
| `ErrCorruptJournal` | Invalid journal sequence, payload, or canonical state |
| `ErrUnsupportedJournalVersion` | Unsupported journal format |
| `ErrUnsupportedCanonicalVersion` | Unsupported canonical value format |
| `ErrCanonicalModeMismatch` | Reopen requested a different projection mode |
| `ErrCorruptDatabaseConfig` | Corrupt persisted database configuration |
| `ErrProjectionUnavailable` | External projection callback required but absent |

`store.ErrNotFound` identifies a missing or tombstoned canonical record.
Unexpected storage errors are wrapped rather than converted to not-found.

## Recovery

`Options.Recovery` defaults to `RecoveryManual`: an unhealthy index returns
`ErrRecoveryRequired` and preserves canonical data and the journal.

`RecoveryAutomatic` performs a synchronous startup rebuild before the DB is
published. The rebuild scans canonical projections, flags unsupported or corrupt
state, and replaces the complete unified index using a verified staging
directory. It does not attempt selective namespace replacement, and it never
modifies canonical records.

`DB.RecoveryStatus()` reports namespace health and the acknowledged generation.

### Recovery units

Bitmap namespaces share one physical bitmap store and timestamp namespaces share
one physical time store, so selective namespace replacement is not safe in 0.5.
Recovery uses the complete unified-index rebuild. The activation and staging
boundaries are documented in [`FORMAT.md`](FORMAT.md).

## Limits and options

| Option / limit | 0.5 behavior |
|---|---|
| `Options.BatchSize` | Defaults to 64; values above 64 are rejected. This is the per-index-apply batch, not the total mutations allowed in one `Commit`. |
| `Options.MaxPending` | Bounds unacknowledged journal lag; zero means unbounded. |
| `Options.MaxProjectionBytes` | Bounds copied before+after projection bytes in one coordinator batch; default 1 MiB. |
| `Options.OversizedRetentionBytes` | Bounds the one retained oversized projection buffer; default 4 MiB. |
| IDs | `ValueID` zero is reserved. Entity IDs are allocated by `schema.dat`; explicit IDs are last-writer-wins and do not protect against already-issued concurrent IDs. |
| Vector IDs | Must fit the vector engine's `uint32` NodeID range; vector APIs remain experimental. |
| Journal | Journal v2 only; unsupported versions fail closed. |

## 0.5 persistence policy

- `schema.dat` and canonical record semantics are 0.5 compatibility contracts.
- Journal replay must remain correct for committed 0.5 databases.
- Derived index files are rebuildable caches, not permanent format promises.
- Pre-0.5 experimental databases are not automatically opened.
- See [`FORMAT.md`](FORMAT.md) and [`schema/FORMAT.md`](../schema/FORMAT.md)
  for the versioned details.

## Example

A runnable end-to-end example is
[`cmd/imageidx/examples/database`](../cmd/imageidx/examples/database/main.go).
