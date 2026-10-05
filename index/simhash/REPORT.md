# SimHash mmap Store

A specialized, fixed-width, mmap-backed columnar store for the canonical image
tuple `pHash64 + PDQ256 + PDQQuality`. It is not a generic column store: the
only supported shape is this tuple, indexed directly by ImageID.

## Addressing

No ImageID is stored and there is no per-record offset table. The low 16 bits of
the ImageID select the slot; the high bits select the logical page:

```go
slot := uint16(imageID & 0xffff)   // 65,536 slots per logical page
page := imageID >> 16
id   = page<<16 | slot
```

ImageIDs are dense, stable, monotonically assigned from 1, and never reused, so
the committed high-water mark (`Len`) bounds every scan.

## Columnar page

Each logical page holds three independent, 64-byte-aligned columns:

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
block). The page stride is `2,686,976 + 8·blocks` bytes.

## Sharding

Shard files are capped (default 4 GiB); the shard index is derived
arithmetically from the logical page, so no shard directory is needed:

```go
shardIndex = page / pagesPerShard
```

Each shard carries a dual-slot CRC32C superblock (shard index, first page, page
count, committed count, generation). A store-level `manifest` records the format
geometry (pages per shard, sequence block size), the committed high-water mark,
the generation, and the shard count. Both reuse `internal/storage`
(`MappedFile`, `EncodeSuperblock`, `WriteFileAtomic`, `SyncDir`).

## Concurrency

Many lock-free readers plus one serialized writer. Searches never take a mutex.

Point updates are performed **in place** (no COW of the multi-megabyte page) and
protected by a per-block seqlock of `atomic.Uint64` counters:

```text
writer:  seq++ (odd)  →  store pHash/PDQ/quality  →  seq++ (even)
reader:  a = load(seq); if odd retry; read tuple; b = load(seq)
         if a != b retry — only the affected block, not the whole page
```

All column accesses use `sync/atomic`, so the tuple is race-free under `-race`
while the seqlock detects a torn 32-byte PDQ. Quality stays a 1-byte column and
is updated by an aligned 8-byte atomic read-modify-write (the writer is
serialized, so no lost updates).

Default block size is **1024 records**, validated by benchmarks (256/512/1024/
2048/4096 are all within ~5% at 100k; 1024 is the default).

## Durability

Runtime atomicity and crash durability are separate:

- **Append / Upsert** write the mmap columns and publish the in-memory view.
  Readers observe them immediately. This matches `index/radixhash`, where `Apply`
  publishes in memory and durability is explicit.
- **Checkpoint / Sync** write every shard superblock and the manifest, then
  `msync` the mappings and fsync the directory. This is the durable commit point.
- Under `DurabilitySync`, each append also persists before returning.
- After a crash without a checkpoint, records since the last checkpoint are lost
  (their manifest/generation was never published); the store reopens at the last
  committed generation. In-place point updates are likewise durable only after
  Sync/Checkpoint.

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
prefilter in a combined query.

PDQ distance uses word-at-a-time early rejection for low thresholds, a two-word
prefix for the mid range (20 < d < 40) where a single-word reject branch is
~50/50 and mispredicts, and the prefix also for high thresholds. All scans
allocate zero bytes when the caller provides capacity.

Deletion is external: the store returns ImageIDs; the caller filters them
through BitmapIdx liveness. No tombstones or presence bits live here.

## Benchmarks

Apple M4 Pro (arm64), Go 1.27, warm mmap-resident data, 0 allocs/op unless
noted.

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
| point `Upsert` (in place) | 8 ns |
| `AppendBatch` of 64 | 1.15 µs (18 ns/record) |
| checkpoint (1M records, one 43 MB shard) | ~12 ms |
| open + map + superblock select | ~42 µs |
| sequence block 256/512/1024/2048/4096 @100k | 85/84/85/82/82 µs |

Threshold 31 is exercised explicitly in correctness and performance tests, and
the two-word prefix cut it from 5.9 ms (naive word-at-a-time, branch
misprediction) to 1.37 ms. Combined pHash-first is ~3.5× faster than PDQ-first
at these thresholds because the pHash filter is cheaper and more selective.

## Correctness / race

Tests cover direct ImageID↔page/slot mapping and the `0xffff/0x10000` boundary,
pHash and PDQ exact distance, threshold-boundary inclusion, threshold 31,
combined AND semantics in both orders, quality persistence, append visibility,
reopen, generation selection, shard boundaries and reopen, sequence odd/even,
and concurrent readers during pHash/PDQ/quality updates. `TestNoTornPDQ` ties the
pHash to the PDQ value and asserts no mixed tuple is ever observed; the
concurrency tests pass under `-race`.

## Not in this change

- **SIMD kernels** (AVX2 / AVX-512 VPOPCNTDQ / NEON). The layout is ready and
  the scalar implementation is the baseline; kernels are added behind the same
  scan loop once benchmarks justify them. Note that plain-memory SIMD kernels
  cannot run unsynchronized against in-place updates, so the SIMD path is for
  quiesced scans or must be combined with the sequence validation.
- **Accelerators** (MIH, LSH, bit-sliced, 32×8). The canonical format does not
  depend on any of them.
