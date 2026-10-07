# simhash

`index/simhash` is a persistent, mmap-oriented **exact similarity store for the
canonical image tuple `pHash64 + PDQ256 + PDQQuality`**. It is deliberately not a
generic column store: the only supported shape is this tuple, indexed directly by
`common.ImageID`.

The full design is in [`AGENTS/SimHash.md`](../../AGENTS/SimHash.md). This README
covers usage and measured results.

## Properties

- **41 bytes/image** canonical storage, no stored `ImageID`, no offset table.
- **Lock-free readers**, one serialized writer.
- **Exact** pHash and PDQ radius queries returning `Result{ImageID, Distance}`.
- **Zero allocations** on steady-state reads and writes (caller-owned results).
- **Batch redo WAL** for in-place updates; appends need no WAL.
- **Manifest is the durable commit marker.**
- Reuses `internal/storage`; no parallel persistence framework.
- Deletion is external: `BitmapIdx` owns liveness, the store returns `ImageID`s.

## Usage

```go
import (
    "github.com/evanoberholster/imageIdx/common"
    "github.com/evanoberholster/imageIdx/index/simhash"
)

store, err := simhash.Open("./data/simhash", simhash.Config{
    MaxShardBytes:   4 << 30,
    SeqBlockRecords: 1024,
    Durability:      common.DurabilityOS,
})
if err != nil {
    return err
}
defer store.Close()

// Append: dense IDs are assigned in order; returns the first assigned ID.
first, err := store.AppendBatch([]simhash.Record{
    {PHash: ph, PDQ: pdq, Quality: q},
    // ...
})

// Point update of an existing record.
if err := store.UpsertBatch([]simhash.Update{
    {ImageID: first, Record: rec},
}); err != nil {
    return err
}

// Exact radius queries; dst is caller-owned and reused, 0 allocs.
// Distance is the exact Hamming distance as uint8 (pHash 0..64, PDQ 0..255).
var phMatches, pdqMatches []simhash.Result
phMatches = store.SearchPHash(phMatches[:0], ph, 8)
pdqMatches = store.SearchPDQ(pdqMatches[:0], &pdq, 31)

// Quiescent-only SIMD scan (no concurrent writer); same results, 0 allocs.
bulk := store.SearchPDQBulk(nil, &pdq, 31)

// Durable commit point; Close does not checkpoint.
if err := store.Checkpoint(); err != nil {
    return err
}
```

## Addressing and format

No `ImageID` is stored and there is no per-record offset table. The low 16 bits
select the slot; the high bits select the hash page:

```go
slot     := uint16(imageID & 0xffff) // 65,536 slots per hash page
hashPage := imageID >> 16
id        = hashPage<<16 | slot
```

Each hash page holds three independent, 64-byte-aligned columns:

| column | bytes | per slot |
|---|---:|---:|
| `pHash64[65536]` | 512 KiB | `slot<<3` |
| `PDQ256[65536]` | 2 MiB | `slot<<5` |
| `PDQQuality[65536]` | 64 KiB | `slot` |

**41 bytes/image**, no duplicated `ImageID`. A cache line holds exactly two PDQ
hashes. A page is followed by a runtime-only sequence region; the page stride is
`2,686,976 + 8·(65536/SeqBlockRecords)` bytes.

Shard files are capped (default 4 GiB) and selected arithmetically from the hash
page. Each shard has a dual-slot CRC32C superblock; a store manifest records the
geometry, committed high-water mark, committed generation, and shard count.
`Open` validates the geometry and rejects a CRC-valid but nonsensical manifest
(including a shard count inconsistent with the committed mark), a shard newer
than the manifest, or a shard that does not cover committed records.

## Concurrency

Many lock-free readers plus one serialized writer. In-place updates (no COW) use
a per-block seqlock of atomic counters; all column accesses are atomic, so the
tuple is `-race` clean while the seqlock detects a torn 32-byte PDQ. Readers spin
briefly before yielding and retry only the affected block. Sequence counters are
ephemeral: `Open` zeroes them after WAL recovery. `Close` waits for readers
before unmapping, and read APIs check the closed flag around the reader token, so
no read touches an unmapped arena.

## Updates and durability

- **Append/AppendBatch** write new, invisible slots: no WAL, preflight the shard
  range, write contiguous page runs, then publish.
- **Upsert/UpsertBatch** overwrite visible records via a **batch redo WAL** (one
  frame, one generation per batch). Duplicate IDs last-win; sequence blocks are
  grouped with consecutive runs.
- **Checkpoint/Sync** commit in order: flush canonical → write shard
  superblocks → flush metadata → publish the manifest (the commit marker) →
  reset the WAL. A failure before the manifest rolls back unpublished append
  state; a failure after it keeps the commit and the stale WAL is skipped or
  repaired on `Open`.
- **Close** does not checkpoint, matching `index/radixhash`.

## Search

Two exact, inclusive radius queries are supported; both are concurrent-safe
(atomic scalar loads) and return results in ascending ImageID order:

```go
func (s *Store) SearchPHash(dst []Result, query uint64, maxDistance uint8) []Result
func (s *Store) SearchPDQ(dst []Result, query *PDQHash, maxDistance uint8) []Result
```

`Result` carries only the `ImageID` and the exact Hamming `Distance` (uint8);
the caller knows which search produced it. Distance is not normalized and no
score, Top-K, sorting, heap, `CandidateSet`, or count-only API is provided.

pHash distance is exactly 0..64. PDQ distance is mathematically 0..256, so the
search threshold is 0..255 and the internal primitive stays `uint16`: a pair 256
bits apart is excluded by every threshold rather than wrapping to `Distance=0`.
The distance is never narrowed to `uint8` before the threshold test. Search
consistency is per sequence block, not whole-generation MVCC.

`SearchPDQBulk` uses the SIMD block kernel; it reads plain memory, is
**quiescent-only** (no concurrent writer), and on a quiescent store returns
exactly the same ImageIDs, distances, and order as `SearchPDQ`.

## Kernels

`internal/hashkernel` selects kernels at compile time:

- scalar reference (`math/bits`; POPCNT on amd64) — correctness oracle and
  concurrent-safe path;
- NEON block kernel on arm64 (`EOR` + `CNT`);
- AVX2 on amd64 (vector XOR + scalar POPCNT), runtime-detected but **not enabled
  by default** pending an amd64 benchmark;
- no SIMD pHash (scalar is already ~0.83 ns/hash).

## Benchmarks

Apple M4 Pro (arm64), Go 1.27, warm mmap-resident data, 0 allocs/op unless noted.

### 1M records

Destination slices are pre-sized to the record count, so `B/op` and `allocs/op`
are 0 for every row.

| query | latency | ns/hash |
|---|---:|---:|
| pHash scan d = 8 / 16 | ~0.78 ms | 0.78 |
| pHash scan d = 32 (≈half the DB) | ~5.8 ms | 5.83 |
| PDQ scan d = 16 | ~0.96 ms | 0.96 |
| PDQ scan d = 31 / 32 | ~1.45–1.48 ms | 1.45–1.48 |
| PDQ scan d = 50 | ~1.44 ms | 1.44 |

### Updates (1M, relaxed)

| operation | cost | allocs |
|---|---:|---:|
| `Upsert` (journal + in place) | ~2.4 µs | 0 |
| `UpsertBatch/64` | ~5.9 µs (~93 ns/update) | 0 |
| `Append` | ~123 ns | 0 |
| `AppendBatch/64` | ~273 ns (~4.3 ns/record) | 0 |
| checkpoint (one 43 MB shard) | ~13–15 ms | |
| open + recovery + sequence reset | ~42 µs | |
| recovery replay, 64 updates | ~156 µs | |

64 × `Upsert` is ~30× the per-update cost of one `UpsertBatch(64)`.
`UpsertBatch` scales smoothly: 1/4/16/32/64/128/256 → 2803/2665/3150/3929/6147/
7715/11027 ns/op (0 allocs). `AppendBatch/64` is ~273 ns at every boundary
(within page 268, page 296, sequence block 272, shard 281).

Batching amortizes fsync: `UpsertBatch` sync vs relaxed is ~3.15 ms / 193 µs /
60 µs per update at 1 / 16 / 64. One 64-update WAL frame is 3176 B (~49.6 B per
update) versus 5696 B for 64 individual frames.

### Kernels and SIMD (arm64)

| kernel | ns/op | per hash | speedup |
|---|---:|---:|---:|
| PDQ distance, scalar / NEON | 1.39 / 1.57 | — | 0.9× |
| PDQ 64-record block mask, scalar | 69.4 | 1.08 | — |
| PDQ 64-record block mask, NEON | 35.8 | 0.56 | **1.94×** |
| pHash scalar (64 hashes) | 53.3 | 0.83 | — |

`SearchPDQBulk` (NEON) vs `SearchPDQ` (scalar), 1M, 0 allocs:

| threshold | scalar | bulk | speedup |
|---:|---:|---:|---:|
| 0 | 962 µs | 631 µs | 1.53× |
| 16 | 955 µs | 637 µs | 1.50× |
| 31 | 1461 µs | 638 µs | 2.29× |
| 32 | 1449 µs | 638 µs | 2.27× |
| 50 | 1435 µs | 705 µs | 2.04× |
| 255 | 2581 µs | 3054 µs | 0.85× |

Every threshold is `uint8` (0..255). The old d=256 case is no longer a search
threshold: an exact distance of 256 is excluded rather than represented, and the
bulk kernel simply never sets its mask bit for it. At d=255 essentially every
record matches, so the bulk path's second exact pass dominates; that is far above
any real threshold and remains correct.

## Allocation budget

| operation | B/op | allocs/op |
|---|---:|---:|
| `Get`, `Quality` | 0 | 0 |
| `SearchPHash`, `SearchPDQ`, `SearchPDQBulk` | 0 | 0 |
| `Append`, `AppendBatch/64` | 0 | 0 |
| `Upsert`, `UpsertBatch/64` | 0 | 0 |

Searches require caller-provided capacity. Guarded by
`testing.AllocsPerRun`. No maps, `sync.Pool`, reflection, interfaces in loops,
COW, MVCC, or B-trees.

## Testing

`go test ./index/simhash/...` covers exactness against an oracle, pHash and PDQ
boundary thresholds (including pHash 0/63/64 and PDQ 254/255/256 with 256
excluded at threshold 255), page/shard boundaries, quality persistence, reopen
and generation selection, arithmetic boundaries including `MaxUint64`, manifest
corruption, journal recovery at every stage, persistence failure injection at
every commit stage, stale WAL after commit, batch atomic recovery, closed-store
safety and Close-vs-reader stress, and randomized differential tests against
brute-force Hamming distance for both searches plus `SearchPDQ == SearchPDQBulk`.
All pass under `-race`.
