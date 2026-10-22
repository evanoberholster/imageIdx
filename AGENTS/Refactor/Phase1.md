# ImageIdx Phase 1 — Codex Implementation Instructions

**Repository:** `evanoberholster/imageIdx`
**Scope:** `store`, `schema`, `database`, `index`, existing benchmark infrastructure; `search` for regression tests only.
**Priority:** correctness → durability/recovery → bounded memory → performance → simplicity.
**Compatibility:** **no changes to existing disk formats**, persisted namespace order, journal encoding, public API behavior, or durability guarantees.

## 0. Mandatory repository-first audit

Before editing code, read `AGENTS.md`, `.github/copilot-instructions.md`, `AGENTS/Bench.MD`, existing `AGENTS/Refactor/Phase1.md`, and the actual relevant package implementations/tests. Follow repository-specific validation requirements, including `sigmap validate` when configured. Do not implement features already present. Record the baseline commit SHA and list the exact measured hot paths. If code differs from this document, prefer inspected source and document the discrepancy.

Known current facts to preserve:

- `store.Store` uses a `map[Namespace]struct{}` **only for KV namespace membership**; `schema.Definitions` separately uses name/ID maps and retains **registration-order** `defs` for binary serialization. `index.Store` already has dense slice-based routing. These are distinct layers.
- `schema.Definitions.MarshalBinary`, golden fixtures, and `database/migration.go` depend on registration order. **Do not sort, reorder, or change `Definitions.defs` or persisted `schema.dat` bytes.**
- Namespace map lookup is already zero-allocation; the selected bitmap replacement is a **memory-layout decision**, not a claim of allocation improvements per lookup.
- `database.Open` currently caps `BatchSize` at **64**. Preserve that maximum. Benchmark batch sizes 1, 16, 32, 64; 128/256 are not in scope.
- `database.Commit` rejects duplicate `(EntityKind, ValueID)` within a single commit. Preserve this API rule. The journal coordinator currently ends a batch before a repeated entity/ID. Phase 1 will change **coordinator application grouping** in a controlled manner, not canonical commit semantics.
- `database/projection.go` already skips unchanged associations but currently uses O(N×M) comparisons and allocates on some differences. Replace the implementation; do not invent a second redundant mechanism.
- The coordinator's public `Decoder func([]byte) (index.Entry, error)` must remain supported. The repository does not currently contain an application-specific MessagePack/EXIF codec; use repo-grounded projection fixtures rather than presuming one.
- `index/batch.go` already uses typed stage slices, resets their lengths, sorts by namespace, and reuses its time batch. Focus on measurable remaining allocations and stale pointer retention.
- `search.Workspace` is caller-owned, while `search.Executor` pools workspaces. Do not redesign either in Phase 1; include concurrent search tests.
- Build on `internal/bench`, `AGENTS/Bench.MD`, current benchmarks and `search/PERFORMANCE.md` rather than introducing a parallel benchmark framework.

## 1. Confirmed architectural decisions

1. KV membership in `store.Store`: **replace the map with a fixed 65,536-bit bitmap** (1,024 × `uint64`, 8 KiB). This is explicit even if the existing map benchmarks competitively. Benchmark and report startup/retained-memory tradeoffs honestly; don't replace maps inside `schema.Definitions` in this phase.
2. Schema definitions: preserve persisted registration order. Index routing: reuse its existing allocation-free slice routes. Optionally resolve frequently accessed typed handles at initialization **only when a measured benefit exists**.
3. Memory: concrete fixed-width struct values/arrays for mutation descriptors, reusable typed slice backing arrays for staging, stable 64 KiB byte blocks for variable-length projections. Prefer stack-eligible locals for short-lived fixed-size data; accept reusable heap-resident storage for long-lived workspaces.
4. Ownership: database coordinator owns its own buffers; index writer owns its stage; search callers/executor follow their existing workspace model. No global generic arena, global `sync.Pool`, or shared mutable scratch.
5. Batch bounds: max 64 journal mutations; byte budget is **sum of copied before + after projection payload bytes**, default 1 MiB, excluding descriptors and block slack. A valid oversized single entry is processed alone, not rejected. The configured count and byte limits cannot silently drop the entry that crosses a limit.
6. Blocks: fixed 64 KiB reusable blocks; retain up to roughly 1 MiB of ordinary block capacity by default; represent oversized allocations in the same addressable directory and retain at most one bounded, reusable oversized buffer (configurable retention ceiling); release exceptionally large buffers after use. Do not assume logical byte budget equals physical block capacity: account for tail fragmentation separately.
7. Diffs: typed deterministic `slices.SortFunc` plus linear merge; compare complete semantic associations and preserve set/uniqueness rules. Diff **during journal application**, not before persistence. Keep the existing journal format.
8. Coalescing: **within one bounded publication group**, combine nonadjacent/interleaved journal mutations affecting the same entity/ID when initial→final-state equivalence and engine preconditions are proven; preserve the original contiguous generation range. Never cross acknowledgments, crash-recovery, schema or publication boundaries. Use bounded slices (at most 64) and linear scans; no map accumulator.
9. Pebble: preserve synchronous canonical durability. Benchmark conventional reusable-scratch `Batch.Set` against v2.1.5 deferred encoding, adopting direct encoding **per record kind only when measured superior**. No record codecs should be invented in the generic database.
10. Backward compatibility: keep current error behavior, input ordering, index mutation semantics, persisted `schema.dat`, Pebble key shape, journal bytes, recovery markers and index formats.
11. Verification: extend benchmark matrix, measure allocs/bytes/latencies/writes, compare via `benchstat`, review compiler escape diagnostics, run all applicable build/test/race/crash checks and repo-specific validation.

## 2. PR 1 — Inventory, baseline and instrumentation

**Deliverables**

- Inventory existing benchmarks in `internal/bench`, `database`, `store`, `schema`, `index`, and `search`. Add only missing focused cases.
- Baseline: namespace membership and schema lookup (separately), 12-byte keys, journal batches, projection diff (equal/one field changed/many changed), index stage/flush by family, canonical commits, journal application, concurrent searches, reopen/recovery.
- Metadata distributions: deterministic existing generator, common cameras/lenses, clustered timestamps/GPS, sparse tags, variable-length classifications/hash/vector projection fixtures, and repeated updates. Use 10k/100k/1M tiers, with 10M opt-in via the existing large benchmark flag.
- Add cheap **disabled-by-default** typed counters/instrumentation to *physical* Pebble commits and index sync/checkpoint/manifest events. The canonical record and journal are written in **one Pebble batch**, not two separate commits. Distinguish logical bytes from actual measured filesystem writes and report limitations.
- Bench 1/16/32/64 entries. Capture `ns/op`, `B/op`, `allocs/op`, p50/p95/p99 where applicable, GC, retained bytes, logical/index/journal bytes, and measured I/O if available.
- Record the baseline commit, Go version, platform, filesystem, durability options and reproducible commands. Do not call estimated logical bytes “physical write amplification.”

**Gate:** baseline exists before optimization PRs; no behavior change.

## 3. PR 2 — KV namespace bitmap (without schema reordering)

Replace only `store.Store.namespaces map[schema.Namespace]struct{}` and associated membership checks with an internal fixed bitmap:

```go
const kvNamespaceWords = 1 << 10 // 65,536 namespace IDs / 64 bits
type kvNamespaceBits struct { words [kvNamespaceWords]uint64 }
func (b *kvNamespaceBits) add(ns schema.Namespace) { n := uint16(ns); b.words[n>>6] |= uint64(1) << (n&63) }
func (b *kvNamespaceBits) contains(ns schema.Namespace) bool {
    n := uint16(ns)
    return (b.words[n>>6] & (uint64(1) << (n&63))) != 0
}
```

- Construct in `store.Open` only after verifying definitions are valid and KV kind. Internal `add` is unexported; initialization finishes before publishing Store, making the bitmap immutable for its lifetime. Keep legacy open path compatibility and validation.
- **Do not modify `schema.Definitions.byID`, `.byName`, `.defs`, `MarshalBinary`, `UnmarshalBinary`, migration positional checks or index route tables.**
- Benchmark existing map vs bitmap on realistic numbers of KV namespaces and report lookup and retained memory. Document that bitmap costs 8 KiB per Store and may increase retained memory for small schemas.
- Verify boundaries (`0`, `0xFFF0`, `0xFFFF` as allowed/forbidden by their actual layer), absent namespaces, duplicate definition errors, legacy modes, restart extension and golden bytes.
- Do not introduce an exported mutable bitmap API or pretend a raw handle remains valid after store close. Existing lifecycle contracts apply.

**Gate:** zero allocs per membership lookup; exact same validation and persistence behavior.

## 4. PR 3 — Coordinator workspace and journal copy consolidation

### 4.1 Concrete reusable types

Use package-private types; names illustrative. Avoid one heap object per queued journal record:

```go
const projectionBlockSize = 64 << 10
const defaultRetainedOrdinaryBlocks = 16

type dataRef struct {
    slot   uint32 // index into a stable per-batch block directory; includes oversized slots
    offset uint32
    length uint32
    epoch  uint32 // debug/validation of reset lifetime
    valid  bool   // absence != empty present payload
}

type projectionBlock struct {
    bytes []byte // stable backing store for this batch; ordinary cap = 64 KiB
    used  uint32
}

type queuedMutation struct {
    sequence uint64
    entity   schema.EntityKind
    id       schema.ValueID
    before   dataRef
    after    dataRef
    deleted  bool // preserve actual journal state
}
```

Choose padding/order with `unsafe.Sizeof` measurements but **do not** introduce unsafe decoding or require packed structs. An epoch is valid only within the workspace lifetime; the workspace must not expose refs that can outlive it. If a simpler tagged representation is more correct/efficient, use it and document the choice.

### 4.2 Buffer rules

- `ProjectionBlock` backing storage must be stable until all staged reads finish. Append into existing blocks without reallocating their storage. Use stable directory slots and offsets so growing the directory doesn't invalidate byte references.
- Copy borrowed Pebble iterator data **once** into these blocks before iterator advancement or callback return. This optimization reduces **heap allocations**, not bytes copied from Pebble.
- Do not split an ordinary payload across blocks; put it in the next available block, tracking tail waste.
- A payload >64 KiB uses a dedicated oversized directory slot. Retain at most one reusable oversized buffer within a bounded capacity; oversized spikes above the ceiling should not permanently inflate retention. Avoid repeated allocation for common moderately oversized payloads.
- Physical block budget and logical byte budget are different. Enforce `MaxProjectionBytes` against `len(before)+len(after)` for grouping and cap retained physical block memory separately. A single oversized mutation remains legal.
- Reset only after the index API has copied or finished using all referenced mutable storage, including on errors. Preserve references needed by recovery. No published mmap/index view may reference coordinator storage.
- Clear pointer-bearing and slice-bearing entries upon reset (`clear(...)` where appropriate), including `entries[:0]` and index staging arrays, rather than only reslicing and retaining obsolete buffers indefinitely. Do not clear memory still owned by an engine; use explicit ownership transfer/copy.
- Prefer `[64]queuedMutation` in the coordinator and reusable typed scratch; support existing smaller batch counts. Do not silently permit >64.

### 4.3 Decoder compatibility and optional allocation-conscious API

Keep the existing public type working unchanged:

```go
type Decoder func([]byte) (index.Entry, error)
```

Add an **optional, opt-in** workspace-aware decoder interface/function form (choose the narrowest nonbreaking integration with existing `database.Options`), conceptually:

```go
// Illustration only: implement in terms of actual existing Entry types.
type DecodeIntoFunc func(encoded []byte, dst *index.Entry, scratch *DecodeScratch) error
```

- Use the new form only when explicitly configured; do not require applications to rewrite their decoder.
- Define scratch ownership: decoded value views borrowing `encoded` are only valid through coordinator processing; indexing must copy what it retains. Expose no callback result that silently outlives borrowed bytes.
- Review `bitmapidx.Value.Bytes` and vector/hash slices for aliasing. Add tests where a decoder deliberately aliases input bytes. Compare results before and after workspace reset. Ensure no stale byte alias can corrupt an indexed value.
- If the existing `index.Entry` contains nested pointer-bearing slices, reuse them carefully and clear them on reset; don't implement naive shallow copying across ownership boundaries.
- Avoid reflection, generalized codecs, invented MessagePack/EXIF schema in core packages.

**Gate:** no per-journal-entry `append([]byte(nil), ...)` allocation in warmed-up normal cases, bounded memory retention, same journal errors/recovery behavior, `go test -race` and aliasing tests.

## 5. PR 4 — Replace projection diff with typed sort-and-merge

Replace `database/projection.go`'s current O(N×M) `changed` implementation **while preserving its existing behavior of suppressing unchanged updates**. Only apply the diff in the existing update path; insert and logical-delete pathways remain separate.

- Sort independent before/after copies into reusable scratch unless exclusive ownership permits in-place sorting. Do not mutate user-owned slices, journal bytes or decoder shared memory unexpectedly.
- Use concrete typed `slices.SortFunc` comparators per existing fields: Times, Geos, Sims, Hashes, Vectors, Bitmaps (BSI is within bitmap kinds, not an independent top-level family). Compare complete typed values, including namespaces, vector element type and actual element slices; use deterministic total ordering and byte-wise comparison where needed. Respect float semantics (`NaN`, signed zero, etc.) consistently with the existing equality contract and documented index behavior.
- For each family, sort then linear merge with **explicit exhausted-side checks**. Never read `before[i]` or `after[j]` after exhaustion. Output only removed and added associations.
- Define duplicate semantics: set-valued bitmap associations do not generate false removals for duplicate identical values. Treat conflicts in single-valued namespaces according to current validation. Do not silently suppress a legitimately distinct association.
- Order the resulting changes in a way compatible with existing `index.Change` reconciliation and engine id-authoritative updates.
- Preserve `changedProjection`'s externally observed behavior; ensure no-op updates remain skipped in `database.apply`.
- Use reusable typed slices for sorting/merge output; no allocation after warmup where adequate capacities exist. Prefer deterministic `slices.SortFunc` first; specialized algorithms require measured evidence.
- Test: empty/one side exhausted, duplicate before/after, same namespace different value, many tags, move geo/time, changes across multiple families, hashes, vector payloads, malformed source, identical projections; differential tests against old reference semantics where valid.
- Bench 1, 4, 8, 16, 64, 256 associations per projection (this is **not** the journal batch-size limit), with identical/one-changed/all-changed scenarios. Report sort cost, allocation, number of staged mutations, and avoided index writes.

**Gate:** equivalent indexed results and strictly no unchanged association writes; no allocation regression for common warmed-up cases.

## 6. PR 5 — Safe nonadjacent same-entity coalescing

This PR changes coordinator application grouping while preserving canonical `database.Commit` semantics (including rejecting duplicate entity/ID **within the same Commit call**) and persisted journal records.

### 6.1 Model

- Read a contiguous sequence interval `(acknowledged, last]` of at most 64 journal entries, bounded by the configured logical byte budget and recovery/publication constraints.
- Detect repeated `(EntityKind, ValueID)` in this interval with a bounded linear scan of a fixed/reusable slice; **no map**. It is allowed for repeats to be interleaved with other entities.
- For each entity/ID group, use the **before-state of its first event** and **after-state/deleted flag of its final event**. Intermediate transitions may be elided **only if** the resulting index effects are equivalent to sequential application and all engine preconditions remain satisfied.
- Keep a deterministic first-seen group ordering where required. The final mutation must be converted into at most one safe existing `index.Batch.Add`, `.Update`, or `.Delete` operation per entity/ID, or fall back to ordered sub-batches if this cannot be proven safe.
- Explicitly model existence/liveness (absent, live, tombstoned) and distinguish *auxiliary namespace deletion* from *whole-entity deletion*. `Add→Delete`, `Delete→Add`, `Update→Delete`, `Delete→Delete`, `A→B→A` need dedicated tests. An index no-op may still advance the acknowledged generation after a durable completeness barrier.
- A batch comprising coalesced events must be acknowledged at the **last original journal sequence**, and its durable `SourceGeneration` must cover **all sequences**. Never invent, skip, reorder or lose generation numbers.
- Never coalesce across schema transitions, previously acknowledged generation boundaries, a pending recovery marker, a partially applied publication, or a user-visible `WaitIndexed` guarantee already satisfied at an intermediate generation. Since `WaitIndexed` means **indexed at least through N**, not necessarily a separately observable intermediate snapshot, document precisely why an intermediate state may be skipped.

### 6.2 Engine restrictions

- Validate engine semantics before enabling coalescing for that family. SimHash's dense append/updates, exact hash duplicate semantics, vector graph behavior, timestamp/geo replacement rules, bitmap set semantics and global liveness all need explicit treatment.
- **Do not use a generic blind 'last write wins' rule on engine operations.** Coalesce at the entity projection level *before* the unified `index.Batch` stages by family.
- If a repeated group cannot be represented safely as one entity-level operation (including unsupported engine preconditions), split into sequential application groups at a safe publication boundary rather than forcing coalescing.
- The existing `index.Batch` flushes by kind; do not feed it multiple ordered same-entity operations expecting chronological execution.
- The journal remains replayable without stored coalescing metadata. Crash recovery must produce the same final state whether a group was coalesced or applied sequentially.

### 6.3 Validation

- Property-based/reference tests: replay every supported operation pattern sequentially into a baseline index and compare against coalesced ingestion across all relevant index queries/liveness; also verify durable source generations.
- Interleavings: `A1, B1, A2`, `A1, B1, C1, A2`, repeated deletes/reinserts, mixed entity kinds sharing identical numeric IDs, same hash for distinct ImageIDs, duplicate tags, AI metadata changes.
- Fuzz malformed/missing before or after state, invalid journal sequences, byte-limit boundaries, and errors mid-apply.
- Crash-injection at intent, each family mutation boundary, liveness publication, index durability, acknowledgment and cleanup. On reopen, compare the final state against ordered replay under the existing recovery policy.
- Add counters for original logical events, coalesced groups, actual engine operations and mutations avoided.

**Gate:** no coalescing path merges unsupported operations; all index/liveness/recovery equivalence tests pass. If this gate cannot be met, leave unsafe families using sequential sub-batches and document them—do not weaken guarantees to reach a performance target.

## 7. Pebble direct encoding benchmark (PR 2, optional adoption)

Compare reusable scratch plus `Batch.Set` against Pebble v2.1.5's actual deferred API (`SetDeferred`/`Finish`, exact signatures verified from local dependency). Benchmark the **existing** store/journal formats: short keys, small canonical payloads, variable-length opaque record values, compact/large encoded projections, 1/16/32/64 mutation batches. Measure encoded-size calculation cost and lifetime constraints. No application MessagePack encoder is to be introduced just for benchmarks. Adopt direct encoding only where it measurably reduces total allocs/copies/CPU, and keep durability and journal bytes identical. For a case where determining size means double encoding, benchmark the whole path rather than the write primitive alone.

## 8. PR 6 — Final integration and verification

**Required commands** (adapt any command to documented repository specifics without dropping equivalent checks):

```bash
go build ./...
go test ./...
go test -race ./...
go vet ./...
go test -gcflags='-m=2' ./database/... ./store/... ./schema/... ./index/... ./search/... 2> escape-analysis.txt
# Run `sigmap validate` and other commands required by AGENTS.md, when available.
```

Run all existing crash/recovery suites and extra PR-specific fault-injection/property tests. Confirm backward-compatible reopen from old fixture data (schema.dat, Pebble, index files), including additive namespace migration. Compare performance against the untouched baseline using repeated runs and `benchstat`, identical hardware/settings. Measure allocation **and** retained heap, logical write volume and observable physical I/O, read p99 under concurrent ingestion, and startup recovery performance. Do not replace failures with looser assertions.

**Final report must include:**

- Baseline and changed commit SHAs; benchmark commands and environment.
- Tables for `ns/op`, `B/op`, `allocs/op` on relevant workloads; throughput and p50/p95/p99 where relevant.
- Total logical journal/index bytes and measured physical I/O (if available), clearly labeled.
- Store bitmap vs map lookup performance plus **8 KiB per-store** retained-memory cost.
- Coordinator ordinary/oversized high-water memory, block fragmentation, allocation behavior after warmup, escape analysis highlights.
- Number of unchanged associations pruned, groups safely coalesced, true engine operations avoided, unsupported groups forced into sequential application.
- Concurrency, aliasing, corruption and crash-recovery results; unchanged persisted golden fixtures.
- Any intentionally deferred work, especially sparse SimHash IDs, vector batching, journal-format delta encoding, cost-based search planning and new read-view infrastructure.

## 9. Merge gates and stopping rules

**Never label Phase 1 complete just because unit tests pass.** Complete only when correctness, persistence compatibility, memory lifetime safety, crash-recovery equivalence and performance measurements are all established.

- No disk-format or public-API breakage; optional decode-into support must be additive.
- Preserve schema registration order and existing schema golden bytes exactly.
- Preserve 64-mutation maximum; don't benchmark unavailable 128/256 database configurations.
- No dangling references to coordinator-owned storage after reset.
- No unbounded ordinary block memory retention; oversized retention bounded and documented.
- Existing search query execution remains safe during background index publication.
- Coalescing must respect engine prerequisites and generation semantics; unsupported cases fall back safely.
- No unmeasured performance claims; any significant regression must be explained and either fixed or explicitly accepted with evidence.
- Prefer a slightly slower, simpler correct implementation over one that reduces allocations by compromising durability or ownership safety.

**Execution instruction to Codex:** Start by auditing the current code and baseline. Implement in the PR order above, include focused tests in each PR, and do not move on after failing correctness or durability checks. Record deviations from the suggested structures where the real code calls for a simpler, safer representation. Do not add unrelated refactors or architecture redesigns.
