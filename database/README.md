# database: authoritative records and derived indexes (alpha)

`database.DB` coordinates `schema.dat` (entity IDs and all logical namespace
definitions), Pebble (canonical bytes, journal, operational generations), and
the unified index (physical engines and recovery metadata). The initial index
may be created with standalone `index.Create`; database startup validates and
retires its old `schema.bin` after establishing schema.dat. Subsequent index
opens consume central definitions. Allocation-file format v1 returns
`schema.ErrVersion`; no namespace-scoped counter migration is attempted.

`database.Open` takes an exclusive database lock. On new databases it validates
the index and writes schema.dat before creating Pebble; Pebble never receives
a serialized schema registry. Existing databases validate old Pebble and index
copies during their one-time migration, preserve namespace IDs and allocator
reservations, remove those copies, then durably mark retirement in schema.dat.
Later opens do not read old Pebble schema keys. Losing schema.dat after migration
fails closed, without scanning records to invent IDs.

Additions supplied through `Options.Definitions` are allowed only at startup.
Existing definitions must remain an identical prefix; removal, renaming,
reassignment, or changing configuration fails. schema.dat publishes the full
target as PREPARED before physical index work; the previous superblock remains
ACTIVE. The index is rebuilt into a verified staging directory from canonical
projections and activated with the index recovery marker. Once every namespace
is healthy and durable, schema.dat publishes ACTIVE. A crash can repeat staging
or finish a verified activation; recovery loads the durable target from
schema.dat without requiring the caller's original options. An index that
cannot be rebuilt from canonical projections fails closed. A KV-only addition
reuses the healthy index unchanged: the coordinator still publishes PREPARED
and ACTIVE, but no index rebuild or backfill runs. Canonical namespaces
come from persisted `entity-<kind>` definitions and need not match EntityKind
ordinals; `Options.CanonicalNamespaces` never renumbers existing databases.

`NextID` issues unique IDs from the entity allocator. Explicit IDs in `Commit`
use caller-owned, last-writer-wins canonical record semantics. `EnsureMinimum`
protects future allocations, **not** IDs already issued to another goroutine.
A bulk import must finish and reconcile its maximum before concurrent `NextID`
for that entity using `PrepareImport(entity, maximum)`; callers must coordinate
this phase themselves. Runtime
external-ID insertion cannot promise uniqueness against outstanding issued IDs.
database directory is exclusively owned by this package: its fixed `entity-*`
collections contain canonical records, namespace 65533 holds ordered journal
entries, and 65534 holds generation metadata. Do not mutate these namespaces
through a separate `store.Store` handle. `db.Index()` exposes the derived
store for searches; **do not write to or close it directly** while the
coordinator is running, or completeness claims no longer cover every mutation.

## Startup recovery (experimental)

`Options.Recovery` defaults to `RecoveryManual`: an unhealthy index returns
`ErrRecoveryRequired`, leaving canonical data and journal intact.
`RecoveryAutomatic` can perform a **synchronous startup** rebuild if the
derived index is missing, corrupt, has an interrupted recovery marker, or
cannot establish the Pebble acknowledgment. A healthy index merely behind the
database instead uses the journal publisher; it does not rebuild. `DB` also
exposes `RecoveryStatus()` for per-namespace `RecoveryHealthy`,
`RecoveryBehind`, `RecoveryRequired`, or `RecoveryFailed` states and durable namespace source
generations. No ordinary database reader or writer receives a DB while its
startup rebuild is in progress.

An existing schema.dat supplies index definitions for rebuild, even if the
entire index directory or old `schema.bin` is missing. A pre-schema.dat legacy
database without either copy cannot recover definitions and fails closed. After
the retirement marker, the old Pebble backup is never consulted.

The current physical rebuild reconstructs **all declared namespaces**, not
just the damaged logical namespace. Bitmap namespaces share one segment and
WAL, and every namespace's liveness uses that store; targeted physical
activation is not implemented. The original directory remains untouched until
a replacement has been completely scanned, durably assigned the canonical
generation, structurally verified, closed, and reopened. The replacement lives
at `<indexDir>.rebuild`; a checksummed `<indexDir>.activation` records BUILDING
or VERIFIED, the source generation, and a fingerprint of the index definitions
from schema.dat. Reopen validates this fingerprint before any rename. Only a
VERIFIED replacement may replace `<indexDir>`; the previous generation is
temporarily moved to `<indexDir>.previous`. Every rename is followed by parent
directory sync. On restart the activation record finishes or restarts the
operation, and cleanup only removes this package's owned staging/previous
directories. This uses *directory* renames, not a single-file rename across
multi-file index families.

The canonical scan streams in bounded index batches (default 64) and skips
deleted tombstones. Namespace definitions and entity ownership are checked
before indexing, so two entities with the same numeric ID cannot mix. SimHash
must still use dense IDs; a sparse record returns `ErrUnsupportedRebuild` and
leaves the original index and canonical records intact. No ImageID remapping
is performed.

**Limits:** activation is safe only before `Open` publishes the index to any
caller. There is no runtime `Rebuild` or hot-swap API: compiled search plans
could otherwise retain old mmap-backed pointers. This version does not permit
canonical writes during a rebuild, independently replace healthy non-bitmap
engines, implement record-by-record logical verification for every index
family, or meet the full PR #22 crash/performance matrix. Do not mark automatic
recovery feature-complete on the strength of these startup tests alone.

## Usage

The application supplies a stable, deterministic binary projection encoder and
`database.Decoder` for its domain. The projection represents a **complete**
`index.Entry` for an entity; both old and new projections must decode after a
restart. Keep photography-specific schemas out of this package. For example,
with an image-tag namespace `101`, a projection can be nine bytes (big-endian
image ID followed by one tag byte); the decoder maps those bytes to
`index.Entry{ValueID: id, Bitmaps: []index.BitmapValue{...}}`.

```go
// idxDir was initialized once using index.Create(idxDir, index.Spec{Schema: schema}).
db, err := database.Open(dataDir, idxDir, decodeProjection, database.Options{
    BatchSize: 64, MaxPending: 100_000,
})
if err != nil { return err }
defer db.Close()

generation, err := db.Commit(database.Mutation{
    Entity: schema.EntityImage, ValueID: imageID,
    Record: imageMetadata, Projection: encodedIndexEntry,
})
if err != nil { return err } // canonical commit + journal are already durable
if err := db.WaitIndexed(ctx, generation); err != nil { return err }

engine, err := search.New(db.Index())
if err != nil { return err }
clause, err := engine.Values(101, bitmapidx.EnumBytesValue([]byte("favorite")))
if err != nil { return err }
plan, err := engine.Compile(schema.EntityImage, clause)
if err != nil { return err }
ids, err := plan.AppendIDs(ctx, nil, search.NewWorkspace(128))
_ = ids
return err
```

`Commit(mutations...)` returns the last strictly increasing sequence, assigning
one sequence per operation and writing all records, journal entries, and the
next sequence in **one synced Pebble batch**. A failed batch consumes no
sequence. Duplicate `(Entity, ValueID)` in the same call is rejected; commit
such revisions separately. `Get` reads authoritative bytes; a deletion is
represented by a canonical tombstone so the old index projection survives a
later re-insertion of the same ID. `Commit` accepts up to the caller's memory
budget; the coordinator reads at most `Options.BatchSize` entries (default 64)
per index batch. `MaxPending` limits sequence lag and causes new commits to
return `ErrJournalFull` rather than buffering unbounded work. A zero limit is
unlimited.

`Options.DecodeInto` is an optional allocation-conscious decoder form; when it
is set, `Decoder` may be nil. The coordinator copies each journal entry's
before+after payload once into reusable 64 KiB blocks, retains roughly 1 MiB of
ordinary block capacity, and keeps at most one bounded oversized buffer.
`Options.MaxProjectionBytes` (default 1 MiB) bounds the copied before+after
bytes in one batch; a single oversized mutation is still processed alone.
`Options.OversizedRetentionBytes` (default 4 MiB) bounds the one reused oversized
buffer. The index copies any bytes it needs to retain; callers must not retain
views into coordinator storage. `DB.Stats()` exposes cumulative logical and
physical writer counters for benchmarks and diagnostics; it is not a
transactional snapshot.

## Journal, ordering and replay

Journal keys use `store.Key{Namespace: 65533, ID: sequence}`: `[uint16
namespace][uint64 big-endian sequence][uint16 flags=0]`. Payload v1 is
`[version:1][op:1][entity:1][valueID:8][beforeLen:4][afterLen:4]
[beforeProjection][afterProjection][crc32:4]`. The before projection is read
from the canonical value/tombstone while the writer lock is held. This avoids
guessing old hashes, locations or tags from the **current** record when
replaying an earlier update. Full canonical record bytes are not journaled;
the caller's compact, versioned projection may contain an embedding if the
index requires it. Projection bytes are also stored with canonical values to
permit a subsequent `Change{Before, After}` after journal cleanup. This is
intentional write amplification needed for correct explicit removals.

The coordinator is one bounded background publisher. It stages entries in an
`index.Batch` and calls `ApplyWithGenerationAll`, which confirms touched and
untouched healthy namespaces in one index manifest write; it does **not**
rewrite unchanged values. Projection diffs use typed deterministic sorting and a
linear merge; identical before/after associations are pruned by family before
staging a `Change`, including timestamp, geography, hash, bitmap and vector
values. Within one bounded journal interval the coordinator coalesces repeated
same-entity events when their initial/final state is equivalent and the engine
families are safe; SimHash/vector groups fall back to ordered sub-batches. Only
after the
store-wide `SourceGeneration()` is at least the batch's last sequence does it
sync the Pebble acknowledgment. Cleanup uses one Pebble range tombstone for
the acknowledged interval. On startup it checks the durable database
generation, acknowledgment, index health/recovery markers and source watermark
before replay. If the index watermark is **ahead** of the Pebble acknowledgment,
it can safely acknowledge that already-durable prefix. If the acknowledgment
is ahead of the recoverable index, it returns `ErrRecoveryRequired` and leaves
the journal intact. Startup reads generation keys, not the whole record store.

**Crash limitation:** a durable intent is written before applying an index
batch. If a crash occurs after intent but before complete store-wide index
durability, manual `Open` returns `ErrRecoveryRequired` instead of replaying
over unknown partial state. Automatic startup recovery replaces the physical
index from canonical projections. In particular, simhash append IDs are dense
and cannot be assumed idempotent. The replacement has no old recovery marker;
the live index's marker is not cleared merely by `Sync` or `Checkpoint`. A
journal checksum or sequence gap is reported as `ErrCorruptJournal` and never
skipped.

Search is **eventually consistent** after a canonical commit. Call
`WaitIndexed(ctx, generation)` or `Sync(ctx)` before requiring search
completeness. Waiters share a broadcast channel, respect cancellation and
receive permanent coordinator errors. `DatabaseGeneration` is the latest
committed sequence; `IndexedGeneration` is the durable acknowledged sequence;
the latter never exceeds the former. Searches do not acquire the canonical
writer lock. Cross-engine snapshots and atomic transactions are not supplied
by `index.Store`.

Tests include independent-process crashes before canonical commit, after
canonical commit, before index apply, after index durability, after
acknowledgment, and before cleanup. The incomplete intent case is deliberately
recovery-required: **do not represent this alpha as automatic recovery from
every mid-application crash.**

## Focused measurements

On an Apple M4 Pro (`go test ./database -run '^$' -bench
'^BenchmarkGenerationPublication$' -benchtime=30x -count=8`), `benchstat`
compared the former nonempty-plus-empty generation path with the integrated
publication path on the same two-namespace fixture: median **9.787 ms →
4.933 ms** per mutation (-49.59%, p<0.001), **6.261 KiB → 4.932 KiB**
allocated (-21.22%), **78 → 64** allocs/op. This isolates generation
publication, not all database ingestion. The 64-image end-to-end batch
measurement with a separate short run was about **2,869–2,890 images/s**;
these are local numbers, not a production throughput guarantee.

One measured startup-rebuild iteration reconstructed **10,000** enum records
in **179 ms** (~56k records/s) and **100,000** in **397 ms** (~252k records/s),
excluding fixture ingestion. These single observations are not a base-branch
`benchstat` comparison and do not validate other index families.
## ID allocation

`DB.NextID(entity)` uses the standalone `schema` package's single-file
`schema.dat` allocator, keyed by the entity's canonical namespace. Other
namespaces for that entity reference the same ID; they do not allocate it.
On first open, the database transfers the former Pebble allocator's durable
high-water marks (if present) and reconciles canonical records and tombstones
before publishing a synced migration marker. Later opens use only the schema
file and never scan canonical records to reconstruct issued IDs. A clean close
preserves the last issued ID; an unclean close skips unused reservations.
