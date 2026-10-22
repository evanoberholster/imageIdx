# ImageIdx — Phase 2 Follow-up (PR #25)

**Codex implementation instructions**
**Repository:** `evanoberholster/imageIdx`
**PR:** https://github.com/evanoberholster/imageIdx/pull/25
**Review baseline:** `b7bb7908802af4c019d30996533c1ee85c688909` (recheck the current HEAD before coding).

## Mission and constraints

Complete and harden Phase 2 before merge. This is a **pre-V1** index: compatibility with older experimental database/index files is **not required**. Prefer one clear, durable format and fail closed on unsupported formats. Nonetheless, fresh databases must survive restart, interrupted writes, partial publication, and journal replay correctly.

Priorities, in order: **correctness → durable recovery → bounded write amplification and memory → search performance → code simplicity**. Preserve the centralized authority of `schema.dat` for schema/namespace configuration, Pebble for canonical data, a single serialized index publisher, and safe concurrent index readers. Avoid maps in hot paths, reflection, whole-dataset COW, excessive abstraction, or unnecessary allocation. Retain typed slices, existing engine-specific optimized kernels, and workspace ownership rules. Do not claim that application-level `Stats()` counters are measured physical device writes.

First inspect the latest PR diff and `AGENTS.md`, plus applicable package design/benchmark instructions. Verify each issue against the latest code; do not mechanically implement a recommendation already resolved. Make focused commits, add tests alongside fixes, and provide before/after benchmarks.

## P0 — Required before merge

### 1. Correct **all** sparse SimHash search variants

**Inspect:** `index/simhash/search.go`, `sparse.go`, associated search tests.

`SearchPHash` and `SearchPDQ` merge sparse records, but at the review baseline `SearchPDQBulk` scans only dense pages and returns early when the dense count is zero. Update `SearchPDQBulk` to cover sparse-only and dense+sparse stores, while retaining its optimized dense kernel and existing concurrency restrictions. Audit *all* other public lookup/search APIs, including `Get` and any bulk variants, for consistent sparse overrides and tombstones.

Requirements:

- Return identical `(ImageID, Distance)` matches and defined ordering for scalar and bulk PDQ searches under the same quiescent state.
- Avoid duplicate results when a sparse record overrides a dense record.
- Preserve caller-provided result prefixes (`dst`) and append semantics.
- Preserve PDQ distance clamp and `uint8` result semantics.
- Test sparse-only IDs (including `>2^32`), mixed dense/sparse, duplicate updates, deleted IDs, exact and near matches, and zero matching results.
- Test under race detection for APIs advertised as concurrent-safe; do **not** silently make bulk SIMD reads concurrent-safe by assumption.

### 2. Replace per-update **full sparse-slice COW** with bounded publication

**Inspect:** `index/simhash/sparse.go`, `store.go`, `search.go`, shared reader tracking and persistence utilities.

At the reviewed HEAD, `appendSparseLocked` executes `append([]sparseRecord(nil), st.sparseRecords...)`, copying all live sparse records per update batch, then republishes an entire sorted snapshot. This makes write cost scale with total sparse population and contradicts low-allocation/low-write-amplification goals.

Implement a **small bounded mutable writer buffer + immutable sorted segments** (or a simpler proven equivalent) rather than copying the complete dataset on every commit. Requirements:

- One serialized writer; readers use atomically published immutable views or otherwise proven safe pinned snapshots.
- Changes visible to readers only after the appropriate publication boundary. Readers must not observe partially updated segments.
- Explicit newest-wins replacement and deletion semantics for duplicate external IDs across segments and dense storage.
- Bound active segment count and stale-version accumulation using measurable compaction triggers; prefer streaming compaction and reusable typed buffers.
- Bound extra memory independently of total history. Avoid per-record maps on read hot paths; an optional temporary map during cold recovery must be benchmarked against a sorted-slice approach.
- Define segment layout, checksums, generation/manifest publication, directory/file sync points, crash recovery, and safe retirement of old reader-pinned files.
- No whole-dataset copying on ordinary updates. Allow larger work only at explicit compaction/rebuild boundaries.
- Avoid a universal LSM framework; keep this specialized to SimHash.

**Design checkpoint:** before rewriting storage, document the chosen data layout, reader lifetime, publication ordering, and a benchmark comparison against the current sorted-overlay implementation. Choose the simpler design if it meets targets. Since the index is pre-V1, breaking the current experimental `sparse` format is acceptable; provide clear format rejection or rebuild instructions, not ambiguous partial compatibility.

### 3. Make sparse persistence replay-safe and recoverable

**Inspect:** SimHash sparse write/Sync/Open, index generation publication, database coordinator acknowledgments.

A durable sparse write can precede database generation acknowledgment. Replaying the same journal interval must produce identical searchable state without unbounded duplicate records. Treat partial tails and bad checksums distinctly; do not accept a fully written corrupt frame as harmless truncation.

Add process-crash/failpoint tests at:

1. Before sparse data write.
2. During a partially written record/segment.
3. After write, before sync.
4. After durable sparse sync, before index manifest/generation publication.
5. After index publication, before Pebble acknowledgment.
6. After acknowledgment and during journal cleanup.
7. Every new compaction/publish/retire boundary.

Assert correct ImageID/hash state, tombstones, no duplicate results, monotonic generation, repeat-replay idempotence, and safe reopen. Do not reset writer-owned workspace memory before synchronous index apply has finished. If introducing deferred work, explicitly redesign ownership/pinning first; otherwise retain the synchronous path.

### 4. Persist and validate canonical projection storage mode

**Inspect:** `database/canonical.go`, `database/database.go`, `schema` persistence and `database/migration.go`.

The current `CanonicalProjectionInline` versus `CanonicalProjectionExternal` choice is caller-supplied, not durable database configuration. Persist the database-wide mode as authoritative configuration in `schema.dat` (or a clearly justified existing schema-owned metadata section, never Pebble). A fresh database selects its mode during creation; subsequent opens must read and enforce it. Do not silently switch modes because different `Options` were supplied.

- For external mode, require a valid `ProjectFromRecord` callback at every operation/recovery path needing reprojection; fail explicitly if unavailable.
- Validate mode consistently during startup, migration, canonical reads, writes, tombstones, and full index rebuild.
- If changing modes is supported, implement an explicit durable restart-only migration using PREPARED/ACTIVATE, with recovery after interruption. Otherwise reject mode changes clearly.
- Add creation/reopen tests for both modes, wrong/missing callback, mismatch, crash during migration, and post-rebuild projection equality.
- Strongly consider optional destination-based `ProjectInto` for reuse of caller-owned output buffers; retain current callback where useful unless API simplification is justified by pre-V1 status.

### 5. Validate journal v2 operation correctness

**Inspect:** `database/journal.go`, `database/coalesce.go`, `database/coordinator.go`, `index/mutation.go`.

Audit durable envelope validation, typed association bounds, duplicate handling, insert versus no-op, delete/liveness, `index.Batch.Touch`, and idempotence. In particular verify every encoded family (time, geo, exact hash, SimHash, bitmap including BSI, and all vector element kinds) has deterministic encode/decode round trips and fails closed on unknown/invalid family, kind, count, length, and overflow. Preserve sorted/set semantics and NaN/signed-zero comparisons as applicable. Do not mistakenly decode workspace-only references from disk.

Test random interleaved changes, update→revert, insert→update with unchanged+changed associations, delete→reinsert, empty projection, replay after failed index apply, and reopen. Compare against an ordered sequential reference implementation. For operations where coalescing is not proven safe (SimHash/vector, lifecycle transitions), preserve ordered fallback; do not force enablement.

## P1 — Complete Phase 2 performance and lifecycle work

### 6. Reuse journal encoder memory

`database/journal.go` currently constructs a new byte slice in `encodeMutation`. Add an internal destination-oriented encoder (e.g. `appendMutation(dst []byte, ...)`) and reuse a commit-owned buffer where lifetime allows. Benchmark current encoding versus scratch encoding and Pebble v2 deferred Set. Do not overwrite a buffer still borrowed by Pebble. Favor typed deterministic encoding, zero intermediate buffers on warmed paths where feasible, and explain remaining heap escapes. Adopt only measurable wins.

### 7. Measure **total** write cost for realistic photographic updates

Extend `internal/bench`, existing benchmark conventions, and `AGENTS/Bench.MD`; do not create a parallel benchmark framework. Cover inline versus external canonical mode on inserts, rating/favorite changes, tag changes, GPS updates, AI classification, embedding change, deletion/reinsert, and mostly unchanged projections. Use deterministic but nonuniform camera/lens/time/GPS distributions.

Report per workload and per batch size (1, 16, 32, 64):

- Canonical KV value bytes, journal bytes, secondary-index logical writes.
- Pebble WAL/flush/compaction metrics when available, with accurate labels.
- Actual filesystem/device bytes only when separately instrumented; do not present `Stats()` as fsync or device I/O.
- `ns/op`, `B/op`, `allocs/op`, throughput, and p50/p95/p99 read latency during ingest.
- SimHash write amplification, segment count, compaction bytes, recovery time.

Adoption criteria: target **≥40% less journal data and ≥15% less combined logical write volume** on representative *metadata-update* workloads. Treat these as targets rather than blanket guarantees (tiny deltas may get bigger due to envelope overhead). Avoid regressions over ~5% in throughput or read p99 without documented justification. Use statistical comparison, not a single noisy run. Record hardware/Go version; Linux CI and Apple M4 Pro developer measurements are separate environments.

### 8. Scale-test sparse SimHash and tune compaction

Benchmark sparse populations 10K, 100K, and 1M; keep 10M behind the existing `IMAGEIDX_BENCH_LARGE` opt-in. Include gaps, out-of-order IDs, nonzero full `uint64` IDs, repeated hot-ID upserts, mixed dense/sparse, tombstones, nearest-match search, and reopen/replay.

Report:

- Apply throughput versus live ID count and stale-log history.
- Query latency and allocations for pHash, PDQ scalar, PDQ bulk.
- Peak/retained memory and snapshot publication bytes per update.
- Disk amplification and compaction cost.
- Startup/recovery duration.

Tune segment capacities and compaction thresholds based on measured read/write/space tradeoffs. Preserve `Result{ImageID, Distance uint8}` and ordering contracts.

### 9. Tighten projection-version migrations

`schema.NamespaceDef.ProjectionVersion` and definitions format v3 already exist. Keep projection-semantic versions **per namespace**, outside engine-specific `Config`, and avoid resorting registration-order definitions. Review `ProjectionMigration` startup flow for version-only changes and namespace additions.

- Compute affected namespaces exactly and reject unrelated kind/entity/config changes.
- Reject unsupported version downgrades unless an explicit conversion exists.
- Require a projection callback that can rebuild from authoritative canonical application records for affected namespaces.
- Reuse crash-safe PREPARED/ACTIVATE; activate new definitions only after valid index replacement is durable.
- Rebuild only affected physical indexes **if** the unified index has a safe namespace-granular replacement mechanism; otherwise keep the documented full rebuild and avoid inventing partial generation publication.
- Cover interrupted preparation, activation, repeated restart, wrong projector, corrupt source, and complete query equivalence after migration.

Because old experimental database compatibility is not required, remove obsolete compatibility paths if they add complexity without value. Preserve fail-closed behavior.

### 10. Document per-family batching and replay capabilities

Add a small, code-adjacent capability contract describing the actual guarantees for bitmap/BSI, time, geo, exact hash, SimHash, and vector engines: coalescing-safe, delete-by-ID, replay-idempotent, and batch support. Prefer explicit typed switches/static metadata, not runtime maps or configurable promise flags. Define an index publication unit as the contiguous journal interval durably applied and acknowledged together. Keep SimHash/vector coalescing disabled unless sequential-versus-batched equivalence and crash recovery are proven. Vector batching can remain a benchmarked future improvement.

## Process and deliverables

1. Re-read the **latest** PR #25 head and package instructions, then produce a short status matrix showing which items are already done, partial, or missing.
2. Fix P0 correctness/API gaps first; create focused tests **before** changing large storage structures.
3. Implement bounded sparse publication/compaction behind existing unified-index behavior and benchmark it against the current overlay.
4. Complete P1 efficiency and migration changes; don't expand scope into query planner or global memory allocator redesign.
5. Run `go build ./...`, `go test -count=1 ./...`, `go test -race -count=1 ./...`, `go vet ./...`, `sigmap validate`, and applicable corruption/crash tests; honor `AGENTS.md` checks.
6. Verify no stale workspace references, no reader use-after-unmap, no lost acknowledged generations, and no unsupported version silently accepted.
7. Provide before/after benchmark tables, physical-versus-logical metric definitions, indexed/query-state comparisons, and any explicitly deferred work.
8. Do not declare merge-ready until all P0 issues have passing tests, the index can recover from the new publication and compaction crash boundaries, and there is no significant unexplained performance regression.

**Do not mischaracterize existing work:** v2 typed deltas, two canonical modes, per-namespace projection versions, sparse-ID support, and conservative coalescing are already implemented in PR #25. This follow-up hardens and optimizes those features; it does not request a second implementation of Phase 2 from scratch.
