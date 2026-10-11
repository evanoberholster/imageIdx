# geoidx size-bounded sharding (single-file, crash-safe)

Design and results for size-bounded shards stored in **one file**, `geoidx.dat`,
with generation-aware, crash-safe copy-on-write. Flat SoA, the public search API,
and lock-free readers are unchanged.

## Architecture

```
geoidx.dat (one mmap; grown by adding mappings, never re-mapped in place)
  slot 0  superblock (512 B)     \  dual-slot CRC32C commit point
  slot 1  superblock (512 B)     /
  arena (pagealloc, 512 B granularity)
    shard extent 0  keys[] ids[]
    shard extent 1  keys[] ids[]
    ...
    directory extent (shard references)
  global mutation overlay (active/frozen/layers, id-sorted, runtime only)
```

- **Shard.** One contiguous `(S2Key, ImageID)` interval of flat SoA arrays held as
  one independently replaceable extent. Boundaries are chosen by payload size, not
  an S2 prefix, so a repeated S2Key may span shards.
- **Directory.** A compact sorted in-file array of
  `firstKey, firstID, offset, count, keyCRC, idCRC, minID, maxID`. A lookup
  binary-searches it and visits every shard whose key range overlaps.
- **Global overlay.** ImageID-authoritative, so stale-key deletes and moves are
  correct with no reverse index.

## Extent ownership and reclamation

Every extent is `FREE → ALLOCATED_UNCOMMITTED → COMMITTED_LIVE → RETIRED → FREE`.

The authoritative live set is `committed directory + committed metadata roots`.
The implementation **reconstructs** the free-space map from the two recoverable
generations (Option B), so there is no separate transactional free-space
database and no stale in-memory free list survives a reopen:

- A new generation is committed by writing the inactive superblock slot. Before
  that, its extents are `ALLOCATED_UNCOMMITTED`; a crash leaves them unreferenced
  and therefore free on recovery.
- The **previous** committed generation is retained: the durable live set is the
  union of the newest committed generation and the one before it. Only when a
  generation's slot is overwritten does its extent set become reusable. This is
  what makes fallback to the previous superblock safe.
- Readers are protected separately: replaced extents are only marked reusable
  after `ReaderTracker.WaitForPriorReaders` drains readers of the prior view, and
  superseded mappings are unmapped only then and only when no published shard
  references them.

## Reconstructible allocator (Option B)

`allocatorFromLive(arenaBytes, live)` builds the allocator by marking the second
superblock and every live extent used; everything else is free. It is called:

- at commit, with the union of the new generation and the previous one;
- at open, with the union of the selected and next-older valid generations.

Overlapping live extents fail `Reserve`, so a corrupt directory cannot produce
overlapping live extents. `StorageStats` exposes the resulting fragmentation.

## Commit sequence

1. Allocate replacement extents.
2. Write replacement shards.
3. Write the new directory extent.
4. Sync data.
5. Write the inactive superblock slot (the durable commit point).
6. Sync.
7. Publish the new in-memory directory.
8. Retire replaced extents (inside the allocator rebuild).
9. Wait for prior readers.
10. Release superseded mappings.

A failure before step 5 frees the new extents and keeps the previous generation
authoritative; the frozen snapshot is retained for retry. The commit reports one
of three outcomes:

- **aborted** — no bytes of the slot were modified (an encoding failure or a
  pre-write check); the new extents are rolled back.
- **durable** — the slot is written and synchronized; the new generation is
  published.
- **uncertain** — the slot may have been modified but durability is unconfirmed (a
  write error, including a partial write, or a sync error). Durability is never
  inferred from mmap contents. The index keeps the previous generation
  authoritative in memory, protects both generations' extents (plus any prior
  uncertain extents) so none is reused, and returns the error so the caller can
  retry. The retry overwrites the same inactive slot, resolving the uncertainty.

The newest slot is validated end to end (directory bounds, ordering, checksums)
on open; if it fails, an older valid generation is selected instead. Publication,
retirement, and reclamation cannot fail; they expose test-only crash points (not
ignored error hooks) so termination between the durable commit and in-memory
publication can be exercised.

## Safe mmap growth

The arena is mapped at a fixed capacity (`MaxFileBytes`). When it must grow, the
file is extended with `os.Truncate`, a **new** mapping is created (the old one is
never re-mapped), and the allocator is rebuilt over the larger arena with the
current durable live set — **both recoverable generations are preserved**, so no
committed extent is freed by growth. The rebuild happens before the new mapping
is swapped in, so a failed growth leaves the mapping and allocator consistent and
retryable. The old mapping is appended to a retired list and unmapped only after
`WaitForPriorReaders` and only when no published shard references it. A growth
marks the directory for a one-time full migration so the old mapping is released
on the next commit, bounding mapping accumulation.

Growth is the one case where the whole index is rewritten (the migration), so it
temporarily raises write amplification. Measured (50k seed, 400k batch, 4 MiB
arena): selective compaction keeps the arena at 4 MiB with 800 KB live; the growth
migration rewrites a full copy (live delta 6.4 MB) into a 16 MiB arena and releases
the old mapping (one mapping retained). These are logical/allocated bytes, not
physical device writes.

## Extent identity

`allocatorFromLive` reserves every live extent; the shared allocator rejects
overlapping runs, and `dedupExtents` rejects a shared offset with mismatched
lengths while still allowing two generations to share an identical extent.
Offsets must be 512-aligned, extents must be in bounds (`offset + length` is
checked without overflow), and the reserved superblock region is rejected.
Corrupt directories, unaligned offsets, and out-of-bounds offsets fail recovery
and fall back to the previous generation.

## Compaction and pruning

Only affected shards are rebuilt: inserts route by `(key, id)`, and a shard is
affected when a base ID is named by the delta (stale-key deletes/moves). Shards
carry conservative `MinImageID`/`MaxImageID` bounds so the delta's ImageID range
prunes the affected-shard scan. Rebuilt shards above `MaxShardBytes` split;
adjacent underutilized plans coalesce; emptied shards leave the directory.

## Results (Apple M4 Pro)

Query latency is unchanged from the unsharded PR #15: exact cell 31.6 ns / 0
allocs; 2 km multi-range 5.48 µs / 0; nearest k=10 87.7 µs; filter initial
5.42 µs / 0; clustered range 50.9 µs.

`BenchmarkAffectedShardSelection` (one delete on a 1M memory index, 62 shards):

| distribution | ns/op | shards scanned |
|---|---:|---:|
| monotonic ImageIDs | 81 µs | 1 |
| clustered/random IDs | 5.39 ms | 62 |

`BenchmarkSingleFileCompact` (persistent, 500k, fixed arena, one update +
`Compact`): 1 MiB target rewrites 1.18 MB/op (7 free runs, largest 75 MB); 4 MiB
target 4.46 MB/op (3 runs, largest 67 MB). Reused, not grown.

## Correctness tests

- Single-file layout with no sidecar; extent reuse; allocator reconstruction
  equals the pre-close accounting.
- Directory ordering/boundaries; repeated key spanning shards; cross-shard ranges
  differential vs a reference; split/merge; empty-shard removal; selective
  compaction; prune correctness with stale-key deletes; multi-shard moves.
- Corruption: malformed superblock, shard extent, directory, truncated file;
  unaligned and out-of-bounds extents; extent-identity rejection (same offset with
  mismatched length, partial overlap, zero length, superblock overlap); every
  compaction failure stage (allocate, grow, mmap, write, directory, data sync,
  commit, superblock sync, publish, retire, reclaim, remove) with reopen;
  **fallback** to the previous generation when the newest is corrupt.
- Commit-state injection: before slot write, during slot write (partial and
  full-then-error), before sync, during sync, and after sync/before publish, each
  with retry and reopen.
- Differential batches of 1/8/32/64 plus moves and duplicate ids checked against a
  reference for exact membership, ascending order, ImageID uniqueness, and
  `(S2Key, ImageID)` directory ordering after every commit.
- Growth: forced growth with four concurrent readers (`-race`); injected file
  extension and mapping failures with retry; second-generation crash during growth
  recovered by subprocess; growth reuses/releases mappings.
- Crash: subprocess hard-exit at the commit, superblock-sync, and
  pre-publication boundaries, then reopen and read.
- Stress: 600 repeated shard replacements reach a steady-state arena; concurrent
  searches with repeated replacement, forced growth, and shutdown (`-race`).

```
go test ./...
go test -race ./index/geoidx/... ./search/...
go vet ./index/geoidx/... ./search/...
```

## Migration

Pre-shard monolithic, multi-file sharded, and the earlier single-file v1 layout
are all rejected with `ErrLegacyFormat`; rebuild by re-inserting from canonical
image metadata.

## Trade-offs and limitations

- **Two-generation retention** keeps roughly one extra generation's extents live,
  trading space for crash fallback safety.
- **Affected-shard selection is O(base)** when ImageID ranges overlap heavily; it
  allocates nothing and is far cheaper than a rewrite.
- **Growth requires a full migration.** Single-file sharding does *not* eliminate
  all full-index rewrites: growth extends the file and rewrites every shard into
  the new arena once, then releases the old mapping. Routine compaction remains
  selective. Bounded mmap windows are a future option only if benchmarks justify
  them.
- **Per-mutation write amplification** (~128 B at `base/8`) is unchanged; sharding
  bounds each rewrite.
- **Checksums validate the whole base on open**, so open is O(base) read.
- 20M/30M datasets were not run here; 4M/1M exercised multiple shards and growth.
