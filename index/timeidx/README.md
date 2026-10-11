# timeidx — exact timestamp index

`timeidx` maps **one exact timestamp per `common.ValueID` per namespace** and
answers half-open `[start, end)` range queries in `(TimeKey, ValueID)` order.

It is a namespace-aware, copy-on-write, mmap-backed index with lock-free readers
and one serialized writer per namespace. There is no WAL, delta layer, background
compactor, or queue: a successful persistent `Apply` has already committed a
complete immutable generation before it returns. Both `DurabilityOS` and
`DurabilitySync` receive that synchronous commit guarantee.

See [`BENCHMARKS.md`](BENCHMARKS.md) for measured search, write, reopen and
shard-size evidence, and the root [`TimeIdx_Design.md`](../../TimeIdx_Design.md)
for the full design rationale. The implemented design supersedes any historical
PR notes.

## Timestamp encoding

`TimeKey` is a `uint64`: **exact nanoseconds since 1900-01-01T00:00:00Z**. All 64
input bits are stored — no shift, bucket, or quantization. `Entry.Timestamp`,
mutation arguments and search bounds keep their `uint64` signatures with that
unit.

`EncodeTime`/`DecodeTime` validate calendar years **1900–2100** inclusive and use
whole Unix seconds relative to the 1900 epoch plus a nanosecond remainder (never
`UnixNano` or `time.Duration` arithmetic). Timezone-free local/EXIF times must be
resolved to UTC by the caller; equality and order concern UTC instants.

| Instant | Key |
|---|---:|
| 1900-01-01 UTC | 0 |
| 1970-01-01 UTC | 2,208,988,800,000,000,000 |
| 2100-01-01 UTC | 6,311,433,600,000,000,000 |
| 2101-01-01 UTC (excluded by the helpers) | 6,342,969,600,000,000,000 |

The low-level index accepts opaque full-width keys, including the extrema,
without converting them. Applications wanting calendar validation use the
conversion helpers. `TimestampShift`, `DefaultBlockSize` and `DefaultDeltaCapacity`
survive only as deprecated source-compatibility constants; none affects indexing.

## Public API

```go
// In-memory or one persistent namespace file.
index, err := timeidx.NewMemory(spec, config)
index, err := timeidx.NewStore(dir, spec, config, common.DurabilityOS) // create
index, err := timeidx.OpenStore(dir, config, common.DurabilityOS)     // reopen

batch := timeidx.NewBatch(64)
batch.Append(timeidx.Insert(id, timestamp))       // upsert by ID
batch.Append(timeidx.Move(id, oldTS, newTS))      // atomic replace; oldTS advisory
batch.Append(timeidx.Delete(id, timestamp))       // remove by ID; timestamp advisory
if err := index.Apply(batch); err != nil { /* ... */ }
batch.Reset()                                     // reuse caller-owned storage

results := index.Search(dst, start, end)          // exact [start, end)
results = index.SearchExact(dst, start, end, lookup)
count := index.EstimateRange(start, end)
```

- `Spec{Namespace, Name, Flags, Entries}` carries the identity. `Entries` seed the
  initial shards; duplicate IDs are rejected with `ErrDuplicateImageID`.
  `Spec.NamespaceDef()` produces the `common.NamespaceDef` to register in a
  `common.Schema`.
- `Metadata()` returns namespace, flags, generation, compatibility offset,
  logical count and shard count. `SourceGeneration()` returns the completeness
  watermark.
- `Insert` is ID-authoritative. `Delete` removes by ID even when the asserted
  timestamp is stale. `Move` removes any old location and inserts the new one
  atomically; `from` is advisory. The **last operation for an ID in a batch
  wins**. Absent deletes and identical upserts commit nothing.
- `Batch` is reusable, caller-owned and fixed-capacity. `Append` returns false
  when full, in flight, or given an invalid mutation. `Apply` takes exclusive
  batch ownership for its duration and returns `ErrBatchInFlight` on reuse.
- `Search` appends matches in `(TimeKey, ValueID)` order without duplicates. With
  sufficient `dst` capacity it does not allocate. `start >= end` is empty and
  `end = ^uint64(0)` excludes that key, so no `end+1` can overflow.
- `SearchExact` keeps an optional canonical-data callback for compatibility; it
  is not needed to correct quantization and is not transactionally pinned with
  external metadata. `ExactTimestampFunc` reports `(timestamp, ok)` for an ID.
- `EstimateRange` returns the exact snapshot-local candidate count. A separate
  estimate and search may observe different committed snapshots.

Compatibility barriers: `Checkpoint`, `Sync`, `Vacuum` and `Compact` never
advance the generation or rewrite data (writes are already durable). The first
three require a persistent index and return `ErrNotPersistent` otherwise;
`Compact` only surfaces a latched failure.

## Copy-on-write shard model

A published snapshot is one immutable sorted sequence of shards, each a pair of
parallel columns:

```text
shard = Times[count] uint64   // strictly increasing by (time, ID)
        ValueIDs[count] uint64 // 16 payload bytes per entry
```

Shards are size-bounded by `Config.MaxShardBytes` (default **4 KiB**). Equal
timestamps may span shards; the full `(time, ID)` boundary stays strictly
increasing. `MinID`/`MaxID` prune unaffected shard scans.

`Apply` normalizes the batch, removes idempotent upserts, routes additions by
their `(time, ID)` boundary, rewrites only shards that receive additions or
contain touched IDs, splits outputs by balanced entry position (including
all-equal timestamps), and coalesces adjacent **new** undersized shards. Old
shards keep their extents and descriptors; unchanged shards are deliberately not
merged just to improve occupancy. The result is published through one
`atomic.Pointer[snapshot]` store.

Reader-visible state is never mutated. Heap snapshots own fresh columns;
persistent commits copy writer-private merge buffers into mmap before publication.

## Persistence format (version 5)

Each namespace is **one file** named with four lowercase hexadecimal namespace
digits (for example `0001.timeidx`), plus an empty advisory `<name>.lock`. There
are no shard files, manifests, journals, or WALs. The family magic is `TIM4` and
the format version is **5**; version 4 is explicitly unsupported and must be
rebuilt from canonical timestamps, never silently reinterpreted.

- Superblocks use the shared 512-byte CRC32C envelope: slot A in bytes 0–511 and
  slot B in 512–1023. The arena uses shared 512-byte `pagealloc` runs.
- The **64-byte payload** is:

  | Offset | Width | Field |
  |---:|---:|---|
  | 0 | 2 | Namespace |
  | 2 | 2 | Encoding ID: 1 = exact 1900-epoch nanoseconds |
  | 4 | 4 | Spec flags |
  | 8 | 8 | Directory byte offset (zero for empty) |
  | 16 | 4 | Shard count |
  | 20 | 4 | Directory CRC32C |
  | 24 | 8 | Logical entry count |
  | 32 | 8 | Arena byte length at this generation |
  | 40 | 8 | Deprecated offset metadata (unused by encoding) |
  | 48 | 8 | Maximum permitted shard payload for this generation |
  | 56 | 8 | SourceGeneration completeness watermark |

- The **directory** is an immutable sorted array of 56-byte records
  (`storage.ShardDescriptorBytes`): FirstTime, FirstID, payload offset, count,
  time-column CRC32C, ID-column CRC32C, reserved (zero), MinID, MaxID.
- A shard payload is `Times[count]` immediately followed by `ValueIDs[count]`.
  All persisted fields are little-endian; typed mmap columns require a
  little-endian host and 8-byte alignment.
- No free-space record is persisted. Recovery reconstructs the allocator from the
  union of recoverable generation extents, reserving both superblocks.

### Commit sequence

```text
allocate fresh shard/directory extents from the current protected allocator
write changed SoA shards; retain unchanged offsets and checksums
encode and checksum the new directory
sync mapping + file                          // payload-before-root barrier
compute the new + previous protected live set
write the inactive checksummed superblock
sync mapping + file                          // root durability barrier
publish the complete new snapshot
wait for readers holding the previous snapshot
install the reconstructed allocator; unmap retired mappings
```

| Outcome | Behavior |
|---|---|
| Before root write | Aborted: roll back new extents; the old snapshot stays authoritative |
| Partial root write or root sync error | Uncertain: keep candidate extents, keep the old view, latch writes until close/reopen |
| After root sync, before publication | Durable: recovery selects the complete new generation; publication has no fallible step |

Open validates both envelopes, identity, encoding, generation, directory CRC and
placement, arena bounds, count arithmetic, shard CRCs, pair order, ID bounds,
global ID uniqueness and overlapping extents. Dense ID domains use a bounded
validation bitset; sparse domains use a sorted scratch column. The older valid
generation is retained for fallback.

### Growth and close

Arena growth extends the same file, adds a new mapping, and retains the old one;
unchanged shards rebind to slices of the new mapping without copying. Reader
slices never remap in place, and prior mappings are unmapped only after
publication and reader drain. `Config.MaxFileBytes` selects initial sparse
capacity (default 4 MiB), not an immutable limit. `Close` marks the index closed
under the writer mutex, drains prior readers, then closes current and retired
mappings and releases the ownership lock.

These guarantees rely on the OS/filesystem honoring sync ordering. Tests prove
process-crash and error-injection behavior, not physical power-cut safety on
every device. The two 512-byte envelopes share an OS page, so CRC fallback does
not protect against corruption of both slots.

## Source-completeness watermark

`SourceGeneration()` is a caller-supplied database generation carried in the same
superblock as the shard directory.

`ApplyWithGeneration(batch, generation)` commits the batch and the watermark
atomically. The application asserts that **every applicable DB change through the
supplied generation is incorporated**; `timeidx` cannot infer gaps or DB
transaction ordering. Decreasing generations are rejected (`ErrSourceGeneration`),
equal generations allow idempotent retries, and empty batches still persist
advancement. Legacy `Apply` preserves the current watermark.

`CheckSourceGeneration(required)` returns `ErrIndexStale` when behind and
`ErrClosed` after shutdown; staleness is neither corruption nor an I/O error.
`NamespaceInit.RequiredSourceGeneration` checks completeness before a namespace
is exposed: a stale member stays configured but unavailable while healthy members
remain accessible. Zero is the initial, unrecorded watermark.

## Lifecycle and namespaces

Resolve an index once, then call `Search`/`Apply` directly; manager locks never
enter searches or writes.

- `NewNamespaceStore(dir, config, durability)` builds a startup-only registry.
  `CreateNamespace`, `CreateNamespaceWithSpec`, `OpenNamespace` and
  `CloseNamespace` register or unregister before `SealStartup`.
- `SealStartup` publishes the immutable sorted registry permanently. After it,
  membership changes return `ErrNamespaceFrozen`.
- `Namespace` and `NamespaceError` are lock-free lookups against the published
  registry; `Namespace` never acquires the lifecycle mutex.
- `InitializeStore` validates the whole configuration, initializes each namespace
  independently, checks `RequiredSourceGeneration`, and seals. Per-namespace
  failures are returned in `[]NamespaceResult` so one corrupt member cannot block
  healthy ones.
- `Store.Close` disables every owned index through a shared atomic flag before
  draining them; the published registry is never mutated. Repeated closes are
  idempotent.
- `OpenStore` accepts a directory with exactly one namespace (otherwise
  `ErrAmbiguousNamespace`); `OpenNamespace` opens a specific one.
- Standalone namespace zero is supported only by `NewStore` for old callers and
  must not be registered as a schema namespace.

### Corruption and rebuild

`ErrRebuildRequired` wraps an integrity cause (`ErrCorrupt` or
`ErrNamespaceMismatch`). I/O, unsupported-format and configuration errors are
**not** rebuild signals. Rebuild is application-controlled and offline:

1. Detect missing/stale/corrupt/unsupported members from initialization results.
   `ErrIndexStale` permits replay instead of a rebuild.
2. Obtain a consistent canonical DB snapshot and coordinate later writes.
3. Create/import a separate namespace file from canonical exact timestamps.
4. Commit the represented DB generation with `ApplyWithGeneration` (an empty
   batch can mark completeness after a bulk import).
5. Verify the staged namespace and its required source generation.
6. With final and staging owners closed, call `PublishRebuild` to atomically
   rename the staged file on the same filesystem and sync both directories.
7. Reopen, check against the DB checkpoint, then seal/publish the registry.

Pre-rename failures preserve the original and release both locks. Post-rename
sync errors return `ErrRecoveryRequired` joined with the original cause — never
success or corruption. `Verify`/`VerifyNamespace` validate a closed, quiescent
directory read-only.

## Concurrency and lifetime

Readers acquire a two-slot `storage.ReaderTracker` token **before** checking
closed and loading the snapshot, and release it in reverse order. A reader either
observes `closed` before touching mapped data or is counted in the slot `Close`
drains, so there is no unprotected gap and no per-query allocation. Callbacks
passed to `SearchExact` must not synchronously write or close the pinned index.

| Operations | Access | Protection |
|---|---|---|
| `Search`, `SearchExact`, `EstimateRange` | mmap time/ID arrays | reader token → closed check → snapshot load → deferred release |
| `Metadata`, `Generation`, `Len`, `SourceGeneration` | heap snapshot counters and slice header only | atomic snapshot load; no mapped dereference |
| `Apply`, `ApplyWithGeneration` | scratch, allocator, mmap writes | per-index writer mutex; reject closed/failed before mapped access |
| `Checkpoint`, `Sync`, `Vacuum`, `Compact` | heap state under writer mutex | compatibility barriers only |
| `Store.Namespace`, `NamespaceError` | immutable registry + closed atomics | atomic registry load, no registry mutex |
| `Close` (Index/Store) | mappings, locks, scratch | writer mutex; set closed, drain readers, unmap, release locks |

## Invariants

- Each `common.ValueID` has at most one timestamp per namespace.
- Parallel `Times`/`ValueIDs` columns always have equal lengths and are strictly
  increasing by `(TimeKey, ValueID)`.
- A published snapshot is immutable; readers never observe partial batches.
- A batch is accepted atomically or rejected without partial mutation.
- Steady-state range search with caller capacity does not allocate.
- A failed uncertain root publication latches writes until close/reopen.

## Validation

```sh
go build ./...
go test ./index/timeidx/...
go test -race ./index/timeidx/...
go vet ./index/timeidx/...
go test ./index/timeidx -run '^$' -fuzz '^FuzzNamespaceGeneration$' -fuzztime=10s
```

The opt-in `TIMEIDX_SCALE` benchmarks and correctness tests add 1M/10M fixtures;
see `BENCHMARKS.md`.
