# radixhash

A persistent, mmap-oriented exact hash index for fixed-width **256-bit**
digests (SHA-256, BLAKE3-256). It maps a digest to one or more
`common.ImageID` values, allows duplicate digests, and is optimized for
lock-free reads, batched copy-on-write writes, and lazy allocation.

An index holds exactly **one** hash algorithm. Use a separate index (and
shard) per algorithm.

## Structure

Consumes one hash byte per radix level. Every node is a fixed 256-way
`[256]uint32` (1024 bytes, two 512-byte allocator blocks); a zero child is a
miss, and the low tag bit distinguishes a leaf from another radix node.

A small bucket is a **direct root-to-leaf** child:

```text
Hash[0] -> root node
        -> leaf                       // sparse: small bucket
```

When that leaf grows past `Config.PromotionBytes` (or `Config.MaxLeafSlots`) it
is promoted to a radix node and its records are redistributed, recursively, so
there is no fixed L0/L1/L2 depth:

```text
Hash[0] -> root node
Hash[1] -> radix node
        -> leaves
Hash[2] -> optional deeper radix node, only after that leaf is promoted
```

Leaves are **structure-of-arrays**: the consumed prefix is implicit in the
path, so a leaf stores only the unconsumed tail and the IDs, sorted by
`(tail, id)`.

```text
Leaf
├── Header (magic, version, depth, id width, count, capacity)
├── HashTail[]   Hash[depth:32]   // 31 bytes at depth 1, 30 bytes at depth 2, ...
└── IndexID[]    uint32 or uint64 // 32-bit unless an id exceeds 2^32
```

- A leaf larger than `Config.PromotionBytes` (or `Config.MaxLeafSlots`)
  is promoted in place to the next radix level and redistributed, recursively.
- Readers binary-search the sorted tails and walk only the equal run.
- `Apply` groups a batch by radix path and rewrites each affected leaf once,
  path-copies nodes, and publishes one new immutable root.

Storage is a caller-owned arena managed by `internal/pagealloc`: exact
512-byte runs (no power-of-two rounding), `Retire`/`Reclaim` for
copy-on-write, and a free bitmap. The arena can be a heap slice or a sparse
mmap file.

## Memory lifetime (RCU)

Everything reachable from a published `indexView` is immutable, and the view is
the complete reader snapshot: generation, root, and arena travel together. A
transaction reads existing nodes and leaves through the arena of its starting
view, so a root from one view can never be combined with an arena from another.

The arena lifetime invariant is:

```text
build -> publish -> wait for prior readers -> reclaim / unmap
```

`ReaderTracker` is the sole authority. `Reclaim` waits for every reader that
could still hold a retired page before freeing it; readers never track
generations. `Compact` builds the replacement arena fully, publishes it, runs
the grace period, and only then releases the old arena. `Close` sets a closed
flag (so new readers skip the arena), waits for in-flight readers, reclaims
retired extents, and finally unmaps.


## Usage

```go
import (
    "crypto/sha256"

    "github.com/evanoberholster/imageIdx/index/radixhash"
)

index, err := radixhash.New(64<<20, radixhash.Config{}) // 64 MiB heap arena
if err != nil {
    return err
}
defer index.Close()

sum := sha256.Sum256(content)
var digest radixhash.Hash
copy(digest[:], sum[:])

if err := index.Apply([]radixhash.Entry{{Hash: digest, ImageID: imageID}}); err != nil {
    return err
}

ids := index.Lookup(nil, digest)        // ascending, unique
found := index.Contains(digest)
n := index.Count(digest)
```

BLAKE3 uses the same type:

```go
sum := blake3.Sum256(content)
var digest radixhash.Hash
copy(digest[:], sum[:])
```

### Deletion

Deletion is external: pass a liveness predicate to `LookupFiltered` (for
example a roaring bitmap of live IDs) instead of rewriting leaves.

```go
live := func(id radixhash.ImageID) bool { return !deleted.Contains(id) }
ids := index.LookupFiltered(nil, digest, live)
```

### Durable / mmap arena

`NewMmap` creates a sparse file arena, so committed memory follows the pages
actually used. `Checkpoint` writes the current root and metadata to a dual
superblock (with CRCs) and flushes; `OpenMmap` reopens the file, picks the newest
valid superblock, and reconstructs allocation state by walking the tree — so
space retired before the last checkpoint is reclaimed on reopen. `Sync` flushes
without a new generation; `Close` unmaps.

```go
config := radixhash.Config{AutoReclaimBytes: 64 << 20}

index, err := radixhash.NewMmap("/data/hashes.bin", 4<<30, config)
if err != nil {
    return err
}
if err := index.Apply(batch); err != nil {
    return err
}
if err := index.Checkpoint(); err != nil { // durable generation
    return err
}
if err := index.Close(); err != nil {
    return err
}

// Later, in another process:
reopened, err := radixhash.OpenMmap("/data/hashes.bin", config)
if err != nil {
    return err
}
defer reopened.Close()
ids := reopened.Lookup(nil, digest)
```

`Checkpoint` is a no-op for a heap arena; `OpenMmap` requires a file previously
created by `NewMmap`.

### Reclamation and compaction

Replaced pages are retired until no reader can reference them. `Reclaim` waits
for prior readers through the `ReaderTracker` and frees every retired extent;
`AutoReclaimBytes` does it automatically once retired bytes cross the threshold.
`Compact` rebuilds into a freshly packed arena by **streaming** bounded entry
batches from the old immutable view through an internal walker (`walk.go`), so
peak compaction memory is independent of index size.

```go
_ = index.Reclaim()
_ = index.Compact()
```

### Combined search

`search` can use a radixhash index directly. The digest is exact, so a
one-bit difference is a miss.

```go
plan, err := search.Compile(
    search.ExactHash(index, search.HashBLAKE3, digest),
    search.AllValues(tags, bitmapidx.EnumStringValue("favorite")),
)
ids, err := plan.Search(ctx, nil, &ws)
```

## Config

| Field | Meaning |
|---|---|
| `PromotionBytes` | Promote a leaf to the next radix level past this encoded size. Default 96 KiB, retained after sweeping 8 KiB–256 KiB: 64–128 KiB is a broad plateau, while 256 KiB skips promotion and hurts lookup, and 8–32 KiB raises COW allocation. |
| `MaxLeafSlots` | Optional record-count promotion cap; benchmark it — a low cap fragments space. |
| `AutoReclaimBytes` | Reclaim retired space automatically once retired bytes reach this. |

## Files

| File | Responsibility |
|---|---|
| `hash.go` | Public types (`Hash`, `Entry`, `Config`, `Metadata`) and errors. |
| `node.go` | 256-way radix node encoding and child tagging. |
| `leaf.go` | SoA leaf codec, sorted-tail search, promotion partitioning. |
| `index.go` | Arena, views, copy-on-write `Apply`, lookup, reclaim, compact. |
| `walk.go` | Bounded tree iterator used by streaming compaction. |
| `superblock.go` | Radix payload + per-slot root over the common envelope. |
| `persist.go` | `NewMmap`/`OpenMmap`, `Checkpoint`, and reopen reconstruction. |
| `internal/tailkernel` | Scalar reference + NEON/AVX2 16-byte tail-prefix kernels. |
| `internal/storage` | mmap, `ReaderTracker`, and the dual-superblock envelope (header, generation, CRC, slot selection). |

## Lookup kernels

Leaves are sorted, so lookup binary-searches the tails — the correctness
reference. Small leaves (≤ 64 slots) instead scan linearly with
`internal/tailkernel`, which compares 16-byte prefixes on NEON (arm64) or AVX2
(amd64) and confirms each hit with a full-tail compare; the scalar reference is
used everywhere else.

## Scope

Implemented: the exact-fit allocator, the byte-oriented radix engine, adaptive
promotion (default 96 KiB retained after a threshold sweep), batched COW
commits, generation-gated readers, streaming compaction, the file-backed mmap
arena with superblock reopen/checkpoint over the shared `internal/storage`
envelope, and SIMD tail kernels. Follow-ups: WAL durability modes, shard
rollover and parallel lookup, and registering RadixHash in the unified
`index.Store` schema.
