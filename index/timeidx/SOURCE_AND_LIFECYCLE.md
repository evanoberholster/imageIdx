# Source completeness and lifecycle (format 5)

## Source generation is a completeness watermark

`SourceGeneration()` returns a caller-supplied database generation from the
published heap snapshot. `ApplyWithGeneration(batch, generation)` commits it
with that batch's shard directory in the same CRC32C superblock. The 64-byte
payload's bytes 56–63 now hold this uint64; its format version is **5**. The
family magic remains TIM4. Version 4 is explicitly unsupported: rebuild from
canonical data into a separate location, never overwrite it implicitly.

The application asserts that **every applicable DB change through the supplied
generation is incorporated**. TimeIdx cannot infer gaps, filter applicability,
or determine DB transaction ordering. Applications must coordinate concurrent
DB batches; a later watermark batch must include any earlier applicable changes
not yet applied. Do not supply merely the maximum event generation observed.

The writer mutex covers closed/failed checks, generation validation, mutation
normalization, COW persistence and snapshot publication. Decreasing source
generations are rejected. Equal generations allow retries, which should be
idempotent at the application boundary. Empty or identical batches that advance
the watermark still commit. Legacy `Apply` preserves the watermark; it never
invents or advances a DB generation. Advancing the completeness claim without
the corresponding DB data is an application error, not something CRC can detect.

- Aborted commit: neither data nor watermark is published.
- Uncertain root write/sync: no success is reported; candidate extents stay
  protected and further writes are latched until reopen.
- Durable root: data and watermark publish together. Reopen obtains both from
  the same selected root, including older-generation fallback.
- `CheckSourceGeneration(required)` returns `ErrIndexStale` if behind and
  `ErrClosed` after shutdown. Staleness is not `ErrCorrupt` or an I/O error.
- `NamespaceInit.RequiredSourceGeneration` checks completeness before exposing
  a namespace. A stale member remains configured but unavailable; unrelated
  healthy members remain accessible.

Comparison only verifies the supplied required generation. The application
must obtain a trustworthy DB watermark and keep checking/replaying as the DB
advances. This does not create a database/index cross-resource transaction, nor
enable relaxed durability. Zero is the initial, unrecorded watermark.

## Application-controlled rebuild

1. During startup identify missing, stale, corrupt, unsupported or I/O failures
   from initialization results. `ErrRebuildRequired` is an integrity failure;
   `ErrIndexStale` permits replay instead of a rebuild. Do not rebuild blindly
   on permission, read or disk errors.
2. Obtain a consistent canonical DB snapshot/checkpoint and coordinate later
   writes by pausing them or replaying a contiguous change stream.
3. Create/import a separate namespace file from canonical exact timestamps.
4. Commit its represented database generation using `ApplyWithGeneration`;
   an empty batch can mark completeness after a bulk initial import.
5. Close and verify the staged namespace and its required source generation.
6. With final/staging owners closed, use `PublishRebuild` for same-filesystem
   rename and directory sync. The original is preserved on pre-rename failure;
   post-rename sync errors are uncertain and require inspection/reopen.
7. Reopen, check against the DB checkpoint, then seal/publish the registry.

Runtime registry membership never changes. Replacing a runtime namespace requires
controlled shutdown/reinitialization; no active owner may reference the replaced
file. Indexing has no database-specific replay code or automatic rebuild.

Rebuild coverage includes per-call failures at final/staging lock acquisition,
verification, staged sync, rename and both directory syncs, plus staged creation
failures and foreign-namespace rejection. Before rename, failures leave both
verified files unchanged and release ownership locks. After rename, any error
is `ErrRecoveryRequired` joined with its original I/O cause, never success or
corruption. Subprocess exits immediately before/after rename validate the old
or replacement file on reopen, including its source generation. Process exit
is not a simulated power failure; unsynced rename durability remains explicitly
uncertain even when the replacement is visible after process termination.

## Exported lifecycle audit

| Methods | Memory accessed | Protection / shutdown behavior |
|---|---|---|
| `Search`, `SearchExact`, `EstimateRange` | mmap time and ID arrays | Acquire reader token → closed check → atomic snapshot load → data → deferred release. New calls after close return no matches; pinned calls finish before unmap. Callbacks must not synchronously write/close their pinned index. |
| `Metadata`, `Generation`, `Len`, `SourceGeneration` | Heap snapshot counters and slice header only | Atomic snapshot load; no mapped elements are dereferenced. The snapshot is retained on the Go heap after close; no reader token is necessary. |
| `Namespace`, `PendingCompactions`, `WaitIdle` on Index | Immutable heap identity / constants / context | No mmap access. `WaitIdle` reports context cancellation; there is no background compaction to drain. |
| `CheckSourceGeneration` | Closed atomics, heap watermark | No mmap access; returns closed or stale sentinel. Concurrent publication may yield either complete snapshot. |
| `Apply`, `ApplyWithGeneration` | Private scratch, allocator and mmap writes | Per-index writer mutex; reject closed/failed state before mapped access. Admitted writes may finish while Store.Close waits on the same mutex. Watermark and data publish atomically. |
| `Checkpoint`, `Sync`, `Vacuum`, `Compact` | Heap state under writer mutex | Compatibility barriers only. Reject closed state; checkpoint/sync/vacuum require persistence. Vacuum does **not** shrink a file. |
| `CommitStats` | Heap statistics | Writer mutex; safe after close, returns last statistics. |
| `OnDiskBytes` | Current mapping's slice header | Writer mutex excludes unmapping; returns zero once mapping is closed. |
| `Index.Close` | Mappings, locks, writer scratch | Writer mutex; sets closed before tracker drain; closes current/retired mappings and releases file lock. Repeated closes serialize and are idempotent. |
| `Store.Namespace`, `NamespaceError` | Immutable sorted registry and closed atomics | Atomic registry load, no registry mutex; handles remain safe when shutdown races resolution. No runtime membership edits. |
| Startup creation/open/removal, `SealStartup` | Private registry builder / index constructors | Lifecycle mutex; reject membership changes after sealing. `OpenNamespace` is a convenience resolver, not the lock-free hot lookup API. |
| `Store.Close` | Immutable entries, per-index close | Lifecycle mutex and atomic owner-closed flag disable all owned indexes before any drain; entries are never mutated. Repeated closes wait for the first shutdown to finish. |
| Constructors, standalone opens | New independent file mappings | Namespace file ownership locks. Failed initialization never exposes an Index handle. |
| `Verify`, `VerifyNamespace` | Independent read-only mapping | Own mapping lifetime, not another Index's mapping. Call on closed/quiescent files for reliable validation. |
| `PublishRebuild` | Independently verified staged file | Offline only; locks exclude final and staging owners; no runtime registry replacement. |
| Time conversion, filename/spec helpers | Values only | No lifecycle/mmap access. |
| `Batch` methods | Caller-owned heap operations | Caller must not modify/read batch contents concurrently with submission. `inFlight` rejects reuse while applying; it is not a general shared mutable-batch lock. |

The shared two-slot `ReaderTracker` rechecks its epoch after incrementing the
reader count. Reclaimers are serialized. Close sets its flag before changing
the tracker epoch, so a reader either observes closed before mapped access or
is counted in the slot that close drains. There is no unprotected closed-check
then-acquire gap and no per-query allocation. OwnerClosed is assigned before
startup exposes a handle and remains immutable thereafter.

Deterministic tests hold an exact-lookup callback across shutdown and hold a
growing writer across Store.Close. Multi-namespace tests exercise retained
handles, all metadata/search methods and repeated concurrent closes under race
detection. Existing pinned-reader tests cover publication, reclamation and
mapping growth. Reclamation stays synchronous and bounded; no retirement worker
or unlimited pending mapping list was added.
