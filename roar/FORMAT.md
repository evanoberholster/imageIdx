# ROAR bitmap format

This document describes the version-1 serialized format produced by the
`roar` package. The serialized byte region is the read representation: a
`Bitmap` operates directly over these bytes (typically an mmap) with zero-copy
unsafe access, decoding no container objects.

## Format stability

The version-1 format is **frozen**. The header layout, container types
(array and bitmap), flags, and alignment rules described here are the
stability contract: they will not change for data tagged version 1. Any future
change (for example a run-length container type) will bump `Version` and be
read as a distinct format, leaving version-1 data readable unchanged. The
transposed "RAOR" magic was accepted during development and is no longer
recognized.

## Design goals

- **Immutable.** The bytes are the value; readers never modify them.
- **Zero-copy.** Every structure is 8-byte aligned so headers, keys, and
  bitmap words are read in place as native little-endian integers.
- **Compact.** Sparse containers are sorted `uint16` arrays; dense containers
  are 1,024 `uint64` words.

## Value space

An ID is a `uint64` split into two parts:

| bits | field | meaning |
|------|-------|---------|
| 48..63 | key | container key (upper 48 bits) |
| 0..15 | value | position within the container (lower 16 bits) |

`ImageID` `0` is reserved and never represents an image. A key of `0` with a
non-zero value is valid; only the full `uint64` `0` is rejected.

## Byte layout

```
+--------+------------+-----------------------+---------------------+
| header | key table  | containers            |                     |
| 24 B   | 16 B/entry | (variable, aligned)   |                     |
+--------+------------+-----------------------+---------------------+
```

The header records the absolute byte offset of every container, so readers
jump straight to a key's payload without walking intermediate containers.

## Header (24 bytes, little-endian)

| offset | size | field | description |
|--------|------|-------|-------------|
| 0 | 4 | magic | `0x52414F52` ("ROAR") |
| 4 | 2 | version | `1` |
| 6 | 2 | flags | see Flags below |
| 8 | 4 | header bytes | offset of the key table (24) |
| 12 | 4 | key count | number of key entries |
| 16 | 8 | cardinality | total ID count (when flag set) |

## Flags

| bit | name | meaning |
|-----|------|---------|
| 0 | `FlagCompact` | layout is the compact form described here |
| 1 | `FlagStoredCardinality` | `header.cardinality` holds the exact total |

When `FlagStoredCardinality` is unset, readers fall back to summing container
headers.

## Key table

One 16-byte entry per container, sorted by ascending key:

| offset | size | field |
|--------|------|-------|
| 0 | 8 | key (upper 48 bits; low 16 bits are zero) |
| 8 | 8 | absolute byte offset of the container |

Entries must be strictly increasing by key, with no duplicate keys.

## Container header (8 bytes)

| offset | size | field |
|--------|------|-------|
| 0 | 2 | size in 8-byte blocks (includes this header) |
| 2 | 2 | type: `0` array, `1` bitmap |
| 4 | 4 | cardinality |

Every container occupies a whole number of 8-byte blocks, keeping the next
container aligned.

## Container types

### Array (`type == 0`)

A sorted, strictly-increasing sequence of `uint16` values, stored
little-endian immediately after the header. Used when the cardinality is at or
below `ArrayToBitmapThreshold` (2048).

### Bitmap (`type == 1`)

1,024 `uint64` words (8 KiB) storing one bit per value, after the header. Used
above the threshold.

## Alignment and endianness

- The header, key table, container headers, and bitmap payloads all begin on
  8-byte boundaries, and every container occupies a multiple of 8 bytes. This
  is what makes zero-copy `uint64` access valid.
- The format is little-endian by design. Multi-byte fields are written and
  read with the host's native order; `Open` rejects big-endian hosts, and
  constructors assume little-endian as a precondition.

## Validation

`Open`/`Validate` reject untrusted input that fails any of: magic or version
mismatch, unaligned base address or offsets, key table out of bounds, unsorted
or duplicate keys, low key bits set, reserved `ImageID` 0, unsorted array
values, bitmap cardinality mismatch, and (when stored) a cardinality that does
not match the summed container headers.
