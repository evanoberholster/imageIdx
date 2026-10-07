# SimHash Index

## Goal

Build a persistent, mmap-oriented **exact similarity store for the canonical
image tuple `pHash64 + PDQ256 + PDQQuality`**. It is deliberately not a generic
column store: the only supported shape is this tuple, indexed directly by
`common.ImageID`.

The design priorities, in order:

1. Correctness, including crash consistency for in-place updates.
2. Predictable, low allocation: zero allocation on steady-state reads and writes.
3. Query/read performance for exact Hamming distance.
4. Simple, auditable persistence.
5. Compact canonical storage (41 bytes/image).

It reuses `internal/storage` (mmap, dual-slot CRC32C superblock, atomic file
publication, WAL transport, directory sync, reader epochs) and
`common.Namespace`/`Schema` for identity. It does not add a parallel
persistence framework.

## Non-goals

- No tombstones or deletion state. `BitmapIdx` is authoritative for liveness;
  the store returns `ImageID`s and the caller filters them.
- No similarity accelerators (MIH, LSH, bit-sliced, 32×8). The canonical format
  must not depend on any of them.
- No COW pages, MVCC, B-trees, offset tables, page lookup maps, `sync.Pool`,
  reflection, or interfaces in scan/update loops.
- No AVX-512 in this phase.

## Packages and files

```text
index/simhash/
    doc.go         package overview and invariants
    types.go       PDQHash, Record, Update, matches, Config, errors, distances
    shard.go       hash-page geometry, addressing, shard columns, superblock codec
    store.go       Store, Open/manifest/validation, Get, reader view
    apply.go       append, upsert/batch, grouped apply, persistence commit
    journal.go     batch redo WAL (framed, fixed-width, CRC)
    search.go      concurrent-safe scalar scans + quiescent SearchPDQBulk
    internal/hashkernel/
        scalar.go  reference kernels + dispatch types
        generic.go non-arm64 direct entry points (scalar)
        arm64.go   NEON PDQ kernel selection
        arm64.s    NEON assembly
        amd64.go   AVX2 kernel selection (not enabled by default)
        amd64.s    AVX2 assembly
```

## Hash page format

There is no stored `ImageID` and no per-record offset table. The low 16 bits of
the ID select the slot; the high bits select the hash page:

```text
slot     = id & 0xffff        // 65,536 slots per hash page
hashPage = id >> 16
id       = hashPage<<16 | slot
```

ImageIDs are dense, stable, monotonically assigned from 1, and never reused. The
committed high-water mark (`Len`) bounds every scan. `ImageID 0` is reserved.

Each hash page holds three independent, 64-byte-aligned columns:

```text
pHash64[65536]      512 KiB    slot<<3
PDQ256[65536]         2 MiB    slot<<5
PDQQuality[65536]    64 KiB    slot
```

**41 bytes/image**, with no duplicated `ImageID`. A 64-byte cache line holds
exactly two PDQ hashes, so the layout is naturally AVX2/NEON friendly. A pHash
scan reads only the pHash column, a PDQ scan only the PDQ column, and quality is
read only when required.

A page is followed by a runtime-only sequence region (8 bytes per block). The
hash page stride is therefore:

```text
2,686,976 + 8 * (65,536 / SeqBlockRecords)      // 2,686,976 + 512 at 1024
```

Page starts are 64-byte aligned, so every column and every atomic `uint64` is
aligned. Column offsets and the offset math are fixed in `shard.go`; no column is
compressed and no `ImageID` is written.

## Sharding and manifest

Shard files are capped (default 4 GiB). The shard index is derived arithmetically
from the hash page, so no shard directory is required:

```text
shardIndex = hashPage / pagesPerShard
```

Layout arithmetic is validated at `Open` and keeps `actualShardBytes <=
MaxShardBytes` for every supported `SeqBlockRecords` (256/512/1024/2048/4096).

Each shard carries a dual-slot CRC32C superblock: shard index, first page, hash
page count, committed count, and generation. A store-level `manifest` records
the format geometry (`pagesPerShard`, `SeqBlockRecords`), the committed
high-water mark, the **authoritative committed generation**, and the shard
count.

`Open` validates the manifest before using any of its geometry and returns
`ErrCorrupt` (never a panic) for:

- `pagesPerShard == 0` or an unsupported sequence block size;
- shard-size or capacity overflow, or a shard size above the maximum;
- a shard count inconsistent with the committed high-water mark
  (`required = 1 + (committed-1)/perShard`, so `committed == perShard` needs one
  shard, not two).

For each shard it selects the newest superblock whose generation was committed by
the manifest; a newer superblock is an interrupted commit and is ignored. A
shard that does not cover the committed records is rejected. Per-shard counts use
subtraction-based arithmetic (`committedInShard`) that is safe for `lastID` up to
`MaxUint64` and accounts for reserved `ImageID 0`.

## Concurrency

Many lock-free readers plus one serialized writer. Searches never take a mutex.

In-place point updates (no COW) use a per-block seqlock of `atomic.Uint64`
counters:

```text
writer:  seq++ (odd) -> store pHash/PDQ/quality -> seq++ (even)
reader:  a = waitSeqEven(seq); read tuple; b = load(seq); retry block if a != b
```

All column accesses are atomic, so the tuple is `-race` clean while the seqlock
detects a torn 32-byte PDQ. Quality stays a 1-byte column and is updated by an
aligned 8-byte atomic read-modify-write. A reader spins briefly
(`seqSpinLimit = 64`) before yielding, and retries only the affected block.

**Sequence counters are ephemeral runtime synchronization, not persistent
state.** They live in the mmap file but are meaningless across processes. `Open`
replays the WAL, then zeroes every counter before publishing the reader view, so
a crash that left a counter odd can never wedge a reader.

The committed high-water mark and the published generation are atomics on the
`Store`, separate from the immutable shard-set view. When an append creates a new
shard, the shard set is published before the committed mark advances; a reader
that observes a newer committed mark than its view clamps to the view's shard
capacity, so it scans a consistent older snapshot instead of a missing shard.

`Close` sets a closed flag, waits for in-flight readers (`ReaderTracker`), then
unmaps. Read APIs check the flag before and after acquiring a reader token, so a
read racing with `Close` never touches an unmapped arena. No `RWMutex` is used
around searches.

## Append versus update

- **AppendBatch** writes new, previously invisible slots. A crash cannot expose a
  torn record, so appends need **no WAL**. It preflights the required shard range
  arithmetically, writes contiguous page runs with hoisted column bases, updates
  per-shard committed counts, advances the generation, and publishes. A preflight
  failure leaves logical state unchanged; an already-created shard is retained as
  physical capacity, never a partial logical append.
- **Upsert / UpsertBatch** overwrite already-visible records and therefore use a
  **batch redo WAL**.

## Batch redo WAL

The WAL (`updates.wal`, built on `internal/storage`'s framed transport) is
batch-oriented. Protocol:

```text
1. serialize writer
2. validate the whole batch
3. encode one complete batch frame into reusable scratch
4. append it; fsync under DurabilitySync
5. seqlock odd -> apply all records (grouped by sequence block) -> seqlock even
6. advance and publish the generation once
7. Checkpoint/Sync makes canonical + manifest durable
8. only then truncate the WAL
```

Frame payload (fixed width): `baseGeneration`, `newGeneration`, `recordCount`,
then `recordCount` updates of `ImageID + pHash + PDQ + quality`, then a CRC32C.
One `UpsertBatch` is exactly one WAL frame and one logical generation. Duplicate
ImageIDs in a batch are allowed; the last update wins. Records are grouped into
consecutive runs that share a sequence block so the block's seqlock toggles once
per run; the batch is not sorted (sorting regresses random batches).

Recovery replays valid frames in order (idempotent) and repairs an interrupted
tail. It validates a single-step generation chain; a forward gap from
uncheckpointed appends is tolerated, and an update whose `ImageID` is beyond the
manifest high-water is dropped (its append was lost) rather than treated as
corruption. A permanently mixed pHash/PDQ/quality tuple is impossible for any
update whose WAL record survived.

## Durability and the commit boundary

Runtime atomicity and crash durability are separate. Under the default OS
durability, appends and updates publish in memory immediately; `Checkpoint`/`Sync`
makes them durable. Under `DurabilitySync` a commit is made durable before it is
published to readers.

The durable commit sequence is split at the manifest write, which is the commit
marker (`storage.WriteFileAtomic` fsyncs the temp file, renames, and fsyncs the
directory):

```text
PRE-COMMIT
  canonical mmap Sync
  -> write new shard superblocks
  -> shard mmap Sync
  -> atomically publish the manifest
COMMITTED
POST-COMMIT
  -> reset the WAL
```

- A failure before or at the manifest write returns an error and, under
  `DurabilitySync`, rolls back the unpublished `AppendBatch` writer state;
  readers never observe the new IDs.
- A failure after the manifest write (WAL reset) leaves the commit in place and
  returns the error; the stale WAL is skipped (already-committed batches) or
  repaired on the next `Open`.

`Close` does not checkpoint, matching `index/radixhash`; explicit
`Sync`/`Checkpoint` is required for durability. On `Open`, the manifest
generation is the durable checkpoint floor and a validated WAL chain may advance
the runtime generation beyond it (manifest `G` with WAL `G→G+1`, `G+1→G+2` opens
at `G+2`).

## Search

Exactly two radius queries are supported:

- `SearchPHash(dst, query uint64, maxDistance uint8)` — 64-bit Hamming distance.
- `SearchPDQ(dst, query *PDQHash, maxDistance uint8)` — 256-bit distance,
  evaluated independently of pHash.

Both take a caller-owned destination slice and a single result type:

```go
type Result struct {
    ImageID  common.ImageID
    Distance uint8
}
```

`Distance` is the exact Hamming distance and the only similarity score. There is
no `Score`, `Similarity`, `NormalizedDistance`, `HashType`, or `Quality` field,
and no `CombinedMatch`, `SearchCombined`, `Order`, `PHashFirst`, `PDQFirst`,
`PHashMatch`, or `PDQMatch`. SimHash is not a generalized query engine: no
Top-K, `CandidateSet`, count-only API, sorting, heap, ranking workspace, or
temporary result arrays. Higher-level code may sort by `Distance`.

Matches are emitted in ascending `ImageID` order (natural scan order); they are
not sorted by distance. The comparison is inclusive.

pHash distance is exactly 0..64. PDQ distance is mathematically 0..256, so the
supported search threshold is 0..255 and `DistancePDQ` stays `uint16`. The exact
distance is never narrowed to `uint8` before the threshold test, so a pair 256
bits apart is excluded by every threshold instead of wrapping to 0.

A search samples an immutable shard-set view, the committed high-water at that
moment, and **per-sequence-block** tuple consistency. It does **not** provide
whole-generation MVCC snapshot isolation; a concurrent writer may leave different
sequence blocks at different points in time. This is intentional.

PDQ distance uses word-at-a-time early rejection for low thresholds and a
two-word prefix for the mid range (`20 < d < 40`) where a single-word reject
branch is ~50/50 and mispredicts. The concurrent-safe searches use atomic scalar
loads; `SearchPDQBulk` uses the SIMD block kernel on plain memory and is
**quiescent-only** (no concurrent writer). On a quiescent store it returns
exactly the same ImageIDs, distances, and order as `SearchPDQ`.

## Kernels

`internal/hashkernel` holds the kernels behind one dispatch point, selected at
compile time with no interfaces or per-loop dispatch:

- **Scalar reference** (`math/bits`; POPCNT on amd64) — the correctness oracle
  and the concurrent-safe implementation.
- **NEON (arm64)** — two 16-byte vectors per record, `EOR` + `CNT`, reduced with
  `VUADDLV`, with a 16-byte early reject.
- **AVX2 (amd64)** — Option B (vector XOR of the 32-byte record + scalar
  POPCNT), runtime-detected with `cpu.X86.HasAVX2`. Implemented and
  differential-tested but **not enabled in the default dispatch** until an amd64
  benchmark shows it beats four scalar XOR+POPCNT operations end-to-end.

SIMD kernels produce a 64-bit match mask for up to 64 records and receive an
explicit count, so they never read past the committed high-water, page end, or
shard end. pHash has no SIMD kernel: one XOR plus hardware POPCNT is already the
fast path. AVX-512 is out of scope.

## Allocation budget

Steady-state targets, guarded by `testing.AllocsPerRun`:

```text
Get, Quality                       0 B/op   0 allocs/op
SearchPHash, SearchPDQ,
SearchPDQBulk                       0 B/op   0 allocs/op  (caller capacity)
Append, AppendBatch/64             0 B/op   0 allocs/op
Upsert, UpsertBatch/64             0 B/op   0 allocs/op
```

The canonical data never touches the Go heap; it lives in the mmap arena. The
WAL frame is encoded directly into reusable scratch and decoded by value during
recovery. The only regular allocation on the write path is one small `view` per
published append batch.

## Testing

Oracle-based exactness for pHash and PDQ radius queries (including the inclusive
boundary thresholds and the PDQ distance-254/255/256 regression against
`uint16 -> uint8` wrapping), page/shard boundaries, tombstones (external, via
filtering), quality persistence, reopen and generation selection, arithmetic
boundaries including `MaxUint64`, corrupt manifest geometry, journal recovery at
every stage, persistence failure injection at every commit stage, stale WAL
after commit, batch atomic recovery, closed-store safety and Close-vs-reader
stress, randomized differential tests against brute-force Hamming distance for
both searches, and `SearchPDQ == SearchPDQBulk` on quiescent stores. All pass
under `-race`.

Benchmarks and measured results live in `index/simhash/README.md`.
