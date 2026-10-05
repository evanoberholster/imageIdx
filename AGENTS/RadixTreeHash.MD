# Radix Tree Hash Index

## Goal

Build a persistent, mmap-oriented exact hash index for fixed-width **256-bit
hashes** such as BLAKE3 and SHA-256. Each index file contains only one hash
type and maps a hash to a `uint64 IndexID`, while allowing duplicate hashes.
The design prioritizes SIMD-friendly lookup, lock-free readers, batched writes,
COW updates, low write amplification, lazy allocation, and ~4 GiB shard files
that can be searched in parallel.

The hash index and persistent space allocator must be separate packages.
Deletions are represented externally with a Roaring bitmap keyed by
`IndexID`; deleting an ID must not rewrite hash leaves.

This document describes a **multi-level, byte-oriented radix tree**. It
supersedes the earlier flat `uint16 Hash[0:2]` root design: the root is no
longer a preallocated 65,536-entry directory, and leaves no longer carry a
16-bit discriminator slot. The radix path itself is the coarse hash
discriminator.

---

## Packages

### `pagealloc`

A reusable persistent **512-byte-granular allocator** for mmap-backed files.
It knows nothing about hashes or radix trees.

Requirements:

- Minimum allocation block: **512 bytes**.
- Allocations occupy an exact number of contiguous 512-byte blocks:
  `ceil(size/512)`. There is no power-of-two class rounding, so a 5,808-byte
  leaf reserves 6,144 bytes rather than 8,192.
- Free space is tracked in one compact free bitmap; releasing adjacent extents
  makes them coalesce implicitly into one free run.
- Use block addresses rather than Go pointers; all persistent references
  remain valid across mmap reopen.
- Support delayed reclamation for COW: replaced blocks are `Retire`d with the
  last generation that referenced them and become reusable only after no
  reader can reference that generation.
- `Free` returns a never-published extent immediately (batch rollback).
- Allocator metadata is recoverable/checkpointable with magic, version, and
  CRC32, without a general-purpose heap format.
- Radix nodes require exactly **1024 bytes = 2 × 512-byte blocks**. Because
  every node requests the same size, nodes remain 1024-byte aligned in
  practice; the allocator itself only guarantees 512-byte alignment.

Suggested API:

```go
type Ref uint32

type Allocator interface {
    Alloc(size uint32) (Ref, uint32, error) // ref + actual capacity
    Free(ref Ref, size uint32) error        // rollback of a never-published extent
    Retire(ref Ref, size uint32, generation uint64) error
    Reclaim(oldestReaderGeneration uint64) error
    FreeBytes() uint64
}
```

Keep `pagealloc` free of hash-index-specific logic.

### `radixhash`

Generic persistent byte-oriented radix-tree/hash-index engine for exactly
**32-byte hashes**.

Public logical mapping:

```text
[32]byte -> uint64 IndexID
```

Duplicate hashes are allowed, so lookup must be capable of returning multiple
live `IndexID`s for an identical hash.

### `hashidx`

Public integration layer. It owns index lifecycle, sharding, batching, WAL
policy, tombstones, and hash-type-specific wrappers while delegating tree
storage/search to `radixhash` and allocation to `pagealloc`.

Provide separate index files/shard sets for each algorithm, e.g. BLAKE3 and
SHA-256. Do not mix algorithms within a file.

Potential API shape:

```go
type Hash [32]byte

type Index interface {
    Lookup(hash Hash) ([]uint64, error)
    InsertBatch(entries []Entry) error
    Delete(indexID uint64) error
    Close() error
}

type Entry struct {
    Hash    Hash
    IndexID uint64
}
```

Algorithm-specific wrappers may provide stronger types such as `Blake3Index`
and `SHA256Index` while sharing the same persistent engine.

---

## Radix Structure

Use **one hash byte per radix level**:

```text
Hash[0] -> L0 radix
Hash[1] -> L1 radix
Hash[2] -> optional L2 radix
...
        -> Leaf
```

Each radix node has exactly 256 entries:

```go
type RadixNode [256]uint32
```

Each `uint32` is a page/block reference managed by `pagealloc`. `0` means the
child does not exist. Because 512-byte allocation alignment leaves the low
bits of a page reference free, the reference is encoded as:

```text
child = (pagealloc Ref << 1) | kind
kind  = 0 for a leaf, 1 for a radix node
```

The low-bit tag is what lets lookup distinguish a leaf from another radix
level without a separate header probe. `0` remains the empty child.

A radix node therefore occupies exactly **1024 bytes** and naturally fits two
512-byte allocator blocks.

Do **not** implement a general-purpose Adaptive Radix Tree (ART), compressed
prefixes, Node4/16/48/256 types, or variable fanout. Cryptographic hashes are
uniformly distributed, so use only fixed 256-way radix nodes. Do not add
SwissTable control bytes, Bloom filters, Cuckoo filters, or MIH to the
exact-hash lookup path.

---

## Initial Tree

Do **not** preallocate a flat 65,536-entry directory.

- The initial root is one 256-entry radix node indexed by `hash[0]`.
- Radix nodes are allocated lazily and only where data requires them.
- A small `Hash[0]` bucket is a **direct root-to-leaf child**; it does not
  allocate a `Hash[1]` node up front.

```text
Root[hash[0]]
       |
       v
     Leaf              // sparse bucket
```

When that leaf exceeds `PromotionBytes` or `MaxLeafSlots` it is promoted to a
radix node and its records are redistributed, recursively, so the shape grows to
`Root -> Radix -> Leaf` and beyond only when it pays off. There is no fixed
L0/L1/L2 depth.

```text
Root[hash[0]]
       |
       v
Radix[hash[1]]
       |
       v
     Leaf
```

Root and interior nodes are stored in the pagealloc arena. Path copying
allocates a replacement node only when a node on an affected path changes, so
an insert that touches one leaf copies only the nodes on that leaf's path (and
one root node per publishing batch). The root itself is carried in the
published view, not the arena.

---

## Leaves

Leaves use a **Structure of Arrays** layout and store only the portion of the
hash not already represented by the radix path. For the normal two-level tree:

```text
Leaf
┌────────────────────────────────────┐
│ Header (magic, version, depth, ...) │
├────────────────────────────────────┤
│ HashTail[]  [][32-depth]byte        │
├────────────────────────────────────┤
│ IndexID[]   []uint64                │
└────────────────────────────────────┘
```

For the normal two-level tree (`depth == 2`):

```text
HashTail[i] = Hash[2:32]   // 30 bytes
IndexID[i]  = uint64
```

Maintain a strict one-to-one correspondence between `HashTail[i]` and
`IndexID[i]`. Optimize arrays for mmap access, cache locality, SIMD
comparisons, and minimal allocations.

Do **not** store the consumed radix prefix again in the leaf. The leaf header
must identify the radix **depth** so the implementation knows how many leading
hash bytes have already been consumed and how long each tail is.

Slots are sorted by `(HashTail, IndexID)` so equal hashes stay contiguous and
their IDs ascend. Support duplicate hashes: equal hashes may map to multiple
`IndexID`s, while each `(Hash, IndexID)` record remains distinct. Keep hash
tails contiguous and separate from `IndexID`s so equality comparisons do not
unnecessarily load IDs.

The leaf header records whether IDs are stored as 32-bit or 64-bit values. A
leaf whose IDs all fit in 32 bits stores them as `[]uint32`, which is the common
case for database-wide image IDs; any leaf with a larger ID falls back to
`[]uint64`. Lookup widens a 32-bit id to the public `uint64` identity.

---

## Adaptive Radix Promotion

Leaves must be capable of being promoted to another radix level when they
become too large.

```text
Before:
Hash[0]
  -> Hash[1]
       -> large leaf containing Hash[2:32]

After:
Hash[0]
  -> Hash[1]
       -> RadixNode indexed by Hash[2]
            -> smaller leaves containing Hash[3:32]
```

Do **not** create the third level globally. Only promote an individual leaf
when its encoded size or record count crosses a configurable threshold. Start
with a target promotion threshold around **64–128 KiB**, but benchmark the
threshold rather than treating it as a permanent format constant.

`Config.MaxLeafSlots` adds a record-count cap on top of the byte threshold so
scan and copy-on-write cost stay bounded regardless of id width. Benchmark it:
promoting too early scatters entries into minimum-size leaves and can multiply
space use, while the byte threshold alone is usually enough once lookup uses a
binary search over the sorted tails.

The implementation must permit additional radix levels using the same
mechanism if an unusually large leaf requires them (for example if many records
share long hash prefixes). Because each leaf header stores its depth, lookup
knows exactly how many bytes the radix path consumed.

Promotion is a COW operation like any other leaf rewrite:

1. Read the existing leaf.
2. Apply all inserts for that leaf.
3. If the result stays below the promotion threshold, allocate an
   appropriately sized replacement leaf.
4. If it exceeds the threshold, replace the leaf with a new 256-way radix node
   and distribute its records into child leaves (or deeper nodes when a child
   still exceeds the threshold).
5. Publish the new page references as part of the new generation.
6. Retire the replaced leaf and node pages through epoch reclamation.

---

## COW Behavior

Preserve the existing Copy-on-Write design.

- Batch insertions before modifying the tree.
- Group mutations by radix path so that each affected leaf is copied at most
  once per batch.
- Build replacement leaves and nodes **without** modifying pages visible to an
  existing mmap reader.
- Build every replacement in a first phase; only then publish the new root and
  retire the replaced pages. A failure during the build frees the freshly
  allocated extents and leaves the published view untouched, so a mid-batch
  out-of-space can never retire a page the live view still references.
- Never overwrite pages visible to an existing mmap reader.

Readers remain lock-free and operate against a stable generation.

---

## Concurrency

Use **many lock-free mmap readers and a serialized batch commit path**.

Readers acquire a `ReaderTracker` token, load one immutable `indexView`
(generation, root, and arena together), and release the token when the lookup
completes. They take no locks and never track generations.

Writers build replacement pages without modifying pages visible to existing
readers, then publish the new view. The arena lifetime invariant is:

```text
build -> publish -> wait for prior readers -> reclaim / unmap
```

`ReaderTracker` is the sole authority for page lifetime. `Reclaim` waits for
prior readers and frees retired extents; `Compact` builds a replacement arena,
publishes it, waits, and only then releases the old arena; `Close` sets a closed
flag, waits for in-flight readers, reclaims, and unmaps. An arena is never
unmapped before its replacement is published and the grace period completes.

---

## Tombstones

Deletion is by `IndexID`, not hash. Maintain a separate **Roaring bitmap of
deleted IndexIDs**.

```text
Delete(IndexID)
    ↓
Tombstones.Add(IndexID)
```

Do not locate or modify the hash tree during deletion, and do not introduce
per-leaf deletion deltas. Lookup verifies the exact hash, obtains candidate
IndexIDs, and filters them through the tombstone bitmap. This keeps deletion
independent of the COW radix structure and avoids a reverse `IndexID -> hash`
index solely for deletion.

Tombstoned records can be physically removed when a shard is compacted/rebuilt.

---

## Sharding

Target approximately **4 GiB of live index data per shard**, rather than
treating 4 GiB as a hard address-space boundary. COW, WAL, allocator metadata,
and retired extents can temporarily make the physical file larger.

When the active shard reaches its live-data target:

1. Seal it against new inserts.
2. Create a new active shard.
3. Continue inserting into the new shard.

Exact lookup searches shards in parallel. Each shard independently performs its
byte-oriented radix lookup and leaf search. Stop/merge according to whether the
caller requests the first match or all duplicate IndexIDs.

Because each shard is independent, allocator state, root directory,
generations, and COW reclamation are shard-local.

---

## Hash Types

The radix engine supports only fixed 256-bit cryptographic hashes:

```go
type Hash [32]byte
```

BLAKE3 and SHA-256 use separate index files but share the same generic storage
implementation. Do not mix hash algorithms within a shard or index file. MD5
and other non-256-bit hashes are outside this format unless represented by a
separate implementation.

---

## Lookup

The common lookup path is approximately:

```text
Hash[0]
   -> root radix
Hash[1]
   -> second radix
leaf
   -> compare Hash[2:32]
   -> obtain IndexID
   -> check tombstone
```

If the path has been promoted:

```text
Hash[0]
 -> Hash[1]
 -> Hash[2]
 -> ...
 -> leaf
```

The radix path itself is the coarse hash discriminator. A zero child is an
immediate miss. At the leaf, compare the unconsumed tail (typically
`Hash[2:32]`) against the query tail and return the corresponding `IndexID`s
that are not tombstoned.

The leaf tails are sorted, so lookup binary-searches for the lower bound and
walks the short equal run rather than scanning the whole leaf. This keeps
latency close to O(log n) even for large two-level leaves and removes most of
the motivation for promoting solely to shorten a scan.

Implement scalar first, then architecture-specific AVX2/AVX-512/NEON tail
comparison kernels behind the same interface. The persistent layout must not
depend on CPU architecture.

---

## Durability Modes

Support the same on-disk format with configurable durability.

### Durable

Use **WAL + COW**:

1. Record the batch/WAL intent.
2. Group mutations by radix path.
3. Allocate and write replacement leaves/nodes.
4. Flush required data.
5. Atomically publish the new generation/root metadata.
6. Mark/checkpoint the WAL commit.
7. Retire old extents for later epoch reclamation.

### Rebuildable / Fast

Use COW generation publication with relaxed WAL/fsync behavior. An interrupted
batch may be discarded, but the last published generation remains readable.
Hash indexes are assumed rebuildable from source image metadata/content when
this mode is selected.

---

## On-Disk Shard Sections

Conceptually each shard contains:

```text
Shard
├── Superblock A/B
│   ├── format/version
│   ├── hash algorithm
│   ├── generation
│   ├── root location
│   └── allocator/checkpoint metadata
│
├── Root radix node (256 × uint32 = 1024 bytes)
│
├── pagealloc arena
│   ├── free bitmap
│   ├── radix nodes (1024 bytes each)
│   ├── leaves
│   └── COW/retired space
│
└── optional WAL/checkpoint region or companion WAL file
```

Use dual/checksummed superblocks or an equivalent atomic-generation publication
mechanism so recovery can select the newest valid committed generation.

The reusable part of this envelope lives in `internal/storage`
(`SuperblockHeader`, `EncodeSuperblock`, `DecodeSuperblock`,
`SelectSuperblock`): magic, version, flags, generation, payload length, and a
payload CRC plus a header CRC. An index package owns only its payload; RadixHash
stores record counts, depth, and its root CRC in a version-2 payload and keeps
its per-slot root in the arena. Any mmap index can reuse the envelope and
selection without depending on RadixHash.

---

## Space and Memory Optimizations

The persistent format is shaped to keep both live bytes and committed memory
small:

- **Exact-fit leaf allocation.** Leaves are allocated as exact 512-byte runs
  rather than power-of-two buddy classes, removing the 20–30% rounding waste
  that a 5,808-byte leaf would otherwise incur in an 8 KiB block.
- **32-bit IDs when possible.** A leaf stores `[]uint32` IDs while every id
  fits in 32 bits and falls back to `[]uint64` otherwise.
- **Automatic reclamation.** `Config.AutoReclaimBytes` bounds retired space:
  once retired bytes reach the threshold, the writer drains prior readers and
  reclaims automatically instead of holding up to 2× live data as dead pages.
- **File-backed mmap arena.** `NewMmap` maps a sparse file as the arena, so a
  large logical arena consumes disk and resident memory only for touched pages;
  `Sync` flushes and `Close` unmaps.
- **Compact.** `Compact` rebuilds the tree into a freshly packed arena and
  publishes it atomically, reclaiming fragmentation and any remaining slack.

---

## Benchmarks

Benchmark on both amd64 and arm64.

### Two-level versus three-level lookup

This comparison is required before freezing the promotion threshold. Build the
same logical record set two ways and benchmark point lookup at increasing
record counts:

```text
10K
100K
1M
10M
```

- **Two-level:** disable promotion (threshold above the largest leaf) so the
  normal `Hash[0] -> Hash[1] -> leaf` shape is retained.
- **Three-level:** enable promotion so large `Hash[0]/Hash[1]` buckets become
  `Hash[0] -> Hash[1] -> Hash[2] -> leaf`.

Report `ns/op`, `B/op`, and `allocs/op`, and also measure:

- hit with one ID;
- hit with 2/4/16 duplicates;
- random miss;
- hot repeated hit;
- random hits across a large mapping.

The two-level path should win when buckets stay small (one fewer memory
dereference); the three-level path should bound leaf size and scan cost as
buckets grow. Confirm the crossover empirically and pick the default threshold
from it.

### Promotion threshold

Sweep configurable thresholds:

```text
8 KiB
16 KiB
32 KiB
64 KiB
96 KiB
128 KiB
256 KiB
```

Measure lookup latency, leaf scan length, promotion frequency, COW allocation
volume, index bytes/entry, tree depth, leaf count/occupancy, and compaction
throughput for each, for both uniform and skewed hashes. Benchmarks live in
`BenchmarkPromotionSweep` / `BenchmarkPromotionSweep10M`.

Measured (uniform, per `index/radixhash` `BenchmarkPromotionSweep`): a broad
64–128 KiB plateau, with 256 KiB skipping promotion (large leaves, slower
lookup) and 8–32 KiB raising COW allocation. The default stays **96 KiB**;
adaptive promotion is unchanged.

### Compaction

`BenchmarkCompact` streams the old tree in bounded batches. Benchmark
throughput, peak temporary memory, allocations, and total bytes allocated at
100K/1M/10M. Compaction never builds an `[]Entry` proportional to index size.

### Insert

Measure single-entry append, batches of 100/1K/10K, page seal cost, WAL cost,
checkpoint throughput, splits/promotions per batch, and bytes written per
logical entry.

### Negative and duplicate lookup

Measure misses, duplicate runs, and hot/cold mixed workloads.

### Storage and allocator

Measure page growth classes, batch sizes, COW write amplification, allocator
fragmentation, retired-space coalescing, and physical/live byte ratio before
and after compaction.

### CPU counters where available

Record cycles/hash, instructions/hash, branch misses, L1/LLC misses, and memory
bandwidth.

---

## Implementation Order

1. Build and fuzz-test `pagealloc` independently: allocation, splitting,
   retirement, reclamation, exact-fit run coalescing, reopen/recovery, and
   corruption checks.
2. Build scalar `radixhash` with byte-oriented 256-way nodes, lazy second-level
   allocation, and the depth-tagged SoA leaf format.
3. Add adaptive leaf promotion and the promotion-threshold configuration.
4. Add batched COW path copying and generation-pinned mmap readers.
5. Integrate the Roaring tombstone index.
6. Add shard rollover and parallel shard lookup.
7. Add WAL/durability modes.
8. Add SIMD tail-comparison kernels and benchmark against scalar/binary-search
   alternatives.
9. Benchmark two-level versus adaptively promoted three-level leaves, page
   growth classes, batch sizes, lookup latency, COW write amplification,
   allocator fragmentation, and negative/duplicate lookup before freezing the
   persistent format.

## Core Design Rule

Keep the layers independent:

```text
hashidx     = lifecycle + shards + WAL + tombstones
radixhash   = 256-bit hash -> IndexID byte-oriented radix tree / SoA leaves
pagealloc   = persistent 512-byte exact-fit allocation + COW reclamation
```

`pagealloc` must remain reusable by other ImageIdx indexes and must not know
anything about hashes. `radixhash` must not implement its own free-space
allocator. This separation allows allocator policy, SIMD kernels, and
hash-index behavior to evolve independently.

## Design Objective

The resulting exact-hash index should prioritize:

- mmap-friendly lock-free reads
- batched COW writes
- low write amplification
- lazy allocation
- SIMD-friendly SoA leaves
- small initial database size
- predictable fixed 256-way radix nodes
- 512-byte exact-fit allocation
- reuse/coalescing of retired space
- exact-fit leaf packing, 32-bit ids, auto-reclaim, mmap arena, Compact
- approximately 4 GiB independent shards
- parallel shard lookup
- Roaring tombstones

and explicitly avoid:

- general-purpose ART complexity
- SwissTable control bytes
- Bloom/Cuckoo filters
- per-leaf delta structures
