# V1 contracts and release policy

This file freezes the intended V1 public surface and operational contracts. It
is a summary; package documentation and tests are authoritative for exact
signatures and error wrapping.

## Stable V1 API

| Package | Stable surface |
|---|---|
| `database` | `DB`, `Open`, `Mutation`, `Options`, `CanonicalProjectionMode`, `RecoveryPolicy`, `RecoveryStatus`, stable error sentinels, `Commit`, `WaitIndexed`, `Sync`, `Checkpoint`, `Get`, `DatabaseGeneration`, `IndexedGeneration`, `Close` |
| `schema` | `Definitions`, `NamespaceDef`, `EntityKind`, `Namespace`, identifier aliases, `Schema` open/ID APIs, durable format errors |
| `search` | `Engine`, `Plan`, `Executor`, clause constructors, `Workspace`, `Page`, result types, metrics, stable errors |
| `store` | Advanced stable KV `Store`, `Key`, `Batch`, `Spec`/`Open`, sync durability, owned `Get`, scan ownership |
| `index` | Advanced stable unified store, typed accessors, `Entry`, `Change`, `Batch`, matcher, definition helpers, health/recovery diagnostics |
| `index/bitmapidx` | Advanced stable namespaces and BSI APIs |
| `index/timeidx` | Advanced stable timestamp APIs |
| `index/geoidx` | Advanced stable geographic APIs |
| `index/radixhash` | Advanced stable exact-hash APIs |
| `index/simhash` | Advanced stable perceptual APIs, including sparse external IDs |
| `roar` | Advanced stable immutable bitmap representation/operators |
| `search/ranking` | Optional advanced ranking/fusion helpers |

## Experimental surface

`index/vectoridx` and vector portions of `index` (`KindVector`, `VectorDef`,
`VectorValue`, vector accessors, and rank/filter integration) are functional but
**experimental**. V1 does not promise vector API, performance, or file-format
stability. No Phase 4 batching or graph redesign is required for V1.

## Primary lifecycle

Current V1 bootstrap:

1. build a `schema.Definitions`;
2. create the derived index once with `index.Create` or
   `index.CreateWithDefinitions`;
3. call `database.Open(dataDir, indexDir, decoder, options)`;
4. use `database.DB` for subsequent lifecycle operations.

`database.Open` on an existing V1 database consumes `schema.dat` and does not
need definitions or index creation repeated. `DB.Index()` is an advanced
read accessor; do not write to or close it while the coordinator runs.

There is no `database.DB.Search` in V1. Search remains explicit:

```go
engine, err := search.New(db.Index())
plan, err := engine.Compile(entity, clauses...)
ids, err := plan.AppendIDs(ctx, dst, workspace)
```

This preserves caller-owned plans, workspaces, destinations, metrics, and
pagination. A future database-level facade must be benchmarked and must not
replace this path; absent that proof, it is not part of V1.

## Ownership

- `Mutation.Record` and `Mutation.Projection` are caller-owned and reusable
  after `Commit` returns.
- `DB.Get` returns caller-owned bytes.
- `Commit` copies what it retains; decoded projections may borrow decoder
  scratch only until the current coordinator batch ends.
- `index.Batch`, `store.Batch`, `search.Workspace`, `SearchMetrics`, ranking
  scratch, and matcher workspaces are exclusive to one use unless their docs say
  otherwise.
- `Plan` is reusable and safe to share across readers once compiled.
- `Engine` plans pin one snapshot per execution; direct `Compile` plans read live
  views and require open indexes.
- `DB.Close` may wait for active plans to release pinned snapshots. Drain
  long-running reads before close.

## Concurrency

- Canonical commits are serialized through `DB.mu`; the index has one serialized
  apply writer.
- The database coordinator is one bounded background publisher. There are no
  independently publishing per-family writers.
- `search.Engine` queries are lock-free with respect to writer publication:
  they capture a coherent snapshot under an even publication sequence, retry if
  a writer overlapped, and pin the captured views for execution.
- Engine reclamation can wait for a pinned old mapping. This is storage
  backpressure, not a query-wide writer lock.
- `Close` rejects new operations, drains the coordinator, closes stores, and
  releases locks. Calls admitted before close follow their documented error
  behavior.
- Vector writes and searches retain their experimental engine-level concurrency
  contract.

## Consistency

- `Commit` means canonical record + journal + generation are durably committed.
- Indexing is asynchronous and eventually consistent. Call
  `WaitIndexed(ctx, generation)` before requiring search freshness.
- `Sync(ctx)` waits for every generation committed before the call.
- `Checkpoint()` checkpoints derived indexes; canonical Pebble data is already
  durable. It does not replace `WaitIndexed`.
- `Engine` plan execution sees one coherent published generation across its
  participating bitmap/time/geo/ACL stages. Separate executions and pages may see
  newer generations.
- Direct `search.Compile` plans read live per-engine views.
- Delete/reinsert preserves liveness and removes stale associations using the
  tombstone's prior projection.

## Error families

Use `errors.Is` with the exported sentinels. Important database errors:

| Error | Meaning |
|---|---|
| `ErrInvalidMutation` | Invalid entity/ID/projection/option |
| `ErrClosed` | Database closed |
| `ErrJournalFull` | `MaxPending` reached |
| `ErrSequenceOverflow` | Generation exhausted |
| `ErrRecoveryRequired` | Startup reconciliation/index recovery required |
| `ErrCorruptJournal` | Corrupt journal sequence/payload/canonical state |
| `ErrUnsupportedJournalVersion` | Unsupported journal format |
| `ErrUnsupportedCanonicalVersion` | Unsupported canonical envelope |
| `ErrCanonicalModeMismatch` | Reopen requested a different projection mode |
| `ErrCorruptDatabaseConfig` | Corrupt persisted database config |
| `ErrProjectionUnavailable` | External projection callback required but absent |

`store.ErrNotFound` means a missing or tombstoned canonical record. Storage
errors are wrapped, not converted to not-found. Index packages expose their own
stable sentinels (`ErrClosed`, `ErrInvalidSchema`, `ErrNamespaceUnavailable`,
`ErrRecoveryRequired`, `ErrUnsupportedVersion`, etc.).

## Limits and options

| Limit | V1 behavior |
|---|---|
| `Options.BatchSize` | Default 64; values above 64 rejected. This is one index-apply batch, not the total mutations in a `Commit`. |
| `Options.MaxPending` | Bounds unacknowledged journal lag; zero is unbounded. |
| `Options.MaxProjectionBytes` | Default 1 MiB for copied before+after projection bytes in one coordinator batch. |
| `Options.OversizedRetentionBytes` | Default 4 MiB for the one retained oversized projection buffer. |
| `Options.CanonicalProjection` | Persisted inline/external mode; conflicts fail on reopen. |
| `Options.Definitions` | Startup-only additive schema target; existing identities immutable. |
| `ValueID` | Zero reserved. Explicit IDs are last-writer-wins and do not reserve against already-issued concurrent IDs. |
| Vector IDs | Must fit `uint32`; vector feature remains experimental. |
| Journal | v2 only; unknown versions fail closed. |
| Tenant/runtime maps | Avoid hot-path maps/reflection; use typed slices and reusable scratch. |

## Version and persistence policy

- `schema.dat` is authoritative. V1 container v3 is the baseline; v2 is read as
  ACTIVE and upgraded on publication. Container/allocator v1 is rejected.
- Canonical inline v1, external v2, and journal v2 are V1 contracts. Derived
  index files are versioned, rebuildable caches, not permanent binary promises.
- Pre-V1 experimental databases are not automatically opened. Unsupported or
  corrupt formats fail closed and are never silently reinterpreted.
- Canonical/schema semantic changes require a version bump and explicit migration
  or rejection.
- Journal replay must remain correct for committed V1 databases.
- API compatibility is promised only for the documented stable surface; vector
  components are experimental.

See [`database/FORMAT.md`](../database/FORMAT.md) and
[`schema/FORMAT.md`](../schema/FORMAT.md).

## Recovery

`RecoveryManual` leaves canonical data/journal intact and returns
`ErrRecoveryRequired` for an unhealthy index. `RecoveryAutomatic` rebuilds and
verifies the complete unified index before publishing the DB. The recovery unit
is the whole unified index; bitmap and time families share physical stores, so
selective namespace replacement is deferred and not claimed.

## Benchmarks

Use `internal/bench` and package benchmarks; do not add a parallel harness.
Measure representative inserts, updates, deletes, compound bitmap/time/geo
queries, sparse SimHash lookups, concurrent ingest/search, reopen, allocations,
and retained memory. Keep 10M runs opt-in via `IMAGEIDX_BENCH_LARGE`. Report
logical byte counters separately from actual filesystem/device I/O.

V1 has no throughput-improvement quota. Correctness, durability, stable documented
APIs, and no serious unexplained regression are the release gates. Any material
regression must be explained and measured on matched hardware/compiler/config.

## Release and platform policy

- V1 must reopen V1 databases and fail closed on unsupported/corrupt formats.
- The release tag must point to the exact validated commit.
- Supported CI exercises Linux `amd64` and `arm64`, including the internal
  SIMD-kernel equivalence suites.
- No selective recovery, per-family writers, adaptive batching, vector
  stabilization, new SIMD architecture, allocator rewrite, or ranking redesign
  is required for V1.

## Post-V1

Deferred unless a demonstrated correctness/data-loss defect forces work:

- per-family parallel writers and composite root/epoch reclamation;
- selective family/namespace recovery;
- adaptive batching and broad write-amplification tuning;
- vector batching, graph redesign, or vector API stabilization;
- new SIMD/SoA architecture or storage allocator changes;
- ranking redesign or new cardinality systems.

Historical design proposals and phase reports are available in git history.
They are not current contracts and should not be reintroduced as parallel
implementation guidance.
