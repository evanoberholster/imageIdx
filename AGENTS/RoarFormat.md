# Roar Format Implementation

The `roar` package is a standalone immutable, zero-copy bitmap implementation.
Its serialized bytes are its read representation. Logical indexes use `roar`
bitmaps directly for bases, deltas, and dense geo cells.

## Format

The little-endian format contains:

- a 16-byte header (version, flags, key count, cardinality — no magic
  signature; parent envelopes identify the payload, and the key table always
  starts immediately after the header);
- sorted 16-byte high-key entries containing 64-bit byte offsets;
- 8-byte container headers;
- sorted `uint16` array payloads or 1,024-word `uint64` bitmap payloads.

Every structural boundary and serialized container size is a multiple of eight.
Array payloads retain `uint16` elements and are padded at the container end;
alignment never implies widening each value.

`Open` validates version, lengths, key ordering, offsets, container
ordering/cardinality, the reserved word, little-endian support, and
buffer-base alignment. `OpenTrusted` skips validation only for
already-verified storage bytes. Constructors emit the version tag with a zero
reserved word. Pre-seal bytes carrying the old `"ROAR"` magic are rejected
with `ErrUnsupportedVersion`, never misread.

## Construction and Operations

`FromSorted` validates strictly increasing IDs and constructs an immutable
`Bitmap` in two passes and one allocation; `FromUnsorted` accepts any order.
`DeltaBuilder` accumulates deferred ID edits and whole-set operations.
`Builder` is reusable output storage for `AndInto`, `OrInto`, and `AndNotInto`;
`Compact` returns its serialized bytes.

`Bitmap` provides zero-allocation `Contains`, fused cardinality operations, and
intersection tests. `AppendTo` is the preferred ascending materialization path.

Container choice and linear-search thresholds are runtime policies and are not
format guarantees. Dense-word kernels have AVX2/NEON backends selected at
startup behind `goexperiment.simd`; scalar behavior and benchmarks remain the
baseline.

The format tests walk every emitted key entry and container to enforce
eight-byte alignment and validate malformed input rejection.
