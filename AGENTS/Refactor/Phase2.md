# ImageIdx — Phase 2 Codex Implementation Plan (updated after PR #24)

**Repository:** `evanoberholster/imageIdx`
**Reviewed PR:** #24, branch `refactor-index-design`, commit `e0c9cece8f32595dd7e9b7938d9ba1a12a602fcd`
**Status at review:** PR open/mergeable; latest GitHub Actions run was **in progress**. The PR reports prior local validation; do not assume the new head's CI has completed.
**Priority:** Correctness → measurable total write reduction → low memory/allocations → implementation simplicity.

## 0. Current state: do not reimplement Phase 1

PR #24 now implements: KV namespace bitmap, 64 KiB reusable coordinator projection blocks, fixed queued descriptors, optional `DecodeInto`/`DecodeScratch`, typed deterministic projection sort-and-merge, a one-association fast path, limited repeated-entity coalescing, and logical writer statistics. The follow-up also added insert→update/reopen with unchanged and changed associations, randomized sequential-versus-coalesced tests, crash-boundary tests, block-boundary/stale-reference tests, and Linux AMD64/ARM64 CI. **Do not reopen the prior suspected insert→update bug as an established defect**: `applyCoalescedGroups` uses `batch.Add(lastEvent.after)` for insert→update; delete/reinsert is conservatively excluded. Do not duplicate these tests or mechanisms without identifying an actual uncovered case.

Observed PR-reported results: bitmap membership ≈0.25 ns/op (versus map 5.6–6.1 ns/op); one changed association 45.7 ns, four associations 146.6 ns, 16 associations 550 ns, and 256 associations 30.8 µs, all 0 allocation in the warmed diff benchmark. End-to-end commit/index throughput is still within approximately ±4% of baseline. `Stats()` measures logical/application-level counters, **not actual fsyncs or device writes**. Pebble `SetDeferred` was benchmarked, not adopted. These numbers are reported by the PR, not independently reproduced here.

**First action:** Rebase after PR #24 is merged or branch from its exact reviewed commit if it remains open. Do not silently stack Phase 2 on an outdated head. Confirm CI completion for the final head and record the SHA. Phase 2 must start with a performance and recovery baseline from that exact SHA.

## 1. Phase 2 goals and scope

1. Reduce **total canonical + journal + secondary-index logical write bytes**, index write operations, and ultimately physical write amplification for frequent small metadata updates.
2. Introduce a durable, typed projection-delta journal, owned by `database`, without adding application-side generic reflection or maps.
3. Establish one canonical, deterministic mutation representation shared by ingestion, replay, coalescing and index application.
4. Make journal and projection compatibility/version handling explicit and fail closed when incompatible.
5. Remove the dense external-ID dependency of SimHash using a benchmark-gated implementation and rebuildable format.
6. Maintain one serialized coordinator writer, synchronous `index.Apply` lifetime, stable reader semantics, and existing recovery correctness.
7. Keep allocation steady-state near zero with caller-/writer-owned scratch; measure actual allocations rather than claiming universal zero allocations.

**Pre-V1 compatibility policy:** ImageIdx has not shipped; **old experimental on-disk formats need not remain readable**. Codex may replace schema/journal/index formats cleanly, remove legacy migration branches and update fixtures. Do **not** weaken crash safety or discard a test of fresh-create → commit → process crash → recover/rebuild → reopen. Document that older development databases must be deleted/rebuilt, and fail explicitly on incompatible format rather than misreading bytes. Do not carry v1/v2 dual-read machinery unless a documented internal test/upgrade requirement truly needs it.

**Out of scope:** cross-engine distributed transactions, full MVCC read-view architecture, dynamic runtime namespace registration, changing Pebble, custom general LSM engine, new query planner, new vector graph algorithm, generic ORM, runtime maps/reflection, immediate async index publication. Keep `schema.dat` the authoritative schema and allocator; Pebble stores only KV records and journal/coordinator operational data.

## 2. PR 2.0 — Baseline and instrumentation (merge gate)

Inspect `AGENTS.md`, `AGENTS/Bench.MD`, `internal/bench`, `database/README.md`, existing benchmarks, index recovery design, and current `store.Stats()`/`index.Stats()`/`DB.Stats()` before editing code. Reuse existing benchmark infrastructure and `IMAGEIDX_BENCH_LARGE` for opt-in 10M-image tests.

Record measurements for 1/16/32/64-image batches and update-only workloads (one rating, favorite, tag, timestamp, GPS, hash, classification, large embedding). Distinguish:
- Logical canonical-value bytes written, including full projection prefix and application bytes;
- Logical journal bytes and count;
- Logical index association changes and index generation/manifest activity;
- Pebble WAL/flush/compaction statistics if available, with their precise definitions;
- OS/device bytes and syncs **only when genuinely measured** by platform instrumentation; otherwise mark unavailable.

Compare with repeatable `benchstat` methodology on a fixed device. Treat Linux AMD64/ARM64 CI as correctness targets; report architecture-specific performance measurements separately. Run concurrent search+ingest and p50/p95/p99 query latency tests; do not conflate warm/cold coordinator allocation.

**Acceptance:** publish a baseline table and explicit metric definitions. Do not invent physical-write counts from application-level counters.

## 3. PR 2.1 — Define ownership and mutation contract BEFORE coding formats

**Decision: the `database` package owns the on-disk delta protocol; the application owns complete projection encoding/decoding.** `database.Decoder` and optional `DecodeInto` remain the complete `index.Entry` reconstruction path. The database computes before/after logical typed associations and encodes the minimal delta. Index engines receive concrete typed operations; do not require the application to maintain a separate opaque delta format.

Define `ProjectionMutation` / equivalent internal typed structs in a documented design, using slices with bounded workspace reuse rather than per-operation interface values. Distinguish:
- Entity existence/liveness changes from auxiliary record changes;
- `Set`/`Remove`/`Replace` association semantics;
- ID-authoritative replaces (e.g. time/vector) versus value-addressed deletes (geo/hash/bitmap);
- Entire-entity delete, auxiliary-record removal, tombstone and reinsert;
- No-index-change mutation that still advances journal sequence/generation.

**Precondition:** For each family define the exact old data necessary to remove or replace an association. Avoid redundant old blobs for ID-authoritative operations only where engine semantics prove old-value omission safe. A bitmap/geo/hash replacement must not leave stale old associations. Define duplicate/set semantics and deterministic association canonicalization; validate namespaces, entity ownership, ID range, and vector element/dimension constraints *before* durable publication.

Do not embed `database` scratch `dataRef` in any journal record. Durable records must be self-contained and independent of block epochs/lifetimes.

## 4. PR 2.2 — Journal v2 and canonical storage strategy

### 4.1 New compact durable format

Since there is no released format to support, use a **single clean journal v2 format** for all new databases; update golden fixtures and remove obsolete compatibility paths. Define a versioned checksum-protected envelope, explicit lengths/bounds, entity/ValueID, op/liveness flags, and typed namespace operations. Prefer fixed-width big-endian keys as already used by `store`; choose one deterministic value encoding per family. Specify the entire wire layout and byte-order in docs *before implementation*.

Suggested *logical* envelope (not mandatory bytes):

```
Version:u8 | Operation:u8 | Entity:u8 | ValueID:u64
DeltaCount:u16 | DeltaEntries... | CRC32C:u32
DeltaEntry := Namespace:u16 | Family:u8 | Op:u8 |
              OldLen:bounded-varint | NewLen:bounded-varint | OldValue | NewValue
```

Check that an explicit logical before/after entity state is representable. Use strict parsing, caps on lengths/count, checksum, no trailing bytes, and overflow-safe decoding. A simple, concrete, allocation-free decoder with caller scratch is preferable to a generic codec framework. Do not store redundant families/fields if namespace definitions unambiguously determine them, but reject a family mismatch where encoded.

### 4.2 Canonical write amplification (critical)

Today the authoritative Pebble canonical value includes a **full projection** plus application bytes. Shrinking the journal alone may not save enough *total* bytes. Evaluate **two concrete designs**, with real fixtures and no invented gains:

**A — Journal-only deltas:** canonical value retains full projection, journal contains old/new delta. Simplest and safest; can still duplicate the after projection logically.

**B — Canonical-layout improvement:** application record is authoritative; a compact current projection or projection summary is stored only if required to compute future removals/recovery. Design a same-batch update strategy with a minimal canonical snapshot and journal delta. If application projection can be reconstructed from canonical bytes, avoid storing duplicate full-projection bytes; if it cannot, retain the minimum independent canonical representation necessary for correct rebuild and future diff. Do **not** assume the opaque application bytes can be reprojected without a new explicit application projection callback.

Before choosing B, specify how `Commit` obtains old canonical state, how updates remain atomic in one Pebble transaction, and how startup recovery reconstructs every index after journal cleanup. Canonical source must remain sufficient for full rebuild. The index must never be the sole authoritative copy of data required for its own reconstruction.

**Adoption gate:** report journal bytes AND combined logical canonical+journal+index bytes for typical small-edit workloads. Suggested targets (subject to baseline): ≥40% journal-byte reduction and ≥15% combined logical-byte reduction, with ≤5% median throughput or p99 latency regression in stable repeated comparisons and no new steady-state allocation. **Do not force B** if complexity outweighs measured improvements: record the result and retain A if demonstrably better overall.

### 4.3 Coalescing and publication

The current Phase 1 coordinator supports up to 64 mutations with typed sort/merge and conservative family fallbacks. Reuse its workspace and coalescer; do not introduce a second competing pipeline. A delta can be normalized to typed operations before coalescing. Preserve the **initial state + final state** of each `(EntityKind, ValueID)` group and the entire contiguous acknowledged generation range. Enforce per-family safety; initially disable coalescing for unsupported SimHash/vector and delete→reinsert cases until proofs/tests exist. No-op deltas still advance generation.

Keep the coordinator→`index.ApplyWithGenerationAll` path **synchronous**. The coordinator blocks must not reset until every consumer has finished copying durable/retained data. If async publication is proposed, **stop and produce a separate lifetime/read-view design instead of changing it here**.

Crash cases: before canonical commit; after commit before journal scan; after durable intent; during each engine write; after index durability; after ack; journal cleanup; on reopening. Acknowledged source generation must never outrun recoverable index state.

## 5. PR 2.3 — Projection semantic versioning and canonical key policy

Projection versions belong in **`schema.dat`** per namespace, separate from engine-specific `NamespaceDef.Config` (some engines decode exact fixed config lengths). Use a new explicit outer schema format version or additive outer field preserving registration order semantics. `schema.Definitions.defs` ordering and namespace identity logic must remain correct; there is no requirement to preserve old golden bytes before V1, but new golden fixtures must pin the new representation.

Store: format version; per-index-namespace `ProjectionVersion:uint16` starting at 1; immutable entity/kind identity; projection version fingerprint used for fresh index compatibility/rebuild. Projection changes require **close/reopen + deterministic rebuild** of affected physical index groups, not runtime hot-swaps. The application must provide an explicit projection-generation callback capable of using the canonical authoritative record to produce the target version. `Decoder` of stored old projection bytes alone cannot invent new projection semantics. If no such callback exists, fail closed with a descriptive error. Integrate with existing PREPARED/ACTIVATE/rebuild machinery only after analyzing current recovery transitions; avoid rebuilding every healthy physical group where dependency-aware rebuild is safe, and retain whole-store rebuild fallback.

**Canonical keys:** keep 12-byte keys. Because existing `store.Key.Flags` and journal flags use 0 and nonzero validation, **do not simply reinterpret Flags as a record version** during this PR. If canonical record codec versioning is necessary, design it as a separate explicit key/value-format decision, with fresh-format fixtures, typed lookup invariants, and journal namespace-specific rules. No silent ambiguity between flags, tombstone, and codec version. The request is projection-semantic versioning, not a general application MessagePack schema migration.

## 6. PR 2.4 — Sparse external ImageID support for SimHash

Current `index/batch.go::flushSimNamespace` uses `idx.Len()+1` dense contiguous append behavior. This conflicts with entity ID gaps and out-of-order images. **Preferred outcome: direct nonzero uint64 external IDs stored alongside hashes**, benchmarked against an optional dense-internal-ordinal mapping prototype. Do not introduce the mapping unless it clearly wins an important workload (suggested ≥20% throughput/latency win without disproportionate memory/write amplification).

Implement semantics:
- Insert new ID: add; insert existing ID: idempotent upsert/replace (validate chosen hash record equality behavior).
- Update existing ID: replace pHash/PDQ record without retaining stale searchable values.
- Delete: inactive result; reinsert: active with new values. Respect liveness and tombstones.
- Support gaps, out-of-order and IDs above `math.MaxUint32` up to nonzero `uint64` (detect overflow in any internal integer conversions).
- Fixed, documented deterministic result ordering for identical Hamming distances; preserve `[]Result{ImageID, Distance:uint8}` public shape and existing pHash/PDQ distance behavior.
- Batch ≤64 for ingestion/check; avoid per-record allocation on warmed typed paths.
- Recovery replays or rebuilds without `Len()+1` checks. Replace prior engine files via verified staged rebuild/activation, not unsafe in-place edits. Since pre-V1, old format may explicitly fail and require rebuild; do not manufacture an elaborate legacy converter.

Benchmark 1/16/64 hash inserts, random sparse IDs, ascending IDs, high IDs, duplicate upserts, deletes/reinserts, queries at several Hamming thresholds, cold mmap load, resident memory, physical/logical write amplification, and concurrent readers. Add property tests comparing a simple reference scan to optimized searches. Keep exact hash (radixhash) semantics unchanged.

## 7. PR 2.5 — Per-family idempotence and batch reduction

Define a private, compile-time **verified capability table** or concrete dispatch, not dynamic maps/reflection or a user-settable family flag. For each engine specify: `BatchSafe`, `CoalesceSafe`, `IdempotentReplay`, `DeleteByID`, `RequiresOldAssociation`, `PersistentGeneration`. Capabilities must reflect tests, not aspirations. A **publication unit** is the contiguous journal range durably acknowledged together; an engine transaction is a different unit.

Do not call an operation idempotent merely because applying it twice seems harmless in memory. Prove repeated application and crash recovery produce the same searchable state and persistent watermarks. Permit conservative ordered sub-batches when any family is unsafe. Optimize redundant liveness writes and avoid unnecessary per-namespace commits only after equivalence and before/after counters demonstrate benefits.

Vector batch API: **benchmark and design only** unless it can be implemented without expanding this PR into a graph/storage redesign. Preserve vector index's existing ID width constraints until an explicit subsequent change.

## 8. Measurement and testing gates

Use existing `internal/bench`, benchmark scripts, `AGENTS/Bench.MD`, and `search/PERFORMANCE.md`; do not create a parallel benchmark ecosystem. Evaluate the full matrix on Linux AMD64 and ARM64 correctness CI; performance baseline uses the same pinned machine/Go version/filesystem for base versus candidate (Apple M4 Pro results may be tracked as additional local baselines if available).

Run at minimum:

```
go build ./...
go test -count=1 ./...
go test -race -count=1 ./...
go vet ./...
sigmap validate
```

Profile changed paths with `-benchmem`, repeated `benchstat`, `pprof`, and escape analysis (`go build -gcflags=-m=2` for source and `go test -gcflags=-m=2` for tests). Add deterministic fault injection for new serialization publication boundaries. For fresh DBs verify creation, reopen, corruption reporting, schema namespace additions on restart, per-entity ID isolation, liveness, full rebuild from canonical, and mixed update patterns. Do not claim `DB.Stats()` returns device writes.

Report per workload: ns/op, B/op, allocs/op, images/s, canonical/journal/index logical bytes, engine operation counts, WAL/compaction stats when measurable, p99 concurrent query latency, peak and steady retained workspace memory, recovery correctness. Record the actual baseline and candidate commit SHAs.

### Definition of Done

- Phase 1 final head CI passes and is used as the explicit baseline.
- New durable formats are fully specified, checksummed, bounded, golden-tested, and fail closed on incompatibility; document that older development DBs must be recreated/rebuilt.
- Journal deltas preserve all removals and entity liveness, including deletes/reinserts, with no-op generation progression.
- Canonical data still supports deterministic complete index rebuild after journal cleanup.
- No index retains coordinator workspace memory beyond synchronous apply.
- SimHash supports sparse, out-of-order, nonzero 64-bit external ImageIDs without dense `Len()+1` constraints.
- Existing `schema.dat` namespace addition and ID allocation remain crash-safe.
- No meaningful unexplained memory, p99 search, or ingest regressions; actual measured amplification improvement is documented (or a proposal is rejected transparently for missing its gate).
- Tests, race, vet, sigmap and crash/restart matrix pass; the PR description includes before/after benchmark tables and remaining limitations.

## 9. Suggested delivery order

1. **2.0** Baseline + instrumentation and freeze mutation-format design (standalone PR).
2. **2.1–2.2** Typed delta model and durable journal v2, with canonical-layout A/B prototype and explicit adoption gate (dedicated PRs if needed).
3. **2.3** Per-namespace projection version + explicit rebuild/migration contract.
4. **2.4** Sparse SimHash uint64 IDs and verified rebuild.
5. **2.5** Proven per-family batching/idempotence optimizations and final profiling.

Each PR must pass focused tests before the next begins. **Do not implement all these changes at once**. When measurements contradict an assumption, favor the simpler correct implementation and record the rationale rather than adding complexity merely to satisfy a speculative target.
