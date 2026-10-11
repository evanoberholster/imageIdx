# Final alignment work — in progress

Baseline: `bd0752f5c170a5b5aaf97efa791c54f5bfbdb59e`. PR #16 must remain
unmerged until the expanded performance/recovery review is complete.

## Current changes

- `InitializeStore` validates the complete namespace configuration before
  initializing files and publishing an immutable sorted registry. Failures are
  returned per namespace; a failed member stays configured but unavailable.
- The compatibility startup builder requires `SealStartup` before `Namespace`
  lookup. Registration/removal after publication returns `ErrNamespaceFrozen`.
  `Namespace` never acquires a mutex or allocates. `OpenNamespace` remains a
  lifecycle convenience, not the hot lookup API.
- Shutdown atomically disables operations through every owned index before
  waiting for per-index readers. The published registry is never mutated.
- Corrupt files/identity mismatches return `ErrRebuildRequired` wrapping the
  integrity cause. I/O/configuration/unsupported-format errors are not rebuild
  signals. Normal older-generation recovery is unchanged.
- `PublishRebuild` is an offline startup-only contract: validate/sync a separate
  completed file, exclude active final/staging owners using existing locks,
  rename on the same filesystem, then synchronize both directories. Failures
  before rename preserve the original. Sync failures after rename are uncertain
  and require inspection/reopen. The application owns DB snapshot/replay.
- Both indexes now share the identical 56-byte physical directory codec;
  domain validation and checksums remain local. No disk-format change.
- Large mutation batches reuse writer-private scratch. Small batches still use
  the fixed 64-operation stack buffer. Fully covered search shards skip bounds
  searches. These changes still need the final performance gate.
- No maps remain anywhere in TimeIdx, including differential reference tests.

## Shared-code decisions

| Component | GeoIdx location | TimeIdx location | Decision |
|---|---|---|---|
| mmap lifecycle | `persist.go`, shard regions | `storage.go` / `index.go` | Keep local orchestration: GeoIdx retains per-shard region ownership; TimeIdx rebinds unchanged slices on growth. A common retire owner would hide different lifetime rules. Both already share `MappedFile`. |
| COW commit | `persist.go` | `storage.go` | Already share `CommitSuperblock` and all three outcomes; payload/publication stays local because deferred vs synchronous semantics differ. |
| Extent union | shared storage | shared storage | Already share sorted owned extent unions; ordered-input merge/scratch optimization still to evaluate. |
| Allocator recovery | shared storage | shared storage | Already share allocator reconstruction/pagealloc; repeated recovery reconstruction still to profile. |
| Shard directory encoding | `persist.go` | `storage.go` | Extracted exact 56-byte codec into `internal/storage/shard_descriptor.go`; bounds/domain checks stay local. |
| SoA allocation | typed S2 columns | `shard.go` | Keep specialized contiguous TimeIdx helper; sharing distinct S2 key backing storage would need unsafe conversion or duplicate allocations. Both share typed mmap `View`. |
| Reader tracking | Index tracker | Index readers | Already share `ReaderTracker`; reclamation orchestration remains local. |
| Splitting/merging | `shard.go` | `shard.go` | Keep specialized; GeoIdx min/target/max and layered compaction differ from TimeIdx's single bound. |
| Search | `search.go` | `search.go` | Keep separate geographic vs chronological semantics. |
| Namespace registry | N/A | `store.go` | Sorted immutable published slice, startup builder only. |
| Corruption classification | `persist.go` | `storage.go` | Keep domain errors local; common parsing primitives remain shared. Explicit TimeIdx rebuild signaling must not classify I/O as corruption. |

## Decisions retained

Synchronous durability remains the only mode: there is no application-supplied
DB synchronization token, and checksums cannot detect a valid but stale index.
No relaxed mode is safe to add without a synchronization contract. Reclamation
still waits for pinned readers; no unbounded retirement list was introduced.
The shard default remains 4 KiB pending the expanded workload matrix.

## Outstanding acceptance work

- Full random-ID/clustered/sparse/repeated-time shard matrix with reopen,
  fragmentation, directory/growth and rewrite-volume counters.
- Bounded group-commit prototype vs synchronous and explicit larger batches,
  including throughput and p95/p99 latency; no queue has been added yet.
- Deferred-reclamation stress/evidence and bounded-memory evaluation.
- Monotonic mutation routing evaluation and final reopened-index profiling.
- Rebuild crash/directory-sync failure injection beyond the current successful,
  corrupt-staging and active-owner exclusion tests.
- Final performance comparison/review and updated PR description.

The initial baseline matrix is saved as `timeidx-final-before-bd0752f.txt` in
the session's approved temporary directory. Initial after measurements are
`timeidx-final-after.txt`; neither constitutes the complete acceptance matrix.
