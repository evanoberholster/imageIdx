# ImageIdx — Final V1 Stabilization Instructions for Codex

**Objective:** Ship a stable, correct, documented ImageIdx V1 without another architecture or performance phase.

**Decisions approved by the maintainer:**

1. **Primary API:** `database.DB` is the main application entry point. Keep `schema`, `search`, `index`, and `store` available for appropriate advanced/standalone use; do not create a top-level `imageidx.DB` wrapper.
2. **Search convenience:** Provide search through `database.DB` **only if** it retains the performance and explicit workspace control of the existing search API. Preserve `search.Executor` / low-level search entry points.
3. **Persistence:** Guarantee V1 compatibility for canonical records and `schema.dat` (subject to explicit, versioned migrations); derived indexes are rebuildable, versioned state, **not** forever-stable on-disk formats.
4. **Vectors:** Existing vector functionality remains **experimental**, not a V1 stable API guarantee. Do not redesign or batch vectors to ship V1.
5. **Release gate:** Correctness, stable documented public APIs and durability, passing supported-platform CI, and no serious measured performance regressions. **Do not** require another 15% throughput or write-amplification gain.

## Binding constraints

- Prioritize **correctness → simple public contract → measurable performance → memory efficiency**. Avoid speculative architectural work.
- V1 does not need to open arbitrary **pre-V1** experimental databases. V1 must reopen V1 databases safely and reject unsupported/corrupt formats explicitly. Do not silently reinterpret data.
- `schema.dat` is the authority for schema, namespaces, entity ID allocation, projection versions and database-wide configuration. Pebble owns canonical KV records and the mutation journal; secondary indexes remain derived and rebuildable.
- Preserve current 12-byte key representation, or explicitly document and test the final V1 meaning of each field. Do not casually reinterpret `Flags`/version during this release.
- Preserve journal durability, ordering, source-generation barriers, crash recovery, single coherent multi-index query snapshots, and the current serialized publication protocol. Do not introduce per-family parallel writers or asynchronous coordinator-buffer ownership.
- Respect project `AGENTS.md` instructions. Prefer concrete typed slices/arrays, reuse workspaces, and avoid hot-path maps/reflection unless measured.
- **Low priority:** selective namespace/family corruption rebuild. Keep existing full-index recovery with fail-closed behavior; no new selective recovery mechanism is required.
- No Phase 4 work, adaptive batching redesign, new SIMD architecture, new indexing families, storage allocator rewrite, ranking redesign, or broad vector work.

## Start: inspect, inventory, and freeze scope

1. Start from the latest intended V1 integration baseline. Inspect `main`, open Phase 3 PRs, and the exact HEAD SHA; **do not** assume Phase 3 is merged. Finish/merge outstanding correctness changes first, then branch V1 stabilization from the verified baseline.
2. Read `AGENTS.md`, `database/README.md`, `schema/README.md`, `index/README.md`, `search` docs, `AGENTS/Bench.MD`, and pertinent format specifications.
3. Inventory exported identifiers with `go doc`, code search, and package documentation across `database`, `schema`, `search`, `index`, `store`. Classify as **stable V1**, **advanced stable**, **experimental**, or **internal-only**. Find redundant options, ambiguous ownership, surprising error semantics, and API paths that require manually starting stores.
4. Produce a brief `AGENTS/Refactor/V1-API-Decision.md` summarizing final signatures and any intentionally deferred features. Do not use this to propose new architecture.
5. Record baseline commands, benchmark environment, allocation counts and representative query/commit measurements **before** changing public APIs.

## PR V1-1 — Finalize public API and contracts

### A. Primary database API

Make `database.DB` the documented main entry point for creating/opening, committing, waiting for indexing, getting records, searching, checkpointing when desired, and closing. Prefer existing methods over aliases. Ensure the basic workflow does not require separately opening low-level index or Pebble stores unless bootstrap genuinely needs it; if index creation is an explicit prerequisite today, decide one clear documented V1 creation path without changing persistence behavior unexpectedly.

Document concrete V1 examples that compile against the **actual** API, not hypothetical signatures:

- Create/open from persisted schema definitions and application projection decoder/projector.
- Commit an insert/update; obtain a generation; wait for the generation before asserting search freshness.
- Search for ImageIDs; optionally retrieve canonical data.
- Delete/reinsert, reopen, add namespace after close/reopen, and close.

Avoid adding redundant CRUD methods if `Commit(Mutation...)` already expresses the behavior cleanly.

### B. Search façade — conditional on performance

- Evaluate the current `search.Executor`, `search.Workspace`, query-plan setup and result APIs. Build the thinnest possible convenience API from `database.DB` without an import cycle. **Do not assume `database` can import `search` safely**: inspect package dependencies first. If adding a method causes cycles or hides expensive allocations, use a small adapter/factory or an explicitly documented integration method instead.
- The high-performance route must continue to allow caller-owned `search.Workspace`, destination slices and snapshot-controlled execution. A simple convenience route may allocate if documented, but must not replace or penalize the allocation-conscious path.
- Benchmark equivalent queries using the façade versus direct executor: single bitmap, multi-index intersection, time/geo filter, and selective/large candidate sets. Compare ns/op, B/op, allocs/op, p50 and p99 where measurable. If a direct `DB.Search` wrapper imposes material overhead or unsafe ownership, **do not merge it**; ship a documented zero-cost advanced integration API instead.
- No canonical-record fetch on the ImageID-only hot path. Optional late materialization must be opt-in; do not redesign scoring or similarity hash APIs.

### C. Public contracts

For each stable exported method/type, document:

- Ownership of input/output byte slices, projection data, destinations, iterators, snapshot pins, and workspaces. `DB.Get` should return caller-owned bytes. Inputs to synchronous `Commit` must be safe to reuse after it returns, unless an explicit and tested alternative contract applies.
- Concurrency: which calls are safe concurrently; which workspaces/batches are single-owner; behavior of `Close` with active reads/writes; context cancellation and error handling.
- Consistency: `Commit` means canonical data + journal durably committed; `WaitIndexed(ctx, generation)` gates visibility in derived indexes; `Sync` and `Checkpoint` have separately documented semantics. Ordinary searches may lag the latest canonical commit unless the caller waits.
- Failure modes: distinguish invalid inputs, closed state, unavailable projection callback, unsupported on-disk version, inconsistent/corrupt schema/journal, indexing lag, rebuild required, and cancellation.
- Configuration invariants: persisted canonical projection mode, namespace/definition changes requiring restart, migration preconditions, and immutable options after first create.

Use stable sentinel errors suitable for `errors.Is`; do not expose internal errors or panic on valid public inputs. Preserve error wrapping and report the relevant entity/namespace where safe.

### D. Stable vs experimental packages

- Mark vector-related APIs/features as **experimental** prominently in package docs and examples. Keep existing functionality working and tested; do not imply V1 guarantees for vector file formats, performance, or APIs. If an exported symbol is already required by stable cross-family types, retain it with explicit experimental scope instead of breaking other families.
- `index` / `store` remain advanced APIs, but their exposed low-level behavior must not undermine database-owned correctness when used through `database.DB`.
- Eliminate duplicate or unused compatibility wrappers for pre-V1 code only where the removal demonstrably simplifies the API. Minimize unrelated API churn.

**V1-1 exit gate:** Public API inventory complete; examples compile; documented ownership/concurrency/durability semantics; benchmarked search integration without a material fast-path regression.

## PR V1-2 — Persistence, correctness and concurrency closure

**No new storage architecture.** Audit and fix only reproducible defects in existing behavior.

### Persistence contract

- Document and golden-test V1 `schema.dat`, canonical record envelope, key semantics, and journal format. Roundtrip and fail-closed tests for malformed lengths, versions, CRCs, namespace identities, and invalid flags.
- Confirm canonical and schema restart compatibility across V1 builds/fixtures where feasible. Index files may evolve only through explicit version detection and a safe rebuild route; do not promise cross-version binary compatibility for derived index files.
- Verify stored projection mode is enforced at reopen. In external canonical mode, deterministic compatible `ProjectInto`/`ProjectFromRecord` must be required for updates and recovery.
- Confirm schema additions require closing/reopening, and projection-version changes use explicit migration with durable PREPARED/ACTIVATE behavior.
- Audit migration validation so a changed `ProjectionVersion` **cannot mask** a simultaneous forbidden engine `Config`, kind, entity, or namespace change. Test the combined-change case explicitly.

### Functional test matrix

- Fresh create → commit → close → reopen → retrieve/search.
- Inserts, metadata-only updates, unchanged associations, updates reverting to initial state, deletes and reinserts, empty/no-op projections and liveness.
- Sparse/nonconsecutive/out-of-order and large `uint64` ImageIDs for supported families; no duplicate search results.
- Compound bitmap/time/geo and exact/perceptual-hash searches; existing vector smoke/reopen tests under experimental designation.
- Namespace addition and invalid migration failure behavior.
- Coalesced vs strictly sequential application equivalence, including crash and replay boundaries; generation acknowledgment must never outrun durable index publication.

### Crash and concurrent execution

- Crash before/after canonical sync; during index application/publication; before/after acknowledgment and cleanup; interrupted schema activation; restart/replay produces the same logical searchable state.
- Concurrent ingest and search: a query must observe one coherent published generation across participating indexes. Writer progress must not require holding a query-wide writer mutex. Snapshot release must be safe on all error/cancellation paths.
- Reader-memory reclamation must not unmap an active snapshot. **Deferred epoch reclamation optimization is not a V1 blocker** unless testing reveals unsafe reclamation, unbounded memory, or unacceptable stalls.
- `Close`, canceled waits, indexing errors and rebuilding must not leak locks, goroutines, files, or borrowed buffers.
- Retain existing `RecoveryManual`/`RecoveryAutomatic` full-index rebuild and fail-closed corruption detection; selective recovery is explicitly deferred.

**V1-2 exit gate:** Deterministic tests + process-crash tests + race tests pass; no known data-loss, incorrect-search, or format-misinterpretation defect.

## PR V1-3 — Documentation, performance baseline and release

### Documentation

- Provide a clear root `README.md` quick start, supported package matrix, durable/derived-data distinction, version policy, and experimental vector warning.
- Provide one runnable V1 example demonstrating schema, open, commit, `WaitIndexed`, search and `Get`, close/reopen. Add an advanced example for workspace-owned low-allocation search if useful.
- Document limits: 64 mutation batch cap unless current code changes for independent reasons, maximum projection bytes, ID rules, snapshot ownership, sync semantics, source-generation eventual consistency, and platform support.
- Mark low-priority roadmap items as **post-V1** without implementing them.

### Release baseline and performance gates

- Reuse `internal/bench` and `AGENTS/Bench.MD`; no parallel benchmark framework. Measure representative inserts, updates, deletes, compound searches, sparse SimHash lookups, concurrent ingest+search, reopen, and memory use on supported hardware.
- Compare to a pinned pre-stabilization commit with same compiler/build flags/hardware. Report ns/op, B/op, allocs/op, throughput and representative latency distributions; distinguish application-level logical byte counters from true filesystem/device I/O.
- **No throughput improvement quota.** Investigate and fix material unexplained regressions; correctness and memory safety override small performance differences. Avoid adding speculative optimization work to meet an arbitrary percentage.
- Large 10M dataset benchmarks remain opt-in via `IMAGEIDX_BENCH_LARGE`; do not make them mandatory CI jobs.

### Validation commands

Run from the final integration commit at minimum:

```bash
go fmt ./...
go build ./...
go test -count=1 ./...
go test -race -count=1 ./...
go vet ./...
sigmap validate
```

Also run the repository's supported CI (including relevant Linux amd64/arm64 jobs), format/corruption/golden tests, and benchmark comparison. If a command or environment is unavailable, report it explicitly, do not claim it passed. Confirm the release tag points to the exact tested commit.

### Versioning policy

- After V1: changing **canonical-record or `schema.dat`** semantics requires a version bump and explicit compatible migration or clear unsupported-version rejection.
- Derived index files are caches/rebuildable projections: maintain version checks and rebuild capability rather than promising permanent binary stability.
- Journal replay must remain correct for a committed V1 database across normal V1 maintenance; never acknowledge entries that cannot be applied or recovered.
- API compatibility is a release promise for the documented stable public API, not for explicitly experimental vector components.

**V1-3 exit gate:** Docs and runnable examples match code; supported CI green; pinned benchmark report; final release checklist; V1 tag ready.

## Strict stop rule — avoid endless production

After V1-1, V1-2 and V1-3 satisfy their exit gates, **stop adding features and tag V1**. Do not reopen architecture choices because a future optimization might be faster. Only block release for demonstrated failures in durability, correctness, security, thread safety, documented stable APIs, supported-platform CI, or severe performance/memory regressions. Record other issues as post-V1 work.

### Explicitly deferred

Parallel per-family writers; broad batch-size/adaptive batching changes; vector batch insertion or graph redesign; vector stabilization; additional SIMD/SoA redesign; sparse SimHash structural changes absent a demonstrated severe defect; new cardinality systems; new ranking machinery; selective namespace/family corruption recovery; new memory allocators; extensive per-engine refactoring; speculative zero-allocation polish unrelated to the stable API.

## Required Codex completion report

For each PR provide: (1) exact SHA and changed files, (2) stable/experimental public API diff, (3) tests and CI with actual results, (4) measured benchmark comparison, (5) any unresolved **V1-blocking** defects, (6) deferred nonblocking items, and (7) explicit **merge / do not merge** recommendation. Never claim verification from unexecuted commands.
