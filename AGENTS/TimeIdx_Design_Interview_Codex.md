# Codex Instructions: Interactive TimeIdx Design Interview

## Mission

Design a high-performance, persistent Go `timeIdx` for `imageIdx` that maps **time keys (`uint64`) to `ImageID` (`uint64`)**, supports duplicate time keys, and uses **Structure of Arrays (SoA)**. Aim for architectural consistency with the **latest `geoIdx`**, particularly single-file mmap persistence, immutable/read-only published views, lock-free readers, one serialized commit publisher, and size-bounded shards **where those features actually exist and are proven in the repository**.

**Do not implement or modify code yet.** First inspect the current repository and conduct an interactive requirements interview. Produce a design proposal only after the user has answered the blocking questions. Do not fill in unresolved requirements as though they were confirmed.

## Repository discovery (before questioning)

1. Inspect the latest local `geoIdx`, `timeIdx` (if present), shared storage code, allocator, WAL/recovery code, public search interfaces, tests, and benchmarks. If network/GitHub access exists, verify the relevant current branch/PR; otherwise clearly identify the revision you inspected.
2. Distinguish **implemented and tested behavior** from proposed behavior. In particular, do not assume `geoIdx` already has a crash-safe single-file allocator or that its persistence protocol is safely reusable.
3. Summarize in no more than 10 bullets which existing components are reusable, what must be designed, and any conflicts with the requirements below.
4. Ask only questions that the code cannot answer or for which product/design choices remain open. Quote relevant repository paths/symbols when explaining a choice.

## Required invariants / constraints

- Logical relation: `(TimeKey, ImageID)`; many images may share the same time, and the handling of multiple time records per image must be decided.
- Physical data layout: **SoA** (e.g., sorted `Times[]`/`ImageIDs[]`, or `BaseTime + Offsets[]` with corresponding `ImageIDs[]`).
- Preferred storage: **one persistent index file**, not a directory of per-shard files. Any WAL requirement must be explicitly reconciled with that constraint.
- Concurrency: lock-free readers over immutable published snapshots, single serialized commit publisher, safe lifetime management of old mappings/extents.
- Optimize for small write batches (approximately 64 entries unless confirmed otherwise), random historical imports, chronological queries, and low allocation; measure before adding SIMD or compression complexity.
- Preserve correctness under duplicates, deletes, updates, process crashes, and mmap growth.
- Do not change the public API, storage file format, or write path before the design is confirmed.

### Important timestamp correction — ask this first

**A 24-bit shift on a Unix-nanosecond timestamp is ~16.777216 ms, NOT 50 ms.** A 26-bit shift is ~67.108864 ms. Exact 50 ms buckets require division by 50,000,000 ns (or an equivalent encoding). The phrase "minimum of 50 milliseconds" is ambiguous: does it mean a bucket **at least** 50 ms wide, or resolution **no worse than** 50 ms? Ask explicitly. The coverage in calendar years also depends on the timestamp's epoch, unit, signedness, and stored bit width. Never assume arbitrary signed Unix nanoseconds can be used unmodified as `uint64` without specifying an order-preserving encoding for pre-epoch times.

## How to conduct the interview

1. Start with the **Phase 1 blocking questions**, in groups of **3–5 questions per turn**. Wait for the user's answers before asking the next group.
2. For each question, offer **2–4 concrete options**, a short consequence/tradeoff, and a **suggested default** clearly labeled **provisional**. Use stable IDs (`T01`, `S01`, etc.) so answers can be tracked.
3. Respect answers already supplied. Do **not** repeat an answered question. Resolve ambiguity with a narrow follow-up.
4. After Phase 1, give a concise **decision ledger**: `ID | answer | source (user/code/default) | unresolved`. Ask for confirmation before proceeding to Phase 2.
5. In Phase 2, ask only architecture choices that remain genuinely open after code inspection; in Phase 3, settle APIs/benchmark gates. Group questions, do not dump all questions at once unless explicitly requested.
6. When answers conflict, state the conflict and ask which requirement has priority. **Never treat a proposed default as an approved decision.**
7. Once decisions are complete, write `TimeIdx_Design.md` with layout, binary schema, algorithms, pseudocode, recovery protocol, consistency invariants, failure cases, and test/benchmark gates. Request **explicit design approval**. Only implement if the user separately authorizes implementation.

---

## Phase 1 — Blocking semantics and workload questions

### Timestamp units and quantization

- **T01. Input semantics:** What is the input `uint64`? (A) Unix nanoseconds, (B) Unix milliseconds, (C) existing custom unsigned time encoding, (D) another format? What epoch is used?
- **T02. Meaning of 50 ms:** Is 50 ms (A) the **minimum bucket duration** (coarser is acceptable), (B) the **maximum tolerated precision loss** (finer is acceptable), or (C) **exactly 50 ms**?
- **T03. Quantization method:** (A) `timestamp >> 24` (~16.78 ms on nanoseconds), (B) `>> 26` (~67.11 ms), (C) `/ 50_000_000` (exact 50 ms), (D) custom encoding? Explain mismatch with T02 before suggesting a default.
- **T04. Supported years/range:** Earliest and latest date? Must pre-1970 dates be supported? Should invalid/unknown/out-of-range times be rejected, stored separately, or represented by a sentinel?
- **T05. Stored precision:** Keep (A) full input timestamps, (B) only quantized keys, or (C) keys plus optional exact timestamp elsewhere? Is this index allowed to collapse all times within a bucket?
- **T06. Ordering/timezones:** Are inputs normalized to UTC instants before indexing? Are local/EXIF times without timezone handled outside `timeIdx`? What defines equality and ordering?

### Mapping semantics and query workload

- **W01. Cardinality:** Can each `ImageID` have (A) exactly one indexed time, (B) multiple named time types (capture/import/modify), or (C) multiple occurrences of the same type?
- **W02. Duplicate pairs:** Can the **same `(TimeKey, ImageID)`** be inserted twice? Should insertion be idempotent, reject duplicates, or preserve multiset entries?
- **W03. Timestamp updates:** Can the time associated with an existing image change? If so, is the old time known by the caller, or must the index locate it by `ImageID`?
- **W04. Required queries:** Which are mandatory: exact bucket, inclusive/exclusive time range, newest/oldest N, near-time N, iterator/pagination, count-only, or scan of `(Time, ImageID)` pairs?
- **W05. Ordering/tie-breaks:** Ascending, descending, both? Is stable `(TimeKey, ImageID)` order required for pagination and duplicates?
- **W06. Import distribution:** Mostly newest-first append, mostly historical random dates, or mixed? Expected query-to-write ratio? Expected time skew and concentration on identical timestamps?
- **W07. Batch and scale:** Is ~64 entries per write batch a firm upper bound? Target index size (1M/10M/100M/1B entries), ingestion rate, range-result sizes, and concurrency targets?

**Phase 1 exit gate:** Timestamp encoding, year coverage, duplicate semantics, required query set, and basic workload must all be explicit. Validate year-span arithmetic for the selected encoding, including overflow and boundary behavior.

---

## Phase 2 — Storage and mutation architecture

### Shards and physical SoA representation

- **S01. Shard strategy:** (A) size-bounded adaptive shards (geoIdx-like), (B) fixed time-window shards, or (C) segmented append plus historical partitions? Which matches real ingestion patterns?
- **S02. Shard size:** Target entries/bytes per shard (e.g., 16K / 64K / 256K entries), and maximum single-shard size? Set from benchmarks, not intuition.
- **S03. Split rule:** Split by entry position, by distinct timestamp, or fixed chronological boundary? A popular timestamp may occupy >1 shard; explicitly handle shards with equal boundary keys.
- **S04. Shard directory:** Which keys/offsets/counts are stored for lookup, and how are overlapping/equal min/max timestamps routed so no duplicates are missed?
- **S05. Timestamp SoA width:** (A) full `uint64[]`, (B) `BaseTime + uint32[]` offsets, (C) width-specific fixed arrays (`uint16/32/64`), (D) packed/delta encoding with seek blocks? How do time-span overflow and width promotion work?
- **S06. ID representation:** Confirm `ImageID` width, invalid ID/sentinel rules, sorting tie-break, alignment, endian format, and overflow rules.
- **S07. Compression vs search:** What is the space-per-entry target? How much binary-search complexity, CPU overhead, and rewrite amplification are acceptable?

### One-file mmap and crash consistency

- **P01. Reuse boundary:** Which **verified** `geoIdx` components can be reused (shard directory, allocator, snapshots, storage header, compactor), and which should remain index-specific?
- **P02. Single-file scope:** Does "one file" mean **strictly one file including recovery journal/WAL**, or one primary index file plus permitted auxiliary temporary/log files during transactions?
- **P03. Allocation:** Smallest allocation unit (512 bytes vs OS-page-aligned 4 KiB vs mixed classes)? How are adjacent free extents coalesced? What alignment is required for mmap and array decoding?
- **P04. Crash protocol:** Dual-superblock generations, checksums, write ordering, fsync durability, torn-write detection, and recovery selection? Specify which writes must persist before publishing root metadata.
- **P05. File growth:** Strategy for mmap remapping when growing the same file: preserve older mappings until readers exit or equivalent safe mechanism? How is truncation/reuse prevented while readers reference extents?
- **P06. Reclamation:** Reader epochs, pinned snapshots, refcounting, or equivalent? When can obsolete shard extents be recycled and coalesced?
- **P07. Compaction:** Trigger by delta/base ratio, absolute size, write amplification, time hotness, or mix? May writers commit while compaction builds immutable replacements?
- **P08. Atomicity:** Must a batch of up to ~64 appear atomically to readers? Are cross-index transactions (`timeIdx` + `bitmapIdx`/other indexes) in scope?

### Write path and deletes

- **M01. Mutations:** Buffer + merge, immutable delta shards, in-file COW updates, or hybrid? What paths optimize monotonic append vs older random timestamps?
- **M02. Delete representation:** Internal tombstones, shared `bitmapIdx` tombstones, or explicit deletion entries? Define commit visibility and recovery semantics.
- **M03. Updates:** Atomic remove-old/add-new? How does the system locate old entries if only `ImageID` is provided, without a reverse map?
- **M04. Duplicate timestamp handling:** If one timestamp spans shards, how do insertion, compaction, exact search, and range iteration remain correct?
- **M05. Retention/cleanup:** When are tombstones physically reclaimed? How do failed commits and aborted compactions avoid leaking reusable extents?

**Phase 2 exit gate:** There must be a consistent and implementable single-file layout, allocator/reclaimer, mmap growth strategy, publication protocol, write path, and recovery story. Do not claim "crash safe" without a concrete write-order and failure-injection test plan.

---

## Phase 3 — API, interoperability, performance, and verification

### Search API

- **A01. Output:** ImageIDs only, `(Time, ImageID)` pairs, or both? When is the exact original timestamp required?
- **A02. Iteration:** Caller-provided result buffer, zero-allocation iterator, callback scanning, or several forms? How do snapshot acquire/release and error reporting work?
- **A03. Range boundaries:** Inclusive vs exclusive `start/end`; behavior when `start > end`, at extrema (`0`/`MaxUint64`), and with quantization bucket boundaries?
- **A04. Pagination:** Stable resume key `(TimeKey, ImageID)` or opaque cursor? Should pagination remain valid across commits or only within a pinned snapshot?
- **A05. Search integration:** Should `timeIdx` only yield ordered IDs, or perform candidate intersection with `bitmapIdx`? How does it plug into unified search APIs?
- **A06. Consistency:** Snapshot consistency across shards? Across *other index families*? Visibility after delete/commit?

### Optimization

- **O01. Algorithm selection:** Binary search on sorted fixed-width arrays plus sequential scan as baseline; any case for interpolation search, chunk indexes, or galloping search?
- **O02. SIMD:** Are AVX2 and ARM64 NEON paths required now, or only if scalar baselines lose a defined benchmark? SIMD should not be added to an already contiguous range scan without proof.
- **O03. Zero allocation:** Which operations must guarantee zero heap allocations (point lookup, range setup, `Next`, batch insertion)? What temporary buffers can be reused safely?
- **O04. Persistence tradeoff:** Priority ranking of read latency, sequential throughput, ingestion rate, disk footprint, file growth, compaction amplification, and code simplicity?

### Benchmarks, tests, and release gates

- **V01. Benchmark workloads:** Exact lookups (hits/misses/duplicate-heavy), short/medium/long ranges, newest N, large scans, append batches, random historical batches, mixed read/write, and compaction.
- **V02. Baselines:** Compare full-width SoA vs base-offset SoA, size-bounded vs time-based shards, and scalar vs SIMD (only where meaningful).
- **V03. Correctness:** Property tests vs reference sorted pairs; duplicate boundary keys; max/min times; quantization; 32-bit offset overflow/promotion; updates and tombstones.
- **V04. Recovery:** Crash/fault injection at each commit/allocator transition; both superblock generations; incomplete compactions; extents still pinned by readers; file growth and reopen.
- **V05. Concurrency:** Race-detector tests, long-lived readers during commits/compactions/remaps, no stale references to freed/reused extents, explicit durability/visibility contracts.
- **V06. Acceptance thresholds:** User-defined goals for p50/p99 search latency, allocations/op, disk bytes/entry, batch throughput, read throughput, and write amplification. If unspecified, require benchmark evidence and a no-regression gate rather than inventing targets.

**Phase 3 exit gate:** Public API and all correctness/performance gates are explicit and testable.

---

## Deliverable format after the interview

Write `TimeIdx_Design.md` containing:

1. Confirmed requirements and decision ledger with unanswered items highlighted.
2. Timestamp/key encoding with worked examples, epoch/year coverage, precision loss, edge cases, and validation rules.
3. Physical **single-file** on-disk format: headers/superblocks, shard directory, shard records, SoA columns, free-space records, alignment, checksums, versioning, byte order.
4. Search, batch insertion, splits (including all-equal timestamp shards), delete/update, compaction, and iterator algorithms.
5. Lock-free reader snapshots, serialized publication, mmap growth, extent reuse/coalescing, crash-safe commit/recovery sequences.
6. Compatibility boundaries with `geoIdx`, `bitmapIdx`, and unified API; no abstraction introduced solely for hypothetical reuse.
7. Concrete testing matrix, microbenchmarks, workload benchmarks, performance tradeoffs, and measurable acceptance gates.
8. Alternatives rejected and why; known risks; phase-by-phase implementation plan **without executing it**.

End by asking for explicit confirmation of the design. **Do not start implementation, open a PR, or claim completion before the design is approved and the recovery/concurrency guarantees are validated by tests.**

## First message Codex should send to the user

> I inspected the current `geoIdx` and shared index/storage code at [revision]. Here is what can actually be reused: [short factual summary]. Before designing `timeIdx`, I need to confirm the time-key format. On Unix nanoseconds, `>>24` gives 16.777216 ms buckets, whereas `>>26` gives 67.108864 ms, and exact 50 ms uses `/50_000_000`. **(T01)** What units/epoch does the input `uint64` use? **(T02)** Does "minimum 50 ms" mean bucket width must be *at least* 50 ms, or precision must be *at least as fine as* 50 ms? **(T03)** Which quantization scheme do you prefer? **(T04)** What is the earliest/latest supported year (including pre-1970)? After these answers, I'll ask the next small group rather than assume defaults.
