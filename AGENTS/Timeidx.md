# Timestamp Index (`timeidx`)

> **Design note.** Current implementation: `index/timeidx`. Verify APIs and
> format details against `index/timeidx/README.md` and tests.

This document describes the implemented `timeidx` package. It is a summary; the
package [`index/timeidx/README.md`](../index/timeidx/README.md) is authoritative
for the format and lifecycle, and
[`index/timeidx/BENCHMARKS.md`](../index/timeidx/BENCHMARKS.md) holds the measured
evidence.

## Purpose

`timeidx` maps **one exact timestamp per `schema.ValueID` per namespace** and
answers half-open `[start, end)` range queries in `(TimeKey, ValueID)` order. It
is namespace-aware, copy-on-write, and mmap-backed, with lock-free readers and
one serialized writer per namespace.

There is no WAL, delta layer, background compactor, or queue. A successful
persistent `Apply` has already committed a complete immutable generation and both
sync barriers before returning, under `DurabilityOS` and `DurabilitySync` alike.

## Timestamp encoding

`TimeKey` is a `uint64`: **exact nanoseconds since 1900-01-01T00:00:00Z**. All 64
input bits are stored; there is no shift, quantization, or bucketing.

`EncodeTime`/`DecodeTime` validate calendar years **1900–2100** inclusive using
whole Unix seconds relative to the 1900 epoch plus a nanosecond remainder (never
`UnixNano` or `time.Duration`). Timezone-free local/EXIF times must be resolved to
UTC by the caller. `TimestampShift`, `DefaultBlockSize` and `DefaultDeltaCapacity`
remain only as deprecated source-compatibility constants and do not affect
encoding.

## Public API

```go
index, err := timeidx.NewMemory(spec, config)                       // in memory
index, err := timeidx.NewStore(dir, spec, config, schema.DurabilityOS) // create file
index, err := timeidx.OpenStore(dir, config, schema.DurabilityOS)      // reopen

batch := timeidx.NewBatch(64)
batch.Append(timeidx.Insert(id, timestamp))
batch.Append(timeidx.Move(id, oldTS, newTS))
batch.Append(timeidx.Delete(id, timestamp))
err = index.Apply(batch)

results := index.Search(dst, start, end)
results = index.SearchExact(dst, start, end, lookup)
count := index.EstimateRange(start, end)
```

- `Spec{Namespace, Name, Flags, Entries}` carries the stable identity;
  `Spec.NamespaceDef()` yields the `schema.NamespaceDef` to register in a
  `schema.Schema`. Initial entries are sorted by `(time, ID)`; duplicate IDs are
  rejected with `ErrDuplicateImageID`.
- `Metadata()` returns namespace, flags, generation, compatibility offset,
  logical count and shard count. `SourceGeneration()` returns the completeness
  watermark.
- Mutations are sealed values: `Insert` upserts by ID, `Delete` removes by ID
  even with a stale asserted time, and `Move` atomically replaces a time with an
  advisory `from`. The last operation for an ID in a batch wins. Absent deletes
  and identical upserts commit nothing.
- `Batch` is reusable, fixed-capacity, caller-owned storage. `Append`/`Reset`
  return false when full or in flight; `Apply` holds exclusive batch ownership
  and returns `ErrBatchInFlight` on reuse.
- `Search` appends unique matches in `(TimeKey, ValueID)` order and does not
  allocate when `dst` has capacity. `start >= end` is empty; `end = MaxUint64`
  excludes that key. `SearchExact` keeps an optional canonical callback for
  compatibility. `EstimateRange` returns the exact snapshot-local count.
- `Checkpoint`, `Sync`, `Vacuum` and `Compact` are compatibility barriers that
  never advance the generation or rewrite data (writes are already durable).

## Copy-on-write shard model

A published snapshot is one immutable sorted sequence of shards, each a pair of
parallel columns (`Times[] uint64` then `ValueIDs[] uint64`, 16 payload bytes per
entry), strictly increasing by `(TimeKey, ValueID)`. Equal timestamps may span
shards. Shards are size-bounded by `Config.MaxShardBytes` (default **4 KiB**) and
carry `MinID`/`MaxID` for scan pruning.

`Apply` normalizes the batch, removes idempotent upserts, routes additions by
their `(time, ID)` boundary, rewrites only shards that receive additions or
contain touched IDs, splits outputs by balanced entry position, and coalesces
adjacent **new** undersized shards. Unchanged shards keep their extents and are
never merged merely to improve occupancy. The replacement is published through a
single `atomic.Pointer[snapshot]` store; nothing reachable from a published view
is mutated.

## Persistence (format 5)

Each namespace is **one file** named with four hexadecimal namespace digits
(`0001.timeidx`), plus an empty advisory `.lock`. There are no shard files,
manifests, journals, or WALs. Family magic is `TIM4`; the format version is **5**
and version 4 is explicitly unsupported (rebuild from canonical timestamps).

- Superblocks use the shared 512-byte CRC32C envelope (slot A bytes 0–511, slot B
  512–1023); the arena uses shared 512-byte `pagealloc` runs.
- The 64-byte payload holds namespace, encoding ID (1 = exact 1900-epoch
  nanoseconds), flags, directory offset, shard count, directory CRC32C, logical
  entry count, arena length, deprecated offset metadata, the persisted shard
  bound, and bytes 56–63 the source-completeness watermark.
- The directory is an immutable sorted array of 56-byte
  `storage.ShardDescriptor` records: FirstTime, FirstID, payload offset, count,
  time-column CRC32C, ID-column CRC32C, reserved, MinID, MaxID.
- No free list is persisted. Open reconstructs the allocator from the union of
  recoverable generation extents and retains the older valid generation as
  fallback. It validates both envelopes, identity, encoding, generation,
  directory CRC/placement, arena bounds, shard CRCs, pair order, ID bounds,
  global ID uniqueness and overlapping extents.

Commit sequence: allocate fresh extents → write changed shards and directory →
sync (payload barrier) → write the inactive checksummed superblock → sync (root
barrier) → publish snapshot → wait for prior readers → install the reconstructed
allocator and unmap retired mappings. A partial root write or root-sync error is
**uncertain**: candidate extents stay protected and writes latch until
close/reopen. Arena growth extends the same file, adds a mapping, and rebinds
unchanged shards without copying; old mappings are unmapped only after reader
drain. `Config.MaxFileBytes` selects initial sparse capacity (default 4 MiB), not
a hard limit.

## Source-completeness watermark

`ApplyWithGeneration(batch, generation)` commits mutations and a caller-supplied
DB completeness watermark in the same superblock. The application asserts every
applicable DB change through that generation is incorporated; `timeidx` cannot
infer gaps or DB transaction ordering. Decreasing generations return
`ErrSourceGeneration`; equal generations allow idempotent retries; empty batches
still persist advancement; legacy `Apply` preserves the current watermark.

`CheckSourceGeneration(required)` returns `ErrIndexStale` when behind (never
corruption or I/O) and `ErrClosed` after shutdown.
`NamespaceInit.RequiredSourceGeneration` checks completeness before a namespace
is exposed; stale members stay configured but unavailable.

## Lifecycle and rebuild

Registration is startup-only. `NewNamespaceStore` plus
`CreateNamespace`/`CreateNamespaceWithSpec`/`OpenNamespace`/`CloseNamespace`
build a registry that `SealStartup` (or `InitializeStore`) publishes immutably
and permanently; later membership changes return `ErrNamespaceFrozen`.
`Namespace`/`NamespaceError` are lock-free lookups against the published registry.
`Store.Close` disables every owned index through a shared atomic flag before
draining them and never mutates the published registry.

`ErrRebuildRequired` wraps an integrity cause only; I/O and configuration errors
are not rebuild signals. Rebuild is application-controlled and offline:
`PublishRebuild` renames an independently built and verified staged file on the
same filesystem and syncs both directories. Pre-rename failures preserve the
original; post-rename sync errors return `ErrRecoveryRequired` joined with the
original cause. `Verify`/`VerifyNamespace` validate a closed directory read-only.

## Concurrency and invariants

Readers acquire a `storage.ReaderTracker` token before checking closed and
loading the snapshot, then release it in reverse order, so a reader either sees
`closed` before touching mmap or is counted in the slot `Close` drains. Metadata
and counter accessors read only heap snapshot state. `SearchExact` callbacks must
not synchronously write or close the pinned index.

- One exact timestamp per `schema.ValueID` per namespace.
- Parallel time/ID columns stay equal-length and strictly increasing by
  `(TimeKey, ValueID)`.
- A batch is accepted atomically or rejected without partial mutation.
- Steady-state range search with caller capacity does not allocate.
- A latched uncertain commit rejects further writes until close/reopen.

See [`index/timeidx/README.md`](../index/timeidx/README.md) for the full API and
lifecycle table, and root `TimeIdx_Design.md` for design rationale.
