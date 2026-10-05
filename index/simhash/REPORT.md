# SimHash mmap Store

A specialized, fixed-width, mmap-backed columnar store for the canonical image
tuple `pHash64 + PDQ256 + PDQQuality`. It is not a generic column store: the
only supported shape is this tuple, indexed directly by ImageID.

## Addressing

No ImageID is stored and there is no per-record offset table. The low 16 bits of
the ImageID select the slot; the high bits select the hash page:

```go
slot     := uint16(imageID & 0xffff)   // 65,536 slots per hash page
hashPage := imageID >> 16
id        = hashPage<<16 | slot
```

ImageIDs are dense, stable, monotonically assigned from 1, and never reused, so
the committed high-water mark (`Len`) bounds every scan.

## Columnar hash page

Each hash page holds three independent, 64-byte-aligned columns:

| column | bytes | per slot |
|---|---:|---:|
| `pHash64[65536]` | 512 KiB | `slot<<3` |
| `PDQ256[65536]` | 2 MiB | `slot<<5` |
| `PDQQuality[65536]` | 64 KiB | `slot` |

**41 bytes/image** of canonical data, no duplicated ImageID. A 64-byte cache
line contains exactly two PDQ hashes, so the layout is trivially AVX2
(4×uint64), AVX-512 (8×uint64), and NEON friendly. A pHash scan reads only the
pHash column, a PDQ scan only the PDQ column, and quality only when required.

Following each page's payload is a small sequence-counter region (8 bytes per
block). The hash page stride is `2,686,976 + 8·blocks` bytes.

## Sharding

Shard files are capped (default 4 GiB); the shard index is derived
arithmetically from the hash page, so no shard directory is needed. The
arithmetic is checked for overflow and `actualShardBytes <= MaxShardBytes` holds
for every supported `SeqBlockRecords` (256/512/1024/2048/4096).

Each shard carries a dual-slot CRC32C superblock (shard index, first page, page
count, committed count, generation). A store-level `manifest` records the format
geometry, the committed high-water mark, **the authoritative committed
generation**, and the shard count. Both reuse `internal/storage`.

On open the manifest generation is read first; a shard superblock with a newer
generation is an interrupted commit and is **not** selected, and a shard that
does not cover the committed records is rejected as `ErrCorrupt`.

## Concurrency

Many lock-free readers plus one serialized writer. Searches never take a mutex.

Point updates are **in place** (no COW) under a per-block seqlock of
`atomic.Uint64` counters:

```text
writer:  seq++ (odd) → store pHash/PDQ/quality → seq++ (even)
reader:  a = waitSeqEven(seq); read tuple; b = load(seq); retry block if a != b
```

All column accesses are atomic, so the tuple is race-free under `-race` while the
seqlock detects a torn 32-byte PDQ. Quality stays a 1-byte column and is updated
by an aligned 8-byte atomic read-modify-write. A reader spins briefly
(`seqSpinLimit = 64`) before yielding, instead of yielding on every observed odd
sequence. Default block size is 1024 records.

**Sequence counters are ephemeral.** They live in the mmap file but are
meaningless across processes; `Open` zeroes every counter after recovery, so a
crash that left one odd can never make a reader spin forever.

## Crash recovery (update journal)

A redo journal (`updates.wal`, built on `internal/storage`'s framed WAL) makes
in-place point updates crash-safe:

```text
1. serialize writer
2. append journal record: ImageID + full replacement tuple + CRC
3. fsync the journal when DurabilitySync is set
4. seqlock → odd
5. update canonical columns in place
6. seqlock → even
7. advance/publish the generation
8. Checkpoint/Sync makes canonical + manifest durable
9. only then truncate the journal
```

`Open` replays every valid record in order (idempotent) and repairs an
interrupted tail. A crash before the record is written leaves the previous
canonical state; a crash during the write leaves a short frame that replay stops
at; a crash after the record but before/during the canonical apply is completed
by replay. A permanently mixed pHash/PDQ/quality tuple is impossible for any
update whose journal record survived. Under relaxed (OS) durability a power loss
can still lose both the record and the canonical bytes; `DurabilitySync` closes
that window by fsyncing the record before the apply.

## Durability and generation

- `Append`/`AppendBatch`: write the columns, advance the generation, publish in
  memory. Visible to readers immediately; durable at checkpoint.
- `Upsert`: journal + in-place update, advance the generation, publish.
- `Checkpoint`/`Sync`: write all shard superblocks and the manifest, flush the
  mappings, then truncate the journal.
- `Close`: waits for readers and unmaps. Like `index/radixhash`, it does **not**
  checkpoint; explicit `Sync`/`Checkpoint` is required for durability.
- `Open`: the manifest generation is the **durable checkpoint floor**; a
  validated WAL chain advances the runtime generation beyond it. For example,
  manifest `G` with WAL batches `G→G+1` and `G+1→G+2` opens at runtime
  generation `G+2`. An uncheckpointed `Upsert` is **recovered** from the WAL, not
  rolled back. The three notions are distinct: checkpoint generation (manifest),
  recoverable WAL state, and runtime generation.

### Failure semantics

- **Append preflight.** Required shards form a contiguous arithmetic range and
  are ensured before any canonical write. A preflight failure leaves `committed`,
  the generation, and the reader-visible high-water unchanged; any shard already
  created is retained as physical capacity, never as a partial logical append.
- **Sync append publication.** Under `DurabilitySync`, `AppendBatch` writes the
  unpublished records, then persists canonical data, shard metadata, and the
  manifest, and only then publishes `committedAtomic` and the generation. If
  persistence fails, writer state is rolled back and readers never observe the
  new IDs. Under `DurabilityOS` the faster write → publish → return path is used.
- **Search consistency.** A search samples an immutable shard-set view, the
  committed high-water at that moment, and per-sequence-block tuple consistency.
  It does **not** get whole-generation MVCC snapshot isolation: a concurrent
  writer may leave different sequence blocks at different points in time. This is
  intentional and is not solved with locks or COW.

## Search

Exact, independent, and combined:

- `SearchPHash(dst, query, maxDistance)` — `bits.OnesCount64(query^candidate)`,
  caller-owned `[]PHashMatch`.
- `SearchPDQ(dst, query, maxDistance)` — exact 256-bit distance, evaluated
  independently of pHash; caller-owned `[]PDQMatch`.
- `SearchCombined(dst, pHashQuery, maxPHash, pdqQuery, maxPDQ, order)` — AND
  semantics with both distances returned. `order` selects pHash-first or
  PDQ-first; the result is identical.

A PDQ-only query never consults pHash. pHash is used only as an explicit
prefilter in a combined query. Deletion is external: results are filtered
through BitmapIdx liveness. No tombstones or presence bits live here.

PDQ distance uses word-at-a-time early rejection for low thresholds and a
two-word prefix for the mid range (20 < d < 40) where a single-word reject branch
is ~50/50 and mispredicts. All scans allocate zero bytes when the caller provides
capacity.

## Benchmarks

Apple M4 Pro (arm64), Go 1.27, warm mmap-resident data, 0 allocs/op unless noted.

### 1M records

| query | latency |
|---|---:|
| pHash scan d ≤ 16 | 0.76–0.78 ms |
| pHash scan d = 32 (≈half the DB) | 6.1 ms |
| PDQ scan d ≤ 16 (early reject) | 0.83 ms |
| PDQ scan d = 31 / 32 (two-word prefix) | 1.37 / 1.38 ms |
| PDQ scan d = 40 / 50 | 1.55 / 1.49 ms |
| combined, pHash-first | 0.91 ms |
| combined, PDQ-first | 3.21 ms |

### Other

| operation | cost |
|---|---:|
| point `Upsert` (journal + in place) | ~2.4 µs, 0 allocs |
| `AppendBatch` of 64 | ~1.5 µs (~24 ns/record), 1 alloc |
| checkpoint (1M records, one 43 MB shard) | ~12 ms |
| open + map + journal recovery + sequence reset | ~42 µs |
| sequence block 256/512/1024/2048/4096 @100k | 85/84/85/82/82 µs |

`Upsert` grew from ~8 ns (in-place only) to ~2.4 µs because it now appends a
journal record (a `write` syscall) before mutating the columns; that is the cost
of crash safety and is not fsynced under relaxed durability.

### Profiles (`go test -cpuprofile -memprofile`)

CPU (store workloads, 40.3 s of samples): the three scalar scans account for
~76%. Flat hotspots are `searchPHash` 28%, `searchPDQ` 21%, and the inline
column addressing `pdqWord` 20%, `base` 6.6%, `imageIDFromHashPage` 6.0%; the
remainder is the popcount loop. Roughly a third of CPU is address arithmetic,
which is the headroom a future SIMD kernel would target.

Memory: the store owns essentially no Go heap. In-use heap was ~2 MB; the 41
B/image lives in the mmap arena. Searches allocate 0 bytes/op (the `B/op` at
d = 32/64 is caller result-buffer growth, since the benchmark passes `nil` dst
and d = 32 matches ≈ half the database). The update path is allocation-free for
`Upsert` (0 allocs/op) and allocates one small `view` per append batch (~80 B per
64 records) to publish the new committed high-water mark.

Threshold 31 is exercised explicitly in correctness and performance tests, and
the two-word prefix cut it from 5.9 ms (naive word-at-a-time, branch
misprediction) to 1.37 ms.

## Update path (UpsertBatch)

The expected update workload is ~64 records. A batch is one logical
transaction: **one redo WAL frame, grouped sequence transitions, one generation
advancement**.

`UpsertBatch` sizes (1M records, relaxed durability):

| updates | ns/op | ns/update | allocs/op |
|---:|---:|---:|---:|
| 1 | 2547 | 2547 | 0 |
| 4 | 2660 | 665 | 0 |
| 16 | 3263 | 204 | 0 |
| 32 | 4205 | 131 | 0 |
| 64 | 5982 | 93.5 | 0 |

64 × `Upsert` = 162013 ns (2531 ns/update) versus `UpsertBatch(64)` = 5306 ns
(82.9 ns/update): **~30× lower per-update cost**, both 0 allocs.

Locality (`UpsertBatch(64)`, ns/update):

| pattern | ns/update |
|---|---:|
| sequential | 77.0 |
| same block | 76.1 |
| same page, different blocks | 81.5 |
| spread pages | 82.9 |
| random | 84.1 |

Relaxed versus sync durability (ns/update):

| updates | relaxed | sync |
|---:|---:|---:|
| 1 | 2856 | 3,149,116 |
| 16 | 193 | 252,468 |
| 64 | 80.7 | 60,166 |

Batching amortizes the fsync ~52× (3.15 ms → 60 µs per update at 64).

WAL cost: one 64-update frame writes 3176 B (≈49.6 B/update) versus 5696 B for
64 individual frames (≈89 B/update) — about **44% fewer WAL bytes**.

Seqlock grouping (`ns/op`, grouped via consecutive runs versus per-record):

| updates | grouped same block | ungrouped same block | grouped random | ungrouped random |
|---:|---:|---:|---:|---:|
| 4 | 2601 | 2604 | 2619 | 2547 |
| 16 | 2891 | 2938 | 2960 | 2840 |
| 64 | 3612 | 3744 | 4016 | 3920 |

A sorted-by-ImageID variant was also measured; it improved only already-local
batches (~8%) and cost ~34% on random batches (5835 vs 4360 ns/op at 64), so the
shipped grouping uses **consecutive runs** with no sort: it captures local and
sequential batches at no cost and does not regress random ones.

Concurrent search while updating (`SearchPDQ` d=16, 200k records): 205 µs at
batch 1, 215 µs at batch 16, 225 µs at batch 64 versus ~200 µs with no writer —
**≤12% search-latency impact**, so batching does not create long odd-sequence
windows.

Checkpoint is ~13–14 ms regardless of pending updates (dominated by flushing the
shard mapping). Recovery `Open` replay: 0 updates 83 µs, 1 156 µs, 64 156 µs,
1024 231 µs, 10000 605 µs — linear in the WAL.

### Semantics

- **AppendBatch does not use the update WAL** because the records it writes are
  unpublished; the committed high-water mark advances only after all columns are
  written, so there is no torn-existing-record problem.
- **UpsertBatch uses the redo WAL** because it overwrites already-visible
  records; the batch frame is written (and fsynced under `DurabilitySync`) before
  the in-place columns change.
- **One UpsertBatch = one logical generation.**
- **Manifest publication is the final durable checkpoint commit marker.** The
  order is canonical flush → shard metadata write/flush → manifest LAST →
  directory sync → WAL reset.
- **Search consistency is per sequence block**, not whole-query snapshot
  isolation.

## Allocation budget

Steady-state allocation (M4 Pro, `-benchmem`):

| operation | ns/op | B/op | allocs/op |
|---|---:|---:|---:|
| `Get` | 9 | 0 | 0 |
| `SearchPHash` | 0.61 ms / 1M | 0 | 0 |
| `SearchPDQ` | 1.02 ms / 1M | 0 | 0 |
| `SearchCombined` | 0.78 ms / 1M | 0 | 0 |
| `Upsert` | ~2.4 µs | 0 | 0 |
| `UpsertBatch/64` | 5.1 µs (79 ns/update) | 0 | 0 |
| `Append` | 123 | 0 | 0 |
| `AppendBatch/64` | 346 (5.4 ns/record) | 0 | 0 |

Searches are measured with caller-preallocated result buffers; the `B/op` seen
at d = 32/64 is caller result growth, not the store. Escape analysis
(`go test -gcflags=-m`) shows no per-operation heap escapes in `Append`,
`AppendBatch`, `Upsert`, `UpsertBatch`, `Get`, or the searches; the remaining
escapes are `Open`, checkpoint, shard creation (rare), and caller result slices.
The 64-update WAL frame is encoded directly into reusable WAL scratch, so a
transaction allocates no intermediate representation. Recovery reuses one header
and one payload buffer and decodes updates by value.

The implementation deliberately uses **arithmetic addressing** (`id → hash page →
slot → shard`), **fixed arrays and bounded slices**, and **linear scans over ≤64
items** instead of maps, because the workload is small and bounded. There are no
maps, no `sync.Pool`, no reflection, and no interfaces in the scan/update loops.
Larger batches run through the same single linear pass (one WAL frame up to
`maxBatchRecords`); ≤64 is the tuned fast path. A benchmark-only fixed-array
insertion sort for block grouping was measured and rejected: consecutive-run
grouping was faster both locally (5222 vs 5491 ns/op) and at random (5819 vs 6718
ns/op).

`AppendBatch` preflights the whole batch before touching canonical state: the
required shards form a contiguous range derived arithmetically from the first and
last ImageIDs, and all are ensured before the first record is written, so a
preflight failure leaves logical state unchanged (no map of required shards).

### Final performance pass

- **Append addressing.** `writeRecordsLocked` now processes contiguous runs
  within a hash page and hoists the column bases once per run. `AppendBatch/64`
  improved from 346 to ~273 ns/op (0 allocs).
- **Append boundaries** (`AppendBatch/64`, ns/op): within page 268, page
  boundary 296, sequence-block boundary 272, shard boundary 281 — smooth, no
  cliff, 0 allocs.
- **Larger batches** degrade smoothly (`UpsertBatch` ns/update): 1→2803,
  4→2665, 16→3157, 32→3929, 64→6147 (96/update), 128→60/update, 256→43/update.
- **WAL frame size** grows linearly, ~49.6 B/update plus ~24 B fixed: 89 / 824 /
  1608 / 3176 / 6312 / 12584 bytes for 1/16/32/64/128/256 updates; 64 individual
  frames write 5696 B. A ≤64 batch stays one frame, one generation.
- **Duplicate ImageIDs** take no slow path: `UpsertBatch` with 64 records that
  collide on two IDs is 77 ns/update, 0 allocs.
- **Search addressing.** The scan loops hoist the per-page column base and
  reconstruct the ImageID from a per-page base. Measured A/B against the previous
  helper-based loops on the same machine: ~5% faster PDQ d=16, ~12% faster
  combined pHash-first, ~12% faster PDQ d=31.
- **Unpublished quality writes** keep the aligned atomic read-modify-write. A
  plain byte store was considered and rejected: an unpublished slot can share its
  quality word with a published neighbour that a concurrent `Quality` read loads
  atomically, so a plain store would race.

## Kernel package (SIMD and scalar)

`index/simhash/internal/hashkernel` holds the distance kernels behind one
dispatch point:

- **Scalar reference** (`math/bits`) — the correctness oracle and the
  concurrent-safe implementation. On amd64 it lowers to POPCNT.
- **NEON block kernel** (arm64) — loads two 16-byte vectors per record, XORs
  against the preloaded query, popcounts with `VCNT`, and reduces with
  `VUADDLV`, rejecting after the first 16 bytes when the partial count exceeds
  the limit.

Kernel microbenchmarks (arm64):

| kernel | ns/op | speedup |
|---|---:|---:|
| PDQ distance, scalar | 1.38 | — |
| PDQ distance, NEON | 1.58 | 0.9× |
| PDQ 64-record block mask, scalar | 70.2 | — |
| PDQ 64-record block mask, NEON | 37.4 | **1.9×** |

The single-pair kernel is not where SIMD wins (one vector operation vs an
inlined popcount); the block kernel is.

Store-level `SearchPDQBulk` (NEON block kernel over the canonical PDQ column)
versus the concurrent-safe scalar `SearchPDQ`, 1M records, 0 allocs/op:

| threshold | scalar | bulk |
|---:|---:|---:|
| 0 | 1.00 ms | 0.61 ms (1.6×) |
| 16 | 1.00 ms | 0.63 ms (1.6×) |
| 31 | 1.61 ms | 0.63 ms (2.5×) |

`SearchPDQBulk` reads plain memory and is documented as requiring a quiescent
store (no concurrent writer); `SearchPDQ` remains the concurrent-safe scalar
path. The tests compare the NEON kernel against the scalar reference for random
pairs and every block-count boundary, and `SearchPDQBulk` against `SearchPDQ`
and the oracle.

## Correctness / race

Tests cover direct ImageID↔hashPage/slot mapping and the `0xffff/0x10000`
boundary, pHash and PDQ exact distance, threshold-boundary inclusion, threshold
31, combined AND semantics in both orders, quality persistence, append
visibility, reopen, generation selection, shard boundaries and reopen, sequence
odd/even, concurrent readers during pHash/PDQ/quality updates, and a
no-torn-PDQ test. Recovery tests cover a crash before journal publication, after
journal publication, mid-apply after pHash and after each PDQ word, and after the
quality write, plus an odd sequence left by a crash, a torn journal tail, a
corrupt journal payload, a shard newer than the manifest, a manifest newer than a
shard, a corrupt superblock slot, a missing shard, and 4 GiB geometry across all
sequence block sizes. Atomic column alignment is asserted for the first and last
slot of an 8-byte quality group and at the hash page boundary. All pass under
`-race`.

## Not in this change

- **SIMD kernels** (AVX2 / AVX-512 VPOPCNTDQ / NEON). The layout is ready and the
  scalar concurrent-safe path is the reference baseline. Enabling SIMD requires a
  design proving it is safe with the sequence validation, a quiescent scan epoch,
  or immutable memory — plain wide loads are not sufficient against in-place
  writes.
- **Accelerators** (MIH, LSH, bit-sliced, 32×8). The canonical format does not
  depend on any of them.
