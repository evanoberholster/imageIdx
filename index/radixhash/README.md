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

```text
Hash[0] -> root node
Hash[1] -> L1 node (allocated lazily per Hash[0])
        -> leaf
Hash[2] -> optional L2 node, only after a leaf is promoted
```

Leaves are **structure-of-arrays**: the consumed prefix is implicit in the
path, so a leaf stores only the unconsumed tail and the IDs, sorted by
`(tail, id)`.

```text
Leaf
├── Header (magic, version, depth, id width, count, capacity)
├── HashTail[]   Hash[depth:32]   // 30 bytes at the normal depth 2
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

`NewMmap` maps a sparse file, so committed memory follows the pages actually
used. `Sync` flushes; `Close` unmaps.

```go
index, err := radixhash.NewMmap("/data/hashes.bin", 4<<30, radixhash.Config{
    AutoReclaimBytes: 64 << 20, // bound retired space during sustained writes
})
defer index.Close()

if err := index.Apply(batch); err != nil {
    return err
}
if err := index.Sync(); err != nil { // durable
    return err
}
```

### Reclamation and compaction

Replaced pages are retired until no reader can reference them. Reclaim with
the oldest reader generation, or let `AutoReclaimBytes` do it, and use
`Compact` to rebuild into a freshly packed arena.

```go
_ = index.Reclaim(oldestReaderGeneration)
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
| `PromotionBytes` | Promote a leaf to the next radix level past this encoded size (default 96 KiB). |
| `MaxLeafSlots` | Optional record-count promotion cap; benchmark it — a low cap fragments space. |
| `AutoReclaimBytes` | Reclaim retired space automatically once retired bytes reach this. |

## Files

| File | Responsibility |
|---|---|
| `hash.go` | Public types (`Hash`, `Entry`, `Config`, `Metadata`) and errors. |
| `node.go` | 256-way radix node encoding and child tagging. |
| `leaf.go` | SoA leaf codec, sorted-tail search, promotion partitioning. |
| `index.go` | Arena, views, copy-on-write `Apply`, lookup, reclaim, compact, mmap. |

## Scope

Currently implemented: the exact-fit allocator, the byte-oriented radix
engine, adaptive promotion, batched COW commits, generation-gated readers,
and a file-backed mmap arena. Follow-ups: on-disk superblock/reopen, WAL
durability modes, shard rollover and parallel lookup, and SIMD tail kernels.
