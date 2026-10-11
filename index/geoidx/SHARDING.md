# geoidx size-bounded sharding (single-file)

Design and results for size-bounded shards stored in **one file**, `geoidx.dat`.
Flat SoA, the public search API, and lock-free readers are unchanged.

## Architecture

```
geoidx.dat (one sparse mmap, fixed capacity)
  slot 0  superblock (512 B)     \  dual-slot CRC32C commit point
  slot 1  superblock (512 B)     /
  arena (pagealloc, 512 B granularity)
    shard extent 0  keys[] ids[]
    shard extent 1  keys[] ids[]
    ...
    directory extent (shard references)
    allocator metadata extent
  global mutation overlay (active/frozen/layers, id-sorted, runtime only)
```

- **Shard.** One contiguous `(S2Key, ImageID)` interval of flat SoA arrays, held
  as one independently replaceable extent. Boundaries are chosen by payload size,
  not an S2 prefix, so a repeated S2Key may span shards.
- **In-file directory.** A compact sorted array of shard references:
  `firstKey, firstID, offset, count, keyCRC, idCRC, minID, maxID`. A geographic
  lookup binary-searches the directory and then the shard; it visits every shard
  whose key range overlaps.
- **Global mutation overlay.** Unchanged: ImageID-authoritative, so stale-key
  deletes and moves are correct with no reverse index.
- **One mapping.** The arena is mapped once at a fixed capacity
  (`Config.MaxFileBytes`, default 1 GiB) and never re-mapped while the index is
  open, so reader slices into it stay valid. Free space is reused through
  compaction rather than growing the file.

## Extent allocator

Reuses `internal/pagealloc` (the same allocator `radixhash` uses):
512-byte-granular, variable-sized exact-fit extents, a running allocation cursor,
free-space reuse, and `Retire`/`Reclaim` with a copy-on-write grace period. A
shard extent is simply a large exact-fit allocation (16–32 MiB), so no geoidx
specific allocator exists. Added to the shared allocator: `EncodedSize` (reserve
the metadata extent before encoding), `LargestFreeBytes`, `FreeRunCount`, and
`Used` (validate a persisted reference on reopen).

## Commit and persistence

Two superblock slots alternate; the highest valid generation wins on open. The
superblock payload stores: directory offset/count, allocator metadata
offset/length, total live entries, live bytes, and arena bytes; the header
carries magic, version, generation, and CRCs.

Commit sequence (unchanged in shape, now in-file):

1. Allocate replacement shard extents.
2. Write new shard contents.
3. Write the new directory extent.
4. Write the new allocator metadata extent (a fresh bitmap marking only the
   second superblock and the new live extents used, so retired and uncommitted
   extents are free on reopen).
5. Sync the arena data.
6. Commit the inactive superblock slot.
7. Sync.
8. Publish the new in-memory directory.
9. Retire the replaced extents.
10. Reclaim them after prior readers drain (`ReaderTracker`).

A failure before the superblock commit frees the newly allocated extents and
retains the frozen snapshot for retry; the old slot stays authoritative. A commit
that wrote but reported a sync error is reconciled by re-reading the slots.
Extents referenced by a committed generation are never overwritten.

## Compaction

`prepare → commit → publish → reclaim`. Only affected shards are rebuilt:

1. Insertions are routed to a shard by `(key, id)`.
2. A shard is also affected when any of its base ImageIDs is named by the delta
   (the path by which a stale-key delete or move reaches it). Each shard carries
   conservative `MinImageID`/`MaxImageID` bounds, so the delta's ImageID range
   skips shards that cannot contain a named ID.
3. Rebuilt shards above `MaxShardBytes` split near the median; adjacent
   underutilized plans coalesce with hysteresis; emptied shards leave the
   directory.
4. Unchanged shard extents are kept by reference and never rewritten.

## Configuration

| field | default | meaning |
|---|---|---|
| `TargetShardBytes` | 16 MiB | preferred shard payload; split/merge target |
| `MaxShardBytes` | 32 MiB | payload above which a rebuilt shard is split |
| `MinShardBytes` | 4 MiB | payload below which a shard is a merge candidate |
| `MaxFileBytes` | 1 GiB | fixed single-file capacity, created sparse |

8/16/32 MiB targets were benchmarked (below). `MaxFileBytes` is fixed at
creation; changing capacity requires rebuilding the index.

## Results (Apple M4 Pro)

Query latency is unchanged from the unsharded PR #15 (single-shard at these
dataset sizes): exact cell 31.5 ns / 0 allocs; 2 km multi-range 5.34 µs / 0;
nearest k=10 86.4 µs; filter initial 5.36 µs / 0; clustered range 51.5 µs.

`BenchmarkShardTargetSize`, 4M uniform (memory, `-benchtime=100ms`):

| target | shards | range | exact | compact-small-update |
|---|---:|---:|---:|---:|
| single | 1 | 276 µs | 34.8 ns | 46.6 ms / **64.3 MB** |
| 8 MiB | 8 | 281 µs | 32.9 ns | 22.9 ms / **8.3 MB** |
| 16 MiB | 4 | 276 µs | 33.6 ns | 26.2 ms / **16.3 MB** |
| 32 MiB | 2 | 277 µs | 35.2 ns | 33.9 ms / **32.3 MB** |

The maximum individual rewrite is bounded by `MaxShardBytes`.

`BenchmarkAffectedShardSelection` isolates the affected-shard scan (one delete on
a 1M memory index, 62 shards, no disk I/O):

| distribution | ns/op | shards scanned | shards total |
|---|---:|---:|---:|
| monotonic ImageIDs | 70.8 µs | 1 | 62 |
| clustered/random IDs | 5.14 ms | 62 | 62 |

Monotonic/geographically clustered IDs let `MinImageID`/`MaxImageID` prune the
scan by **73×**; overlapping ranges fall back to a full scan (still allocation
free, and far cheaper than rewriting every shard).

Persistent 1M uniform (one file): live payload 16 MB (16 B/image), arena 1 GiB
fixed, 2 free runs, ~1.055 GB free — the file is a preallocated sparse arena
reused across compactions.

## Metrics

`Index.StorageStats()` reports `ArenaBytes`, `LiveBytes`, `FreeBytes`,
`LargestFree`, `FreeRuns`, and `RetiredBytes`. The measured 1M index shows low
fragmentation (2 free runs after a bulk load); `TestSingleFileExtentReuse`
churns 400 single-image updates in a 1 MiB arena and confirms replaced extents
are reused and no sidecar file is created. The arena never grows during ordinary
uploads, so whole-file defragmentation is not needed.

`BenchmarkSingleFileCompact` (persistent, 500k images, 96 MiB fixed arena, one
update + `Compact`): a 1 MiB shard target rewrites 1.2 MB/op with 5 free runs
(largest 79 MB); a 4 MiB target rewrites 4.5 MB/op with 3 free runs (largest
59 MB). Free space is reused rather than the file growing.

## Correctness tests

`shard_test.go` / `persist_test.go` cover: directory ordering and boundaries; a
repeated S2Key spanning shards; cross-shard ranges differentially against a
reference (plus Collect/Filter); split and merge; empty-shard removal; selective
compaction rewriting only affected shards; affected-shard pruning correctness
with a stale-key delete; stale-key deletes; multi-shard moves without duplicate
live ImageIDs; single-file layout with no sidecar; extent reuse in a small arena;
persistent round-trip; legacy-format rejection; malformed superblock / shard /
directory / metadata / truncated file rejection; compaction failure injection at
every stage with reopen; commit reconciliation after a post-write sync error;
reader safety while a slow reader holds the old directory; concurrent readers
across shard replacement and on a persistent single-file index (`-race`); and a
mixed insert/delete/move differential under frequent compaction.

```
go test ./...
go test -race ./index/geoidx/... ./search/...
go vet ./index/geoidx/... ./search/...
```

## Migration from earlier formats

- Pre-shard monolithic (`base.<gen>.dat` + v1 manifest) and the multi-file
  sharded format (`manifest` + `shard.<gen>.dat`) are both rejected with
  `ErrLegacyFormat`; neither is read implicitly. Rebuild by opening a new index
  and re-inserting from canonical image metadata (`KeyFromLatLng` + `InsertBatch`
  + `Compact`).
- The single-file format is new; there is no in-place upgrade.

## Trade-offs and limitations

- **Affected-shard selection is O(base) in the worst case.** Adds are key-routed;
  a delete/move is bounded by `MinImageID`/`MaxImageID` pruning, but when ID
  ranges overlap heavily the scan is still linear in the base. It allocates
  nothing and is much cheaper than a full rewrite.
- **Fixed capacity.** `MaxFileBytes` is set at creation; the file is sparse and
  reused, so it does not grow during uploads, but exceeding capacity returns an
  error and requires a rebuild with a larger capacity. Growth by re-mapping is
  intentionally avoided because it would invalidate reader slices.
- **Write amplification per mutation is unchanged** (~128 B at the `base/8`
  trigger). Sharding bounds the size of each rewrite, not the amortized ratio.
- **Checksums validate the whole base on open** (keys and ids per shard), so open
  is O(base) read; this trades startup time for corruption detection.
- 20M/30M datasets were not run here; 4M/1M were used to exercise multiple shards
  at the target sizes. No SIMD or extra index levels were added.
