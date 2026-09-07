# Timestamp Index Implementation

This document describes the implemented `timeidx` package.

## Purpose and Compression

The timestamp index maps an image to a compressed time marker and supports
half-open range queries. A marker is intentionally `uint32`:

```text
marker = (timestamp - Spec.Offset) >> 24
```

The lower 24 bits are discarded. The representable interval is validated;
underflow and overflow return `ErrInvalidRange`. `Search` therefore returns a
conservative candidate set at marker boundaries. `SearchExact` accepts a
canonical timestamp lookup and removes false-positive boundary candidates.

The marker is not widened to eight bytes. Base storage remains two separate
structure-of-arrays columns: `[]uint32` markers and `[]uint64` image IDs.

## Public API

```go
index, err := timeidx.New(timeidx.Spec{
    IndexID: 9,
    Flags:   0,
    Offset:  offset,
    Entries: []timeidx.Entry{
        {ImageID: 1, Timestamp: createdAt},
    },
}, timeidx.Config{})

batch := timeidx.NewBatch(1)
batch.Append(timeidx.Move(1, oldTimestamp, newTimestamp))
err = index.Apply(batch)
```

`Spec` matches the stable identity pattern used by bitmap and geographic
indexes. `Metadata()` returns `IndexID`, flags, generation, compression offset,
base count, and block count.

Mutations are sealed values created with `Insert`, `Delete`, or `Move`.
`Batch` owns reusable fixed-capacity storage; a move expands internally to a
delete and add without growing the batch. `Append` and `Reset` return false
when the batch cannot be modified. `Apply` waits for atomic acceptance by the
single writer and returns `ErrBackpressure` when a block is awaiting
compaction.

## Block and Delta Model

Initial entries are sorted by marker and then image ID. Equal markers are never
split between blocks. Each block publishes a private view containing:

- immutable SoA Base;
- optional fixed-capacity Frozen delta;
- fixed-capacity Active delta.

The Active and Frozen additions use separate marker and image-ID arrays.
Deletes store image IDs. A fixed hash table records the latest state per image
so readers can ignore superseded additions without allocating.

One writer goroutine owns mutation routing, WAL ordering, delta rotation, and
compaction scheduling. Readers load the block view without locks. The index's
private reader tracker prevents a retired delta from being reset while a
reader still uses it. Public search methods register and release internally;
`Close` waits for prior readers before unmapping storage. The visible bound is
Base + Frozen + Active.

## Search Semantics

`Search(dst, start, end)` appends compressed candidates for `[start, end)`.
`SearchExact` applies canonical verification. `EstimateRange` returns an upper
bound suitable for scheduling; deletions and repeated updates can make it
larger than the result.

Caller-owned destination capacity makes steady-state range search
allocation-free. The combined `search` planner can collect timestamp results
or, when exact lookup is supplied after an earlier stage, filter candidates in
place.

## Persistence

A checkpoint writes three generation files:

- `markers.<generation>.base`: page-aligned `uint32` marker blocks;
- `ids.<generation>.base`: page-aligned `uint64` image-ID blocks;
- `blocks.<generation>.idx`: 64-byte block directory entries.

Marker and ID offsets are multiples of 4,096 and therefore of eight. A marker
block may contain an odd number of four-byte markers; padding outside the
recorded count brings the next structural boundary and file end to a multiple
of eight. The marker element itself remains four bytes and the SoA columns stay
separate.

The version-2 manifest is 80 bytes and persists generation, stable `IndexID`,
counts, timestamp bounds, compression offset, WAL offset, block count, flags,
and CRC. The WAL uses a 16-byte header and 24-byte private write records.
All formats are little-endian and CRC32/IEEE checked.

Manifest version 2 adds stable identity and aligned generation-file endings;
version-1 persistent indexes are not opened by this implementation.

`NewPersistent(dir, Spec, Config, Durability)` creates an index and initial
checkpoint. `OpenPersistent(dir, Config, Durability)` mmaps Base columns,
validates every directory offset, replays the WAL tail, and resumes the writer.
`Vacuum` retains its checkpoint behavior.

## Invariants

- Marker and image-ID arrays always have equal lengths.
- Base is sorted by marker, then image ID.
- Equal markers remain in one block.
- A batch is accepted atomically or rejected without partial mutation.
- Published views never exceed Base + Frozen + Active.
- Steady-state search and accepted updates do not allocate.
- Marker width remains `uint32`; image ID remains the shared `uint64` alias.
- Persisted structural boundaries and fixed records are eight-byte aligned.
- Stable identity and flags survive checkpoint and reopen.
