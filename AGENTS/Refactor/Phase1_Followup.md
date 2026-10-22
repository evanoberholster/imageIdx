# ImageIdx — Phase 1 Follow-up / Pre–Phase 2 Closure

**Target:** `evanoberholster/imageIdx`, PR #24 (`refactor-index-design`, reviewed at `44700968`).
**Goal:** Finish and validate Phase 1 before starting Phase 2. **ImageIdx has not been released/deployed: backward compatibility with existing on-disk databases, historical test fixtures, and earlier APIs is NOT a requirement.** Correctness of **freshly created** databases, clean restart, crash recovery, low allocation, and consistent durable state **are** requirements.

> Codex: inspect the current branch before editing. This document describes the review at `44700968`; if code has changed, adapt to actual implementation and report deviations. Follow repository `AGENTS.md` and repository verification instructions. Work on PR #24 (or a dedicated follow-up branch), keep the changes narrowly scoped, and do not begin Phase 2's journal-delta, projection-schema migration, or sparse-SimHash redesign.

## 1. Required first step: inventory, reproduce, and baseline

Inspect `database/coalesce.go`, `database/coordinator.go`, `database/workspace.go`, `database/projection.go`, `database/database.go`, `database/journal.go`, `database/recovery.go`, `index/batch.go`, `index/mutation.go`, `store/store.go`, and the existing regression/benchmark tests. Read `AGENTS.md`, `AGENTS/Bench.MD`, and `internal/bench` before adding infrastructure.

Capture the PR head SHA, CI status, Go version and hardware. Run existing focused tests and benchmarks before changing code. Separate cold-start workspace allocations from warmed steady-state measurements; Phase 1's overall commit/index throughput and allocations were reported as effectively unchanged, despite the large improvement in projection diff. Find the actual bottlenecks before expanding abstractions.

**Do not reproduce the previously suspected insert→update bug as an asserted fact.** At reviewed head, `applyCoalescedGroups` calls `batch.Add(lastEvent.after)` for an insert followed by updates without a delete. The `initial = firstEvent.after` path is guarded by an unsafe delete/reinsert classification and is not the ordinary insert→update path. Add coverage nevertheless.

## 2. Add merge-blocking coalescing regression tests

Add an end-to-end test exercising one publication interval containing:

1. Insert image `42` with two distinct associations: camera=`Canon`, rating=`3`.
2. Update the same image: camera remains `Canon`, rating changes to `4`.
3. Interleave an update to a different image between those two operations to exercise nonadjacent grouping.
4. Apply/acknowledge the complete journal interval, query both indexes, and verify camera=`Canon` and rating=`4` are present, rating=`3` is absent, and entity liveness is present.
5. Close and reopen the database; repeat every assertion, including indexed generation.

Cover each critical mutation sequence (with distinct entity IDs and interleaving where relevant): add→update, update→update, update→revert (net zero), add→delete, update→delete, delete→reinsert (must use safe sequential fallback), and an empty projection/no-op generation advance. Test both `Decoder` and `DecodeInto` paths. Ensure a skipped/no-op index mutation still advances the *durable* index source generation and database acknowledgment correctly.

**Invariant:** Applying each journal record in order produces exactly the same final index associations, entity liveness, and durable generation as the coalesced path. Use deterministic randomized/property tests for sequences up to 64 journal records, and compare against a reference sequential model. Do not assume all index families support coalescing: retain conservative fallback for SimHash, vector, and delete→reinsert until specifically proven otherwise.

## 3. Fix/verify workspace and decoder ownership

Review `dataRef{slot, offset, length, epoch, valid}`, ordinary 64 KiB blocks, oversized buffers, and `coordinatorWorkspace.reset()`.

- Keep explicit presence and delete flags; empty bytes, missing bytes, and delete are not interchangeable.
- Validate stale references after reset; make malformed/overflowing block references safely reject rather than reading wrong memory. Internal invariant violations may panic in debug/test-only checks, but corrupted persisted journal inputs must return errors, not uncontrolled panics.
- Ensure a journal entry is copied **once** into stable coordinator-owned memory while Pebble iterator bytes remain valid. This is copy consolidation, **not zero-copy**.
- Keep the maximum of 64 journal records per coordinator batch unless benchmarked and intentionally changed. Treat `MaxProjectionBytes` as the *sum of copied before plus after projection payload bytes*, not a physical memory cap. A single oversized record may form its own batch.
- Keep 64 KiB ordinary blocks; retain up to ~1 MiB in ordinary blocks and at most one reusable oversized buffer within `OversizedRetentionBytes` (currently 4 MiB default). Verify oscillating small/large batches do not repeatedly allocate in the common reusable range and do not retain unbounded memory after spikes.
- Zero pointer-bearing slots of `decoded`, `diffs`, staged operations, and scratch when their lifetime ends, **where those slots would otherwise retain old backing storage**. Retain desired backing-array capacities without accidentally pinning oversized/obsolete buffers. Distinguish scratch intentionally retained for reuse from accidental retention.
- Confirm `DecodeInto` may reuse destination slices, but no borrowed `encoded`/scratch bytes reach index-owned persistent state after `Apply` returns. Explicitly test bitmap byte values and vector slice aliasing. A decoder is permitted to borrow input only until the documented synchronous publication boundary.
- Keep the single synchronous coordinator/index apply model in Phase 1. Do not introduce async publication or shared coordinator workspaces; that would invalidate the reset contract.

Add tests for 0/1/65535/65536-byte payload boundaries, block reuse, oversized reuse/discard, corrupt payloads, reset-on-error, stale refs, and repeated reopen. If using oversized length arithmetic or `uint32` block offsets, validate lengths and integer conversions before arithmetic.

## 4. Verify namespace bitmap without modifying schema serialization

PR #24 replaces only `store.Store`'s KV namespace membership map with an **unexported** 8 KiB `uint16` membership bitmap. Keep it.

- `schema.Definitions.defs` must retain registration order as currently required by consumers. Do not sort it in place or conflate it with `store` membership.
- `index.Store` already has its own dense route lookup. Do not duplicate it with another bitmap or typed-handle layer.
- `schema.Definitions` has separate identity/name indexes. Do not rewrite those just to enforce a blanket no-map policy: evaluate cold-path maps separately.
- Verify bit boundaries (`0, 63, 64, 65535`), unknown namespaces, all KV kinds, reserved IDs, and namespace registration before/after open. The bitmap should be immutable while open.

Because **there is no backwards-compatibility requirement**, Codex may simplify unused legacy registry helpers or fixtures if needed; however, preserve the current new-database schema authority (`schema.dat`), namespace-addition-on-restart contract, on-disk integrity checks, and prepared/active recovery. Do not change `schema.dat` or the journal merely to optimize membership checks.

## 5. Verify typed sort-and-merge correctness and cost

`changedProjection` already suppressed unchanged associations before Phase 1. PR #24 replaced the previous O(N×M) comparison with typed sort-and-merge. Focus on validating the replacement; do **not** add another diff layer.

- Confirm input slices are not destructively sorted when they are caller/decoder-owned.
- Ensure merge handles exhausted sides without out-of-bounds reads; enforce set semantics for duplicate associations and validate conflicting values in single-valued namespaces.
- For vectors, test NaN and signed-zero semantics against previous behavior; do not accidentally treat nonidentical float payloads as identical. For byte-bearing bitmap/hash values, ensure comparison uses content rather than slice identity.
- Test 0, 1, 4, 16, 64 and 256 associations; 100% unchanged, one change, 100% changed, duplicates, mixed families, mixed namespaces.
- The reported one-association case regressed from ~67 ns to ~121 ns despite reduced allocations. Benchmark a simple **single-entry fast path** before sorting (and small-array alternatives only if justified). Adopt only when it preserves semantics and improves end-to-end workload, not just a microbenchmark.
- Check lifetime of `projectionScratch` slices returned to `index.Batch`, especially after `Reset`, apply errors and retries.

## 6. Coalescing safety and publication boundaries

`database.Commit` currently rejects duplicate `(EntityKind, ValueID)` pairs in a *single canonical transaction*. Keep that rule unless an independent change explicitly redefines it. Coalescing concerns **multiple already-committed journal records** in a contiguous coordinator interval, not duplicate mutations in one `Commit`.

- Group keys using bounded typed arrays/slices (64 records); no map allocation is required.
- Preserve the **first before-state and final after-state**. For add→update with no delete, publish `Add(final full projection)`, not the difference relative to the first inserted version.
- Interleaving distinct IDs is allowed only when final semantics are provably equivalent; keep source sequence ordering and acknowledge only after the entire effective interval is durably represented.
- A final delete must clear liveness; delete→reinsert retains sequential fallback because its transient liveness behavior is not proven safe for coalescing.
- SimHash dense append and vector graph updates retain ordered fallback. Ensure a mixed-family interval cannot silently bypass these checks.
- Keep index-family grouping from reordering same-entity mutations unsafely. Document the exact definition of a publication unit and where a fallback splits it.
- Test injected failures before index apply, during per-engine apply, after durable source generation and before acknowledgment, during journal cleanup, and at startup recovery. Verify crash behavior and generation invariants for both coalesced and fallback batches.

If tests uncover a correctness failure, prioritize the simple conservative fallback over adding a large general transaction/coalescing framework.

## 7. Instrumentation and accurate performance reporting

Keep `DB.Stats()`, `store.Store.Stats()`, and `index.Store.Stats()`; document these as **application-level counters and logical byte estimates** unless physical I/O is directly measured.

- A single canonical Pebble batch may contain canonical data plus journal entries. Do not misreport them as two physical commits.
- Distinguish canonical batch commits, generation-intent commits, index apply/publication, acknowledgment commits, cleanup commits, logical journal bytes, and available engine-specific counters.
- Do not claim fsync count, device bytes, or write amplification measured at the storage device from logical counters alone.
- Optional diagnostic physical-I/O measurement can be run with OS/filesystem tools, but keep it outside the per-mutation hot path. Platform-dependent results must be labeled as such.
- Extend `internal/bench` and existing `AGENTS/Bench.MD`; do not build a duplicate benchmark suite. Respect `IMAGEIDX_BENCH_LARGE` for opt-in large datasets.

Measure at least: 1/16/32/64 commit batches, repeated metadata edits, coalescible and non-coalescible journals, projection sizes, read-under-write, warmed/cold workspace, allocations (`B/op`, `allocs/op`), p50/p95/p99 where reliable, journal bytes, and index operations. Compare with original `main` baseline and report statistically meaningful results using `benchstat` where possible. Distinguish throughput improvements from faster microbenchmarks.

## 8. Optional decoder and Pebble encoding policy

The current `database.Decoder func([]byte) (index.Entry, error)` remains supported. The new `Options.DecodeInto` may be the preferred optimized API for new consumers, but avoid forcing a breaking change merely for allocation statistics.

Because the project has no released compatibility obligations, public API cleanup *is permitted*, but **only** where it simplifies semantics or ownership; document any change and update all in-repository call sites/tests. Do not introduce a new application-specific MessagePack/EXIF dependency into the generic database package.

Retain the benchmark-driven decision on Pebble `SetDeferred`. The PR reports better raw batch microbenchmark results but the generic database lacks application-specific encoded lengths. Implement direct deferred encoding only for concrete repository-owned records where the length is cheaply known and the complete commit path benefits measurably. Preserve atomicity and correct `Finish` behavior; otherwise leave it unused.

## 9. No compatibility requirement — what that permits and what it does not

**Permitted before first release:** remove unused compatibility paths, rename internal types, revise APIs used only within the repository, regenerate golden fixtures, and simplify on-disk versioning *if there is a separate demonstrably useful correctness/simplicity reason*. Fresh databases and fresh fixtures may use the final chosen format.

**Not automatically permitted in this Phase 1 closure:** redesign canonical keys, invent journal v2 deltas, change index segment layouts, change schema projection semantics, modify SimHash ID layout, replace Pebble, or introduce multi-engine atomic transactions. Those are Phase 2/after Phase 2 design choices and should be treated separately even though migration is unnecessary.

**Still mandatory:** successful fresh Create/Open/Close/reopen, namespace additions only through the intended restart/activation procedure, data persistence across clean restart, crash-safe publication, deterministic recovery, correct index state, race safety, and testable corruption handling. No backwards compatibility does **not** mean no restart/recovery correctness.

## 10. Verification and completion gates

Run repository-required checks (including `sigmap validate` if configured in `AGENTS.md`) and, at minimum:

```bash
go fmt ./...
go build ./...
go test ./...
go test -race ./...
go vet ./...
go test ./database ./index ./store ./schema -run '^$' -bench . -benchmem
# For affected implementation files AND benchmark/test code:
go test -gcflags='-m=2' ./database ./index ./store ./schema -run '^$'
```

Use actual module/package paths and narrow benchmarks when the full suite is costly. Run crash subprocess tests and randomized equivalence tests explicitly. Verify clean restart **and** forced termination around each durable publication point. CI passing alone does not establish the performance claims.

### Codex completion report

Provide:

1. Exact base/head SHA and files changed.
2. Which Phase 1 correctness gaps were fixed, including the insert→update unchanged-association/liveness test.
3. Workspace and decoder ownership contract, including proof index-owned data never aliases reset blocks.
4. Coalescing family restrictions and all tested generation/crash boundaries.
5. Before/after benchmark table, with **steady-state vs cold-start** distinguished.
6. Logical-vs-physical-I/O measurement caveats.
7. Tests, CI, race checks, escape-analysis findings, and any skipped checks.
8. Remaining limitations explicitly handed to Phase 2.

**Stop condition:** Do not declare Phase 1 complete or ready for merge until the regression tests, restart/crash tests, allocation checks, and performance analysis establish correctness. Do not begin Phase 2 implementation during this closure work.
