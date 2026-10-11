# geoidx size-bounded sharding

Design and results for replacing the single monolithic base with independently
compactable, size-bounded shards. The flat SoA layout, public search API, and
lock-free reader model are unchanged.

## Architecture

```
geoidx
  directory (sorted []*shard, tens of entries)
    shard 0  keys[] ids[]   (immutable, contiguous (S2Key, ImageID) interval)
    shard 1  keys[] ids[]
    ...
  global mutation overlay (active/frozen/layers, id-sorted)
```

- **Shard.** Immutable SoA arrays ordered by `(S2Key, ImageID)`. For a persistent
  index they view an mmap arena; for a memory index they own heap arrays. A shard
  is never mutated: compaction replaces it wholesale.
- **Directory.** The ordered shard slice. Boundaries are `(S2Key, ImageID)`
  pairs, chosen by payload size (16 B/record), not by an S2 prefix, so a repeated
  S2Key may span shards. Lookup is a binary search over the directory
  (`shardFor`) followed by an in-shard binary search; a range continues into the
  next shard rather than restarting.
- **Mutation overlay.** One global, id-sorted delta shared by all shards. It is
  ImageID-authoritative, so a delete or move suppresses the old record even when
  the caller supplies a stale key; no reverse index exists.

## Configuration

| field | default | meaning |
|---|---|---|
| `TargetShardBytes` | 16 MiB | preferred shard payload; split/merge target |
| `MaxShardBytes` | 32 MiB | payload above which a rebuilt shard is split |
| `MinShardBytes` | 4 MiB | payload below which a shard is a merge candidate |

8/16/32 MiB targets were benchmarked (below). For small test datasets the same
code is exercised with proportionally tiny targets.

## Compaction

`prepare → commit → publish → reclaim`, unchanged in shape from the monolithic
design but now selective:

1. **prepare (off-lock).** Route each insertion to its shard by `(key, id)`
   (`partitionAdds`). A shard is *affected* if it receives an insertion or if any
   of its base ImageIDs is named by the delta — the latter is how a delete/move
   reaches a shard it does not name by key. Unaffected shards are kept by
   reference. Each affected shard is rebuilt with the shared streaming
   `exactCount`/`mergeInto`. A result larger than `MaxShardBytes` is split near
   the median on `(key, id)`; adjacent underutilized plans are coalesced while
   their combined payload fits `TargetShardBytes` and one is below
   `MinShardBytes`, so split/merge cannot cycle. An emptied shard leaves the
   directory.
2. **commit (locked).** Publish the manifest: the durable linearization point.
3. **publish (locked).** Swap in the new directory.
4. **reclaim (off-lock).** Drain readers, unmap retired shards, remove their
   files.

The trigger is `max(CompactionTrigger, baseEntries/8)` over the whole directory,
so the per-mutation rewrite ratio is the same as before; sharding bounds the size
of each individual rewrite and lets unrelated shards stay untouched.

Only affected shards are rewritten, so a small update costs one (sometimes two)
shard rewrites instead of a full-base rewrite.

## Persistence and recovery

- Each shard is a generation-numbered mmap file `shard.<gen>.dat` with a dual-slot
  CRC32C superblock (magic `GEOS`, count, max ImageID, key/id CRCs).
- The manifest (magic `GEOM`, **version 2**) holds a directory generation and one
  entry per live shard: generation, record count, first `(S2Key, ImageID)`.
  Published atomically with `WriteFileAtomic`; not replaced until all new shard
  files are written and synced.
- `Open` validates the manifest, opens and validates each shard (superblock,
  count, boundary, checksums, strict `(key, id)` order), validates global shard
  ordering, reconstructs the reader view, and removes orphan shard files.
- A **version 1** (pre-shard monolithic) manifest is rejected with
  `ErrLegacyFormat`; it is not reinterpreted. Rebuild by re-inserting from
  canonical image metadata, then `Compact`.
- Commit boundary: a failure before the manifest commit discards the new shard
  files and retains the frozen snapshot for retry; a manifest that committed
  despite a reported sync error is reconciled by re-reading it.

## Benchmarks

Apple M4 Pro, Go 1.27. Query results are identical to the unsharded PR #15
numbers (single-shard at these dataset sizes), so sharding adds no read cost.

Regression check (1M / 200k, one shard): exact cell 29.5 ns / 0 allocs; 2 km
multi-range 5.35 µs / 0; nearest k=10 86.7 µs; filter initial 5.45 µs / 0;
clustered range 50.8 µs.

`BenchmarkShardTargetSize`, 4M uniform (`-benchtime=100ms`, `-count=1`):

| target | shards | range | exact | nearest10 | compact-small-update |
|---|---:|---:|---:|---:|---:|
| single (1 GiB) | 1 | 276 µs | 34.8 ns | 56.7 µs | 46.6 ms / **64.3 MB** |
| 8 MiB | 8 | 281 µs | 32.9 ns | 57.7 µs | 22.9 ms / **8.3 MB** |
| 16 MiB | 4 | 276 µs | 33.6 ns | 58.1 µs | 26.2 ms / **16.3 MB** |
| 32 MiB | 2 | 277 µs | 35.2 ns | 61.1 µs | 33.9 ms / **32.3 MB** |

The query columns are flat across shard sizes. `compact-small-update` deletes and
re-inserts one image and calls `Compact`: its transient allocation tracks the
rewritten shard size (8/16/32 MB) instead of the whole 64 MB base, which is the
core result — **the maximum individual rewrite is bounded by `MaxShardBytes`.**

Small-batch writes (`BenchmarkShardInsertBatch`, 1M base, 64-entry batches):
8.5–9.1 µs, ~35 KB, allocations independent of the shard target.
`BenchmarkGeoInsertRotation`: 3.1 µs / 20.5 KB / 4 allocs (trigger=1M), 9.9 µs /
30.4 KB / 4 allocs (trigger=256).

## Correctness tests

`shard_test.go` covers: directory ordering; lookup boundaries (`shardFor`,
`firstShardAtOrAfter`); a repeated S2Key spanning shards; ranges crossing
multiple shards (differential against a reference, plus Collect/Filter);
split and merge; empty-shard removal; selective compaction rewriting only
affected shards; stale-key deletion across shards; multi-shard moves with no
duplicate live ImageID; persistent round-trip with per-shard files; legacy
manifest rejection; orphan cleanup; mixed insert/delete/move differential under
frequent compaction; and concurrent lock-free readers across shard replacement
(`-race`). Existing compaction failure-injection, slow-reader, and reconciliation
tests exercise the sharded commit path.

```
go test ./...
go test -race ./index/geoidx/... ./search/...
go vet ./index/geoidx/... ./search/...
```

## Trade-offs and limitations

- **Affected-shard selection is O(base) per compaction.** Adds are key-routed,
  but an ImageID-authoritative delete/move is not, so compaction scans the base
  ImageIDs against the delta membership set (O(1) per id, no allocation) to find
  affected shards. This is far cheaper than rewriting every shard and needs no
  reverse index, but it is linear in the base. It is the price of the global
  overlay and is the main remaining optimization target (for example a coarse
  per-shard ImageID Bloom summary).
- **Write amplification per mutation is unchanged** (`~128 B` at the `base/8`
  trigger). Sharding reduces the size of each rewrite and enables independent
  scheduling; it does not reduce the amortized ratio.
- **Reads are unaffected** at these sizes; the directory adds `O(log shards)`
  per range.
- **Merging can rewrite an unaffected shard** when two adjacent undersized shards
  coalesce. Hysteresis (merge only when combined ≤ target and one < min) prevents
  cycling.
- **20M/30M datasets were not run** in this environment; the design is
  size-independent and 4M was used to get multiple shards at the target sizes.
  The 1M/10M query benchmarks and 4M shard-target benchmark cover the hot paths.
- **No new SIMD.** The scan remains memory-bandwidth-bound.

## Not done (scope)

Page-level COW, multi-level radix directories, fixed-prefix S2 sharding, per-cell
postings, per-shard roaring postings, an ImageID reverse index, a new allocator,
and a general LSM framework remain out of scope.
