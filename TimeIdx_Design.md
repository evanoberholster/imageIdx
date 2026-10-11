# TimeIdx namespace-aware mmap design and implementation

**Format update:** Source-watermark work introduces format 5. Version 4 is
explicitly unsupported, not silently upgraded. Bytes 56–63 of the payload now
hold `SourceGeneration`, atomically committed with the directory. See
[`index/timeidx/README.md`](index/timeidx/README.md) for the authoritative
completeness, staleness, rebuild and lifecycle contract.

**Lifecycle update:** Registration is startup-only through `SealStartup` (or
`InitializeStore`), and the runtime registry is immutable and sorted.
`Namespace` is the lock-free lookup API; runtime creation/removal is rejected.
The initial lifecycle description below is historical where it differs.

## Confirmed decisions

| Decision | Answer | Source |
|---|---|---|
| T01 | Exact unsigned nanoseconds since 1900-01-01 UTC; no quantization | User: A |
| T04 | Support 1900–2100, including pre-1970 instants | User |
| D01 | Changed persistent `Apply` completes a COW commit before returning | User: A |
| Namespace | `common.Namespace`; independent file, allocator, generation and lifetime | User requirements |
| Cardinality | One time per `common.ValueID` per namespace; repeated times allowed | User requirements |
| Atomicity | Whole-batch acceptance, publication and recovery; no cross-namespace transaction | User requirements |
| Shards | Adaptive composite-order size bounds; configurable maximum | User requirements |
| Default shard limit | 4 KiB after measured clustered/historical sweeps | Benchmark evidence |
| Mutations | Last input operation per ID wins; insert upserts, delete/move are ID-authoritative | Simplest implementation enforcing uniqueness |
| Legacy format | Explicit rejection; rebuild using canonical timestamps | Disclosed before implementation |

No unresolved timestamp/durability choices remain. Quantitative service-level
targets were not supplied; measurements are evidence, not invented SLAs.

Reference: merged GeoIdx PR #15 at baseline revision
`ae8fda19018d9ea76eed990a963aa40a8536a4b3`. Actual code, not the older
fixed-capacity comments, established the reference behavior.

## Timestamp and identifier encoding

`TimeKey` is a `uint64`. `Entry.Timestamp`, mutation arguments and search bounds
keep their existing `uint64` signatures. Their new semantic unit is **exact
nanoseconds elapsed since 1900-01-01T00:00:00Z**. No namespace or shard changes
the epoch. All 64 input bits are stored without shifting or truncation.

`EncodeTime` and `DecodeTime` validate calendar years 1900 through 2100,
inclusive. Conversion uses whole Unix **seconds** relative to the 1900 epoch,
then adds nanoseconds. It does not use `UnixNano`, signed nanosecond casts or
`time.Duration` subtraction. Timezone-free EXIF/civil times must be resolved
outside this index; equality and order concern UTC instants.

| Instant | Key |
|---|---:|
| 1900-01-01 UTC | 0 |
| 1970-01-01 UTC | 2,208,988,800,000,000,000 |
| 2100-01-01 UTC | 6,311,433,600,000,000,000 |
| 2101-01-01 UTC (excluded by conversion helpers) | 6,342,969,600,000,000,000 |

Thus the last nanosecond of 2100 is `6,342,969,599,999,999,999`; all arithmetic
fits `uint64`. The low-level index accepts opaque full-width keys, including
extrema, without converting them. Applications wanting date validation use the
conversion helpers. `TimestampShift=24` survives only as a deprecated source
compatibility constant: it no longer affects indexing. On nanoseconds that
shift would be 16.777216 ms, not 50 ms.

`common.ValueID` stays generic and full-width. Zero and MaxUint64 IDs are valid.
Multiple namespaces may independently contain the same ID and timestamp.

## Ownership and API compatibility

`NewNamespaceStore(dir, config, durability)` creates a lifecycle registry, not
an implicit namespace. `CreateNamespace`, `CreateNamespaceWithSpec`,
`OpenNamespace`, `CloseNamespace` and `Close` operate explicitly. Concurrent
opens through one manager return the same index. A per-namespace advisory lock
excludes another writer/process using that namespace path.

Resolve an index once and call `Index.Search`/`Apply` directly. Manager locks
never enter searches or writes. Its lifecycle mutex serializes registration
and close operations, which may wait for readers; unrelated resolved indexes
continue searching and writing. Register `Spec.NamespaceDef()` in the owning
`common.Schema` before sharing the collection with the planner.

Existing `NewMemory`, `NewStore`, `OpenStore`, `Search`, `SearchExact`,
`EstimateRange`, `NewBatch`, `Insert`, `Delete` and `Move` signatures remain.
Standalone `NewStore` still permits namespace zero for old callers; manager
operations reject zero, and zero must not be registered/shared as a schema
namespace. `OpenStore` only accepts a directory with exactly one namespace;
otherwise it returns `ErrAmbiguousNamespace`. Named opens validate identity.

Deprecated `Spec.Offset`/`Metadata.Offset` persist as compatibility metadata,
not timestamp offsets. `Config.BlockSize` is a record-count alias used when
`MaxShardBytes` is absent. Delta/queue settings remain validated but are unused.

Both `DurabilityOS` and `DurabilitySync` now receive the stronger synchronous
commit guarantee. There is no deferred-durability mode, pending overlay, WAL,
compaction queue or background worker. `Checkpoint`, `Sync` and `Vacuum` are
compatibility barriers, not generation-advancing rewrites; memory-backed
checkpoint calls retain `ErrNotPersistent`. `Compact` is a no-op barrier;
`WaitIdle` checks the context and pending-compaction count is zero.

## Single-file binary format (version 5)

Four lower-case **hexadecimal** namespace digits name the data file, covering
all uint16 namespace values without decimal-width ambiguity: `0001.timeidx`,
`00ff.timeidx`, `ffff.timeidx`. Each namespace also has an empty advisory
`<filename>.lock`, containing no index state. There are no shard files,
generation files, manifests, journals or WALs.

Creation initializes the same data inode under `.0001.timeidx.creating`, then
renames it to the final name and syncs the directory. No second data inode is
copied. Pre-rename process termination leaves no openable final namespace;
the next explicit creation can discard that reserved, unacknowledged temporary
file under exclusive ownership. A final name is never truncated/reinitialized,
even when corrupted. A failed directory sync after rename leaves a possibly
created namespace to inspect/open rather than silently replace.

All persisted fields are little-endian. Typed mmap columns require a
little-endian host and eight-byte alignment. Superblocks use the existing
512-byte CRC32C envelope; slot A occupies bytes 0–511 and B occupies 512–1023.
The arena uses shared `pagealloc` 512-byte runs. `Ref` and extent sizes are
uint32; arenas are capped at 2 TiB and per-extent arithmetic is checked before
conversion/rounding. Mapping lengths must also fit the platform's Go `int`;
shared mapping helpers reject overflow before truncation/mapping. Persistence
requires a working shared read-write mmap backend. Darwin/arm64 was tested;
the existing Windows read-write backend remains unsupported.

### Superblock envelope

The shared header stores family magic `TIM4`, version 5, generation, payload length,
payload CRC32C and header CRC32C. The 64-byte TimeIdx payload is:

| Offset | Width | Field |
|---:|---:|---|
| 0 | 2 | Namespace |
| 2 | 2 | Encoding ID: 1 = exact 1900-epoch nanoseconds |
| 4 | 4 | Spec flags |
| 8 | 8 | Directory byte offset, zero for empty |
| 16 | 4 | Shard count |
| 20 | 4 | Directory CRC32C |
| 24 | 8 | Logical entry count |
| 32 | 8 | Arena byte length at this generation |
| 40 | 8 | Deprecated offset metadata, unused by encoding |
| 48 | 8 | Maximum permitted shard payload for this generation |
| 56 | 8 | SourceGeneration: application-supplied DB completeness watermark |

The shard bound preserves the largest retained shard if a smaller runtime
configuration is selected on reopen. New replacements use the new limit;
unchanged older shards are not rewritten solely to change sizing policy.

### Directory and SoA extents

The directory is an immutable sorted array of 56-byte records:

| Offset | Width | Field |
|---:|---:|---|
| 0 | 8 | FirstTime |
| 8 | 8 | FirstValueID |
| 16 | 8 | Shard byte offset |
| 24 | 4 | Entry count |
| 28 | 4 | Time-column CRC32C |
| 32 | 4 | ID-column CRC32C |
| 36 | 4 | Reserved, encoded zero |
| 40 | 8 | MinID |
| 48 | 8 | MaxID |

A shard is `Times[count] uint64`, immediately followed by
`ValueIDs[count] uint64`, for 16 payload bytes/entry. Parallel columns remain
aligned and strictly sorted by `(TimeKey, ValueID)`. A timestamp can span any
number of shards; full pair boundaries remain strictly increasing.

No allocator bitmap/free-space record is persisted. Recovery reconstructs
free runs from the union of recoverable generation extents, reserving both
superblocks. Adjacent free runs coalesce in the shared allocator bitmap.

## Algorithms

### Search and estimates

1. Acquire the namespace reader token **before** loading the snapshot.
2. Check closed; load one immutable snapshot containing all shards.
3. Binary-search the first shard whose last time is at least `start`.
4. Visit shards until the first time is at least `end`.
5. Binary-search `[lower_bound(start), lower_bound(end))` in each time column.
6. Bulk-append the matching ID-column slice to caller-owned `dst`.
7. Release the token, including callback panic paths.

Repeated timestamps spanning shards are visited in full. Results are globally
chronological, tie-broken by ID, with no duplicate IDs. `EstimateRange` performs
the same searches and adds lengths, giving an exact snapshot-local count.
Separate estimate and search calls can observe different committed snapshots.

Ranges are half-open; `start>=end` is empty and `end=MaxUint64` excludes that
key. No `end+1` computation can overflow. `SearchExact` retains the optional
canonical callback for compatibility, not quantization correction. Its lookup
uses the same encoding and is not transactionally pinned with external metadata;
callbacks must not synchronously write/close this pinned index.

### Batch writes, deletes, moves and selective compaction

```text
lock namespace writer
validate exclusive reusable batch ownership
copy operations into stack scratch for <=64, otherwise bounded slice
stable-sort by ValueID; retain last operation per ID
remove idempotent upserts by composite-pair binary lookup
sort additions by (time, ID); route by full first-pair boundaries
identify shards receiving additions or containing touched IDs
merge retained rows and additions into new SoA columns
split by balanced entry position, including all-equal timestamps
coalesce adjacent NEW undersized outputs when combined payload fits
commit COW replacements and directory; publish once
unlock namespace writer
```

`Insert` upserts by ID. `Delete` removes by ID even with a stale asserted time.
`Move` removes any old location and inserts the new one atomically; `from` is
advisory. Duplicate initial IDs are rejected rather than silently choosing one.
Absent deletes and identical upserts need no new committed generation.

MinID/MaxID prune unaffected shard scans. A bounded stack bitset accelerates
compact touched-ID spans; otherwise membership uses sorted operations. No
runtime reverse map is required, but sparse/unclustered existing-ID updates
can still scan many base ID columns. Unchanged persistent shards keep their
extents and descriptors. Adjacent unchanged shards are deliberately not merged
just to improve occupancy. Persistent merge columns use reusable writer-owned
buffers and are copied into mmap before publication; heap snapshots instead
own fresh immutable columns. One concrete merge/search implementation serves
both storage modes.

## Persistence, recovery and lifetime invariants

The truly shared mechanisms extracted into `internal/storage/cow.go` are
superblock commit outcomes/barriers, extent union validation and allocator
reconstruction. GeoIdx now calls the same functions; payload schema, routing,
generation decoding and index-specific orchestration remain concrete. There
is no generic database engine, cross-family import or new dependency.

### Changed-batch commit sequence

```text
allocate fresh shard and directory extents from current protected allocator
write changed SoA shards; retain unchanged offsets/checksums
write and checksum the new directory
sync mmap and file                         // payload-before-root barrier
compute new+previous generation protected live set
write inactive checksummed superblock
sync mmap and file                         // root durability barrier
atomically publish the complete new snapshot
wait for readers that could hold the previous snapshot
install reconstructed allocator for the two recoverable generations
unmap retired mappings
return success
```

The OLD allocator continues reserving obsolete extents until the reader grace
period finishes. It must not be replaced earlier. Commit publication is
serialized per namespace; other namespaces have no persistence barrier here.
Long readers can delay writer completion/reuse, not search or publication.

| Outcome/window | Required behavior |
|---|---|
| Before root write | Aborted: rollback new extents; old snapshot remains authoritative |
| Partial root write or root sync error | Uncertain: retain candidate extents, keep old in-memory view, latch writes until close/reopen |
| After root sync, before publication | Durable: recovery selects complete new generation; publication has no fallible steps |
| During reader drain/reclamation | Durable: old mappings/extents remain protected while pinned |
| Data/directory corruption in newest root | Reject that generation; fall back to independently valid prior root |
| Namespace/encoding/version mismatch | Explicit error; never open the wrong namespace or reinterpret another format |
| No valid generation | `ErrCorrupt`, never implicit empty initialization |

Uncertain commits are deliberately latched rather than allowing a complex
in-process retry path: reopening resolves which fully valid generation is
recoverable and rebuilds its allocator. A failed call may recover either
complete batch state, never a prefix.

Open checks both envelopes, identity, encoding, generation, directory CRC and
placement, arena bounds, count arithmetic, shard CRCs, pair order, ID bounds,
global ID uniqueness and overlapping extents. Dense ID domains use a bounded
validation bitset; sparse domains use a sorted scratch ID column. Candidates
are sorted by generation, with the older valid generation's extents retained
for fallback. Shared offsets require identical lengths. No committed free
list from an earlier process is trusted.

### Growth and close

Arena growth extends the SAME file, adds a new mapping and retains the old
mapping. Unchanged shards rebind to slices of the new mapping without copying
their payload. Reader-visible old slices never remap in place. Prior mappings
are only unmapped after publication and reader drain; mapping/growth failures
leave the prior committed view usable. No shrinking/truncation of live extents
is performed. `MaxFileBytes` selects initial sparse capacity (default 4 MiB),
not an immutable capacity limit.

`Close` marks the index closed under its writer mutex, drains prior readers,
closes current/retired mappings and releases ownership. Readers acquiring
after the close flag do not touch mapped data.

These guarantees rely on the OS/filesystem honoring sync ordering. Tests prove
process-crash/error-injection behavior, not physical power-cut safety for every
device/filesystem. As in GeoIdx, the two 512-byte envelopes share an OS page;
CRC fallback is not a guarantee against arbitrary corruption of both slots.

## Migration and interoperability

Old manifests/generation arrays/WALs are explicitly rejected as unsupported.
Do not delete the old store until migration has been verified. Migration is:

1. Export the old logical live ID set through the old binary/read API, including
   its WAL recovery, plus canonical exact timestamps from metadata.
2. Convert canonical UTC instants to the new global encoding. Old `Offset` plus
   a quantized marker cannot reconstruct the discarded exact low bits.
3. Register the same namespace identities/names in the application schema.
4. Explicitly create namespaces in a DIFFERENT new directory; import unique
   IDs through batches, applying any concurrent updates at a coordinated cutover.
5. Verify counts and ordered ranges against canonical data, reopen/verify the
   new files, then switch readers. Keep the original store for rollback.

There is no built-in automatic migration command. Query bounds, exact metadata
callbacks and time-ranking targets must all be converted consistently at the
application boundary. Bitmap intersection/planning remains in `search`; the
index does not import `search` or add cross-index transactions.

## Verification and measured tradeoffs

Tests cover explicit/concurrent creation/open/close, identity mismatches,
failure/isolation, same IDs/times across namespaces, independent growth and
generations, equal-time splits, selective writes, old-reader growth/close,
last-write-wins, extreme IDs/keys, chronological ordering, zero-allocation
searches, CRC/bounds/overlap failures, retained generations and extent reuse.
Reference differential tests exercise batches 1/8/32/64 in memory and across
persistent reopen. Subprocess tests terminate at payload, root, publication,
growth, reclamation and creation-name barriers. Parser fuzzing is included.

Validation commands:

```sh
go build ./...
go test ./...
go test -race ./...
go vet ./index/timeidx/... ./search/... ./internal/storage/... ./index/geoidx/...
go test ./index/timeidx -run '^$' -fuzz '^FuzzNamespaceGeneration$' -fuzztime=10s
sigmap validate
```

See `index/timeidx/BENCHMARKS.md` for before/after measurements and reproducible
commands. Gates are correctness/race/recovery success, zero-allocation searches
with caller capacity, one final data file per namespace, bounded changed-shard
payloads, independent namespace progress and explicit explanations for measured
regressions. No SIMD, compression or interpolation search was justified.

Known scaling risks: directory rewriting is O(shard count), allocator rebuild
is proportional to arena/live extents, sparse ID updates can scan many shards,
and random imports dirty more extents than clustered writes. Smaller shards
reduce random rewrite amplification but increase directory size and object
counts. Reusable writer buffers retain peak temporary capacity until close.
Synchronous COW costs two durability barriers versus the old WAL's one; it does
not preserve the old write latency, and the measurements state that explicitly.
Large-scale/tail-latency acceptance beyond the measured workloads needs the
application's target hardware and workload, not a universal performance claim.
