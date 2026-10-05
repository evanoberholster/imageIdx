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
- `Open`: restores the **committed** generation from the manifest, so an
  uncommitted `Upsert` generation bump rolls back on reopen even though its
  canonical bytes may be repaired from the journal.

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
