# ImageIdx — Phase 3 Implementation Instructions for Codex

**Repository:** `evanoberholster/imageIdx`
**Baseline:** Merge and validate PR #25, then branch from updated `main` (or document the exact approved baseline SHA).
**Goal:** Improve write throughput and total write amplification; add cost-aware search planning, coherent cross-index query snapshots, safe memory reclamation, and selective recovery. **Vector batching is Phase 4, not Phase 3.**

> This document supersedes earlier Phase3.md drafts. Implement the decisions below, not the assistant's earlier defaults. ImageIdx is pre-V1: internal/public APIs and experimental disk formats may change when justified, but fresh-database durability, recovery, integrity and correctness are mandatory. Do not add compatibility scaffolding for abandoned pre-release formats.

## 0. First close Phase 2

1. Inspect the **latest** PR #25 head, CI, `AGENTS.md`, `AGENTS/Bench.MD`, relevant package documentation and existing benchmarks before editing. Do not assume a historic reviewed SHA remains current.
2. Fix/verify `database/migration.go:validateAddition` checks **all** immutable namespace fields independently. A simultaneous `ProjectionVersion` bump and `Config` change must be rejected, including when combined with namespace addition. Test the attempted bypass.
3. Verify sparse SimHash scalar/bulk query parity, concurrent reader safety, compaction/replay crash boundaries, canonical projection mode persistence, journal v2 replay/coalescing, and restart-only projection migration.
4. Run build, tests, race, vet, `sigmap validate`, and repository CI. Merge PR #25 only after relevant gates pass. If merge is not possible, document the blocker and use an explicit base SHA; do not silently mix unresolved Phase 2 fixes with Phase 3 changes.
5. Establish a clean Phase 2 benchmark baseline before changing algorithms. Clearly separate PR-reported results from independently reproduced measurements.

## 1. Binding architecture decisions

| Decision | Approved direction | Implementation consequence |
|---|---|---|
| 1. Writers | **B — one writer per index family** | Permit parallel family execution, but retain **one database coordinator and one global publication/acknowledgment authority**. No independently visible partial publication. |
| 2. Grouping | **B + C — family → namespace, then physical persistence target** | Use typed contiguous slices, namespace grouping and target-aware flushes. Preserve operation order where required. |
| 3. Batching | **B — benchmark-driven** | Add/extend only families with proven benefit. No obligatory generic engine interface. |
| 4. COW | **D — engine-specific** | Container/page/segment/view granularity as appropriate; avoid whole-index copying. |
| 5. Coalescing | **B — extend only when proven equivalent** | Preserve conservative SimHash/vector/delete-reinsert fallbacks until engine-specific proof exists. |
| 6. Planner | **C — lightweight cost-based** | Bounded cost model; no general SQL optimizer. |
| 7. Stats | **B + D, whichever benchmarks better** | Compare write-maintained approximate counters against counters plus selective runtime estimates. Prefer simpler/faster accurate-enough design. |
| 8. Filtering | **C — current hybrid approach** | Preserve existing bitmap/streaming/SIMD paths; optimize only with evidence. |
| 9. Materialization | **B — late** | Filter and rank by IDs first; fetch canonical records only when required. |
| 10. Ranking | **A — preserve** | No score-model redesign; similarity/exact hash remain independent APIs. |
| 11. Consistency | **B — coherent query generation** | All participating indexes must represent one logically committed publication. |
| 12. Publication | **Mostly A — atomic pointer to immutable view** | Prefer one atomic root/snapshot; use hybrid narrow synchronization **only if measured superior**. |
| 13. Reclamation | **B — epoch-based** | Extend/reuse `storage.ReaderTracker` if suitable; demonstrate safe mmap retirement. |
| 14. Readers/writers | **B — readers do not acquire writer locks** | Reader epochs may delay reclamation, never routine publication. Not necessarily wait-free. |
| 15. Recovery | **D — smallest independently recoverable unit** | Rebuild namespace/physical group depending on true durability boundaries. |
| 16. Recovery policy | **C — configurable** | Preserve manual/automatic behavior; never silently serve incomplete results. |
| 17. Source of truth | **Yes — canonical Pebble** | Every derived index rebuildable from canonical application records with required deterministic projector and `schema.dat`. |
| 18. Gates | **Accepted** | ≥15% write throughput improvement and ≥15% total logical write reduction on representative workloads; search p50/p99 ≤5% regression, retained memory ≤10% growth unless justified. |
| 19. API | **B — justified changes allowed** | Change public APIs only where demonstrably useful; simplify pre-V1 formats as necessary. |
| 20. Vector | **Deferred to Phase 4** | Do not implement vector batching, graph restructuring or vector-specific write optimization in Phase 3. Preserve current semantics and cover with regressions. |

**Priority of principles:** correctness and durability first; then measured throughput, latency, memory and write amplification; then simplicity. Avoid hot-path maps/reflection and unnecessary allocations. Do not prohibit maps in cold-path recovery/planning setup where they demonstrably improve clarity and do not affect hot paths.

## 2. Coordinator and family-writer protocol — REQUIRED DESIGN BEFORE CODING

The selected writer model **changes Phase 2's serialized engine apply**, but must **not** weaken atomic multi-index publication. First write `AGENTS/Refactor/Phase3-Writer-Protocol.md` (or the repository's matching design location) showing precise states, ownership, error handling and crash points; then implement.

1. **Single journal sequencer:** `database.Commit` and journal sequence allocation remain authoritative; journal order and duplicate-key rules unchanged unless separately proven safe.
2. **Bounded coordinator batch:** keep existing max 64 mutations and `MaxProjectionBytes` semantics unless separately benchmarked/approved. Coordinator decodes/diffs/coalesces, partitions to per-family **writer-owned** typed staging buffers, and assigns a publication interval `(startSequence,endSequence)`.
3. **Concurrency:** one serialized writer per active physical family; different families may execute in parallel on disjoint derived-state resources. Do not assume namespace ownership implies a separate writer. No family worker may mutate another family's state or share mutable decoded scratch.
4. **Durable prepare:** each affected family applies changes to **unpublished** generation state, persists the family's required files/metadata, and returns a verified readiness token. For engines that cannot stage without making data reader-visible, first implement a safe generation-aware staged path or keep that engine on the current serialized barrier until fixed; do not publish early.
5. **Global commit barrier:** publish **one immutable composite read root** only after all touched families are durable, and the global index source generation can safely advance. Untouched healthy families participate logically at the same publication generation without unnecessary rewrites. Persist coordinator/index intent and activation sufficiently to recover from every power-loss boundary.
6. **Pebble acknowledgment:** advance/clean journal acknowledgment only after the global derived generation is confirmed durable; never acknowledge per-family completion independently.
7. **Failure:** if one family fails, do not expose a hybrid query view. Leave the journal recoverable; reuse, discard or rebuild unpublished outputs deterministically. If already-persisted engines cannot roll back, the recovery protocol must replay idempotently or rebuild the affected family before exposing a new root.
8. **Replay:** source-generation idempotence must be defined for **all** families, including unchanged/no-op groups and tombstones. Prevent generation reuse and ABA of views/file identifiers.
9. **Ownership:** coordinator's reusable 64 KiB blocks and `DecodeInto`/`ProjectInto` scratch are currently synchronous-lifetime. **Never hand borrowed workspace slices to asynchronous family writers.** Choose one: (a) finish all consumers before reset, or (b) copy to per-family owned bounded buffers and document ownership. Benchmark both; prefer simplest correct version. Epoch checks and stale-ref tests are mandatory.
10. **Concurrency limit:** at most one in-flight publication interval initially; fixed/typed per-family worker slots, no unbounded goroutine/queue growth. Avoid per-mutation goroutine launch; favor long-lived workers or bounded tasks if parallelism proves useful.
11. **No premature atomicity claims:** `atomic.Pointer` swap only guarantees one in-process view publication, not multi-file crash atomicity. Specify separately durable prepare/manifest/activation/ack rules.

**Alternative gate:** If a coherent atomic cross-family publication cannot be made correct within Phase 3 without a broad engine redesign, keep the current single apply writer temporarily, deliver other improvements, and report the failing proof/benchmark. Do **not** silently implement independently publishing per-family writers.

## 3. PR 3.0 — Reproducible baseline and instrumentation

Extend existing `internal/bench`, `AGENTS/Bench.MD`, `database/workload_bench_test.go` and engine benchmarks rather than creating a parallel framework.

- Workloads: new imports, 1/16/32/64 mutation commits, repeated metadata-only edits, rating/tag, EXIF, GPS, exact/similarity hash, deletes/reinserts, full projection, sparse/dense ID mixes, index compaction, and concurrent searches. Include realistic embeddings as a **regression workload**, not vector-optimization work.
- Sizes: 10K and 100K routine; 1M local/periodic; 10M behind existing `IMAGEIDX_BENCH_LARGE` opt-in.
- Collect throughput, p50/p95/p99 of commit→indexed lag and query latency, allocations/B/op after warmup, RSS/retained bytes, mmap bytes, journal/canonical/index logical bytes, derived file sizes, and time spent in flush/compaction/rebuild.
- Separate logical metrics (`DB.Stats()`, `store.Stats()`, `index.Stats()`) from actual fsync count, Pebble WAL/flush/compaction metrics, filesystem bytes, and device writes; label unavailable counters as unavailable, never estimated actual I/O.
- Compare Apple M4 Pro baselines when the development environment supports it; CI Linux amd64/arm64 is primarily a correctness gate, not interchangeable performance hardware. Record Go version, OS, architecture, dataset seed, sample count and benchstat confidence.
- Publish the current critical path and % of time/bytes by family. Do not invent baselines or replace prior stored data with numbers from another machine.

## 4. PR 3.1 — Family writers, grouped writes and targeted COW

Inspect: `database/coordinator.go`, `database/coalesce.go`, `database/workspace.go`, `index/batch.go`, `index/mutation.go`, `index/store.go`, `index/capabilities.go`, and per-family flush paths.

- Implement §2's correct per-family prepare + global publication protocol first. Make it testable with deterministic boundary hooks and a serial reference execution path.
- Within each family, group staged records by namespace and physical persistence target using reused typed slices; sort only when required by engine order. Preserve overwrite semantics (last write wins only where documented) and stable ID/result ordering. No hot-path per-record maps.
- Deduplicate genuinely redundant updates before persistence; avoid no-op manifest rewrites and unnecessary touched namespaces. Maintain liveness (`Touch`) and sequence advancement for no-op deltas.
- Keep each engine's existing optimal COW structure: bitmap containers, time/geo immutable generations, radix hash pages/leaves, SimHash bounded segments. Any change requires individual engine benchmarks and crash tests. No whole-dataset copy on small writes.
- Extend coalescing only for families whose implementation proves identical final state and durability for insert→update, update→update, update→revert, delete→reinsert, interleaved events and replay. Keep vector and unproven SimHash cases ordered.
- Per-family batch APIs are optional and benchmark-gated; **do not implement vector batch API**.
- Measure throughput at family counts 1, 2, 4, and all configured families; include contention, CPU overhead, liveness and publication lag. Demonstrate benefit over the serial reference rather than assuming parallel always wins.

## 5. PR 3.2 — Lightweight cost-based search planning

Inspect `search/plan.go`, `search/executor.go`, `search/workspace.go`, existing clauses/ranking, and bitmap/time/geo search APIs. **Maintain current hybrid candidate filtering behavior** as the correctness reference.

- Build a bounded lightweight cost model with estimated selectivity, posting size, operation type and candidate count. Prefer narrower candidate drivers before expensive tests; do not plan exact/similarity hash through general clause combinations.
- Evaluate **B** (write-maintained approximate counters) versus **D** (counters plus selective runtime measurements). Benchmark planning CPU/allocations, accuracy, write overhead and end-to-end queries. Pick the simpler or faster approach with comparable result quality. Derived stats can be rebuilt and must not become an additional schema authority.
- Retain the current hybrid of bitmap SIMD intersections and streaming candidate tests. Avoid intermediate bitmap materialization for tiny sets; retain SIMD for large dense intersections where measured useful.
- Preserve scoring, tie-breaking, pagination, result ordering, ACL filtering and public behavior. Query-planning changes must not change match sets.
- Enforce late canonical materialization: filter/rank/top-K ImageIDs before Pebble metadata retrieval; only fetch full application records when caller requests them. Avoid fetching an entire record as an accidental planner side effect.
- Support caller-owned workspaces and pooled executor paths without introducing a global read lock or additional per-query heap allocations after warmup.
- Use property/differential tests comparing new plan to prior executor for boolean combinations, empty/full/dense/sparse distributions, ACL and cancellation.

## 6. PR 3.3 — One coherent query read root and epoch reclamation

**Target:** predominantly an immutable composite view behind `atomic.Pointer`, not per-engine unrelated latest views. A hybrid implementation is acceptable only if benchmarks show it is measurably better while preserving atomic logical visibility.

- Define a composite root containing coherent family/namespace directory references, one global published source generation and any immutable liveness/routing metadata needed for queries. Do not deep-copy entire index datasets on each publication; copy only bounded root descriptors.
- Query enters an epoch/read pin **before** loading the root, holds the same pinned snapshot for all clauses and result assembly, and releases it on completion, error, panic-safe cleanup and cancellation. No writer locks on normal read path.
- Reuse/extend `storage.ReaderTracker` or an epoch design whose correctness can be demonstrated. An `atomic.Pointer` without safe pin-before-dereference and retirement ordering is insufficient.
- Keep published mmap regions, segments and lookup tables alive until every reader that could have observed them has exited. Never recycle workspace arrays or mutate published data underneath a query.
- Let writers stage and durably commit the next root without waiting for ordinary readers. Old mappings can remain pending reclamation; enforce a bounded backpressure or safe retention policy for pathological long-running readers and document its behavior.
- Validate consistent bitmap/time/geo/hash/liveness results during concurrent ingest, coalescing, compaction, schema restarts and recovery. Define the externally promised snapshot contract; query start sees one fully published generation, not necessarily the newest generation when it ends.
- Race, crash/fault injection, explicit unmap/reclaim, leaked pins and long-held-reader stress tests. Compare atomic-root versus hybrid synchronization on p50/p99 and writer contention before adopting hybrid.

## 7. PR 3.4 — Selective derived-index recovery

- Inventory real independent persistence boundaries: namespace file, grouped family file, unified manifest, journal and supporting metadata. Choose **the smallest independently recoverable unit**, not an arbitrary namespace when shared files make that unsafe.
- Preserve `RecoveryManual` and `RecoveryAutomatic`. Manual mode must identify unhealthy namespaces/physical groups with actionable errors; automatic mode repairs affected units **before** serving queries.
- Rebuild strictly from schema.dat definitions, canonical Pebble application records and a compatible deterministic projector (`ProjectFromRecord`/`ProjectInto` where required); do not treat existing derived indexes as authoritative reconstruction input.
- Stage replacement output separately, validate complete coverage and source generation, sync, atomically publish a new coherent root and only then retire broken files. Do not overwrite healthy units during rebuild.
- Handle partial failures, restart during each phase, missing callbacks, namespace additions, projection-version migrations and sparse SimHash segment corruption. Fail closed if a trustworthy snapshot cannot be constructed.
- Prove targeted rebuild equivalence to full rebuild for each supported engine; measure bytes read/written and downtime with 1 corrupted unit among many healthy units. If shared durability boundaries require a larger rebuild, explain why rather than implementing unsafe partial recovery.

## 8. PR 3.5 — Storage lifecycle and verification

- Inspect compaction/retirement scheduling for bitmap/time/geo/radix/SimHash; tune only confirmed hotspots. Retired files must remain pinned until epoch reclamation permits deletion/unmap.
- Include bursty write workloads, idle periods, large sparse ID gaps, worst-case tombstones, repeated updates of same ID, journal replay, index rebuild and corruption.
- Separate engine-level win from end-to-end win; remove optimizations that increase complexity without material benefits. Avoid generic allocators or a universal index family abstraction.
- Keep vector graph and vector batch redesign entirely out of scope (Phase 4). Existing vector read/write behavior must remain correct under new global snapshot and family-writer protocol.

## 9. Quantitative acceptance and stop rules

Use representative matched baselines, sufficient iterations, `benchstat`, profiling and hardware-identical comparisons:

| Metric | Required acceptance |
|---|---|
| Index write throughput | **≥15% improvement** on representative workloads; report cases where it is not achieved. |
| Total logical write amplification | **≥15% reduction** across canonical + journal + derived indexes on representative update mixes. |
| Search p50 and p99 | No more than **5% regression**, especially during concurrent ingestion. |
| Steady-state allocations | No unexplained hot-path increase; prefer zero-allocation warmed planner/decoder/staging paths. |
| Retained heap and related index memory | ≤10% increase unless supported by a clear, measured payoff. |
| Selective recovery | Correct smaller-unit rebuild, with reported time/bytes relative to full rebuild. |
| Durable correctness | **Zero** tolerated losses/partial publication/false success/corrupt mixed-generation views. |

Targets are gates to investigate and optimize, **not permission to weaken correctness**. If targets conflict (e.g., parallel family staging improves throughput but increases logical writes), report tradeoffs and do not claim completion without clear evidence or an explicit documented exception. CI variability alone cannot validate 5% thresholds.

## 10. Mandatory validation

Execute relevant commands and record actual outcomes (do not claim success without running):

```bash
go build ./...
go test -count=1 ./...
go test -race -count=1 ./...
go vet ./...
sigmap validate
```

Additionally:

- Fuzz/property tests for query plan equivalence, generation pinning, journal replay and mixed family mutations.
- Process-crash tests at every family prepare/sync/manifest publication/global-root publication/Pebble acknowledgment/cleanup boundary; restart must recover without partial visibility.
- Fault injection during selective rebuild activation and segment retirement; verify untouched healthy indexes byte-for-byte where applicable.
- `go test -gcflags='-m=2'` for benchmark/test hot-path escape analysis, plus profiles and `benchstat`; document unavoidable escapes.
- Benchmarks on Linux amd64/arm64 and Apple Silicon when available. Use `IMAGEIDX_BENCH_LARGE` only for opt-in 10M tests.

## 11. Deliverables and definition of done

Submit the work as reviewable PRs in order: **3.0 baselines → 3.1 family writers and publication → 3.2 planner → 3.3 coherent reader snapshots → 3.4 recovery → 3.5 tuning**. Note the logical dependency: the **composite view and safe epoch protocol required by 3.1 must be designed first** and can be implemented as a minimal foundation within 3.1; 3.3 then generalizes it to all search paths. Reorder 3.1/3.3 if necessary to avoid an intermediate state with unsafe multi-family visibility.

Each PR must include scope, exact benchmarks against baseline, tests, crash seams, invariants, memory-ownership statement and rollback/recovery implications. Do not mark a PR complete because unit tests pass while publication atomicity is unproven.

**Phase 3 is complete only when** all queries use coherent published snapshots, epoch reclamation safely retires storage, family-specific writers cannot expose partial generations, derived indexes can selectively recover at safe boundaries, and the measured targets are satisfied or explicitly documented as exceptions. Do not start Phase 4 vector batching in this phase.
