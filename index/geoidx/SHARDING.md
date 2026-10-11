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
authoritative; the frozen snapshot is retained for retry. A write that committed
but reported a sync error is reconciled by re-reading the slots. The newest slot
is validated end to end (directory bounds, ordering, checksums); if it fails, an
older valid generation is selected instead.

## Safe mmap growth

The arena is mapped at a fixed capacity (`MaxFileBytes`). When it must grow, the
file is extended with `os.Truncate`, a **new** mapping is created (the old one is
never re-mapped), and the allocator is rebuilt over the larger arena with the
current live set. The old mapping is appended to a retired list and unmapped only
after `WaitForPriorReaders` and only when no published shard references it. A
growth marks the directory for a one-time full migration so the old mapping is
released on the next commit, bounding mapping accumulation.

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
  every compaction failure stage (allocate, grow, mmap, write, directory, data
  sync, commit, superblock sync, publish, retire, reclaim, remove) with reopen;
  **fallback** to the previous generation when the newest is corrupt.
- Growth: forced growth with four concurrent readers (`-race`); injected file
  extension and mapping failures with retry; growth reuses/releases mappings.
- Crash: subprocess hard-exit at the commit and superblock-sync boundaries, then
  reopen and read.
- Stress: 600 repeated shard replacements reach a steady-state arena (no
  unbounded growth).

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
- **Growth is a new full-file mapping** (old mapping retained), not windowed
  mapping; growth is rare and followed by a full migration.
- **Per-mutation write amplification** (~128 B at `base/8`) is unchanged; sharding
  bounds each rewrite.
- **Checksums validate the whole base on open**, so open is O(base) read.
- 20M/30M datasets were not run here; 4M/1M exercised multiple shards and growth.
