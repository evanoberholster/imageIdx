# Roar Format Implementation

The `roar` package is a standalone immutable, zero-copy bitmap implementation.
Its serialized bytes are its read representation. Logical indexes currently
use `idset`; migrating them to `roar` requires separate benchmark evidence.

## Format

The little-endian format contains:

- a 16-byte header;
- sorted 16-byte high-key entries containing 64-bit byte offsets;
- 8-byte container headers;
- sorted `uint16` array payloads or 1,024-word `uint64` bitmap payloads.

Every structural boundary and serialized container size is a multiple of eight.
Array payloads retain `uint16` elements and are padded at the container end;
alignment never implies widening each value.

`Open` validates magic, version, lengths, key ordering, offsets, container
ordering/cardinality, little-endian support, and buffer-base alignment.
`OpenTrusted` skips validation only for already-verified storage bytes.
Constructors emit the corrected `ROAR` signature. `Open` does not need to accept the
legacy version-1 `RAOR`.

## Construction and Operations

`FromSortedList` validates and constructs an immutable `Bitmap` in two passes
and one allocation. `FromSorted` returns the same compact encoding directly.
`Builder` is reusable output storage for `AndInto`, `OrInto`, and `AndNotInto`;
`Compact` returns its serialized bytes.

`Bitmap` provides zero-allocation `Contains`, fused cardinality operations, and
intersection tests. `View` is a deprecated compatibility alias. `AppendTo` is
the preferred ascending materialization path. The iterator exists for
incremental compatibility.

Container choice and linear-search thresholds are runtime policies and are not
format guarantees. SIMD is not implemented; scalar behavior and benchmarks
remain the baseline.

The format tests walk every emitted key entry and container to enforce
eight-byte alignment and validate malformed input rejection.
