# simhash

`index/simhash` is a persistent, mmap-oriented **exact similarity store for the
canonical image tuple `pHash64 + PDQ256 + PDQQuality`**. It is deliberately not a
generic column store: the only supported shape is this tuple, indexed directly by
`schema.ImageID`.

Current architecture is summarized in [`AGENTS/ARCHITECTURE.md`](../../AGENTS/ARCHITECTURE.md#perceptual-hashes). This README covers usage and measured results.

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
    "github.com/evanoberholster/imageIdx/schema"
    "github.com/evanoberholster/imageIdx/index/simhash"
)

store, err := simhash.Open("./data/simhash", simhash.Config{
    MaxShardBytes:   4 << 30,
    SeqBlockRecords: 1024,
    Durability:      schema.DurabilityOS,
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
// Distance is the exact Hamming distance as uint8 (pHash 0..64, PDQ 0..128).
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

## Sparse external IDs

Contiguous IDs keep the dense hash-page append path. `ApplyUpdates` accepts
arbitrary nonzero `uint64` external IDs and persists non-dense records in
immutable CRC-checked `sparse-<generation>.seg` files published by an atomically
replaced `sparse.manifest`. Ordinary batches write one bounded sorted delta
segment; compaction merges live records and dense-suppressing tombstones into a
replacement segment. Readers see an atomic base+overlay view, filter dense IDs
overridden by the overlay, and preserve ascending `ImageID` result order. Once
the segmented sparse overlay is used, dense `AppendBatch`/`UpsertBatch` calls
are rejected so there is only one source of truth. A crash before manifest
publication leaves only an orphan; complete corrupt segments and manifests fail
closed. The retired experimental `sparse.bin`/`*.sparse` format is rejected with
an explicit rebuild error.

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

| | mathematical distance | search threshold | `Result.Distance` |
|---|---:|---:|---:|
| pHash | 0..64 | 0..64 | `uint8` |
| PDQ | 0..256 | 0..`MaxPDQSearchDistance` (128) | `uint8` |

PDQ is 256 bits, but useful similarity searches stay far below half the hash.
`SearchPDQ` and `SearchPDQBulk` treat `maxDistance` as a similarity radius in
`0..128`: a larger value is clamped to `MaxPDQSearchDistance`, not rejected.
Because only `distance <= threshold <= 128` can be returned, every `Distance` is
exact and fits `uint8` — there is no `255` sentinel, `256` special case, or
saturation. `DistancePDQ` is unaffected and still returns the exact mathematical
`0..256` for callers and tests that need it. Search consistency is per sequence
block, not whole-generation MVCC.

`SearchPDQBulk` uses the SIMD block kernel; it reads plain memory, is
**quiescent-only** (no concurrent writer), and on a quiescent store returns
exactly the same ImageIDs, distances, and order as `SearchPDQ`, with the same
clamping.

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
| PDQ scan d = 16 | ~0.95 ms | 0.95 |
| PDQ scan d = 31 / 32 | ~1.44–1.45 ms | 1.44 |
| PDQ scan d = 40 / 50 | ~1.44–1.52 ms | 1.43–1.52 |
| PDQ scan d = 64 | ~6.6 ms | 6.64 |
| PDQ scan d = 96 | ~7.2 ms | 7.24 |
| PDQ scan d = 128 | ~7.7 ms | 7.74 |

The scalar PDQ scan slows sharply once the threshold approaches an intermediate
word-partial mean (≈64, ≈96) because the reject branch becomes unpredictable;
that regime is far from realistic thresholds. `SearchPDQBulk` avoids most of it
(see below).

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
| 0 | 958 µs | 585 µs | 1.64× |
| 8 | 973 µs | 629 µs | 1.55× |
| 16 | 956 µs | 644 µs | 1.48× |
| 31 | 1448 µs | 642 µs | 2.25× |
| 32 | 1450 µs | 644 µs | 2.25× |
| 40 | 1515 µs | 641 µs | 2.36× |
| 50 | 1451 µs | 719 µs | 2.02× |
| 64 | 6659 µs | 5313 µs | 1.25× |
| 96 | 7228 µs | 904 µs | 7.99× |
| 128 | 7691 µs | 6666 µs | 1.15× |

Every threshold is `uint8` in `0..MaxPDQSearchDistance` (128); larger values are
clamped, so there is no `255`/`256` search case. In the realistic band
(16–50) bulk is 1.5–2.4× faster than the concurrent-safe scalar scan.

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
boundary thresholds (pHash 0/63/64; PDQ up to 128 plus the clamp equivalence of
129/200/255), the exact PDQ mathematical range 0..256 on `DistancePDQ`
(including 254/255/256), page/shard boundaries, quality persistence, reopen and
generation selection, arithmetic boundaries including `MaxUint64`, manifest
corruption, journal recovery at every stage, persistence failure injection at
every commit stage, stale WAL after commit, batch atomic recovery, closed-store
safety and Close-vs-reader stress, and randomized differential tests against
brute-force Hamming distance for both searches plus `SearchPDQ == SearchPDQBulk`.
All pass under `-race`.
