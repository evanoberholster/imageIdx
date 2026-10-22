# Bitmap Kernel SIMD Implementation

> **Design note.** Current implementation: `roar/internal/bitmapkernel`. Kernel
> changes require scalar-equivalence, race, and benchmark evidence.

Implement dense bitmap kernels behind a small internal abstraction. Do not change the serialized Roar format, container layout, public APIs, sparse-array algorithms, mmap behavior, or reader/writer model.

## Goal

Add scalar, AVX2, and ARM64 NEON implementations for the dense bitmap-container hot paths used by `roar`.

Dense bitmap containers are fixed at 65,536 bits = 1,024 `uint64` words = 8 KiB. SIMD should optimize these fixed-size dense operations without changing higher-level data structures.

## Kernel API

Create an internal package such as:

```text
roar/internal/bitmapkernel/
    kernel.go
    scalar.go
    avx2_amd64.go
    neon_arm64.go
```

Expose package-local functions equivalent to:

```go
func And(dst, a, b []uint64) uint32
func Or(dst, a, b []uint64) uint32
func AndNot(dst, a, b []uint64) uint32
func Intersects(a, b []uint64) bool
```

The returned `uint32` is the exact cardinality of the produced bitmap.

Update dense bitmap call sites in `roar` to use this kernel layer instead of embedding architecture-specific code in container logic.

## Scalar Backend

Use the current scalar word loops as the correctness reference and fallback implementation.

Requirements:

- Use `math/bits.OnesCount64` for cardinality.
- Preserve early exit in `Intersects`.
- Perform no heap allocations.
- Preserve current behavior exactly.

The scalar backend must remain available on every architecture and when SIMD is disabled.

## AVX2 Backend

Target amd64 using Go's `simd/archsimd` support under `GOEXPERIMENT=simd`.

Use 256-bit vectors and process four `uint64` words per iteration.

Implement:

```text
AND      dst = a & b
OR       dst = a | b
AND-NOT  dst = a &^ b
```

Keep SIMD vectors as local variables inside flat hot-loop functions. Do not store SIMD vector types in structs or expose them through package APIs.

Perform CPU feature detection once. Use the AVX2 backend only when AVX2 is available. Do not branch on CPU capabilities inside the inner loop.

### Cardinality

Do not perform a SIMD Boolean pass followed by a second pass for popcount.

Compute cardinality during the same traversal that produces the result. If a useful vector popcount is unavailable, store or extract the resulting four `uint64` lanes and apply `bits.OnesCount64` to those values before continuing.

Avoid doubling memory traffic simply to calculate cardinality.

### Intersects

`Intersects` should be the first SIMD kernel implemented and validated because it requires no popcount.

For each vector:

```text
v = a & b
```

Return `true` immediately if any lane is non-zero. Return `false` only after the entire bitmap has been checked.

Do not add AVX-512 in this implementation.

## ARM64 NEON Backend

Target ARM64 using Go's `simd/archsimd` ARM64 vector support under `GOEXPERIMENT=simd`.

Process two `uint64` words per 128-bit vector iteration.

Implement the same semantics as the AVX2 backend:

- vectorized AND, OR, and AND-NOT;
- exact cardinality calculated in the same traversal;
- early exit for `Intersects`;
- no allocations;
- no architecture-specific behavior visible to callers.

The implementation should work on both Apple Silicon and Linux ARM64.

## Dispatch

Select the implementation outside hot loops.

A suitable design is package-level function variables initialized to scalar functions and replaced during initialization when the SIMD backend is compiled and supported by the CPU.

```go
var andKernel = andScalar
var orKernel = orScalar
var andNotKernel = andNotScalar
var intersectsKernel = intersectsScalar
```

Package-local wrappers call the selected implementation without architecture checks:

```go
func And(dst, a, b []uint64) uint32 {
    return andKernel(dst, a, b)
}
```

Benchmark indirect function-call overhead. If it is measurable for these 8 KiB kernels, use another one-time dispatch pattern, but never put CPU checks inside the processing loop.

## Integration Priorities

Implement and benchmark in this order:

1. `Intersects`
2. dense AND
3. dense OR
4. dense AND-NOT
5. existing dense cardinality helpers
6. fused multi-input operations

Do not initially SIMD-optimize array-container operations such as sorted-array intersection, union, difference, lookup, or cursor logic.

## Fused Kernels

After the basic kernels are correct and measurably faster, apply the same backend to high-value fused dense operations.

### AndMany

For dense `AndManyInto`, load several input vectors, combine them in registers, and write the destination once.

Avoid intermediate 8 KiB bitmap containers.

### Apply / combineThree

Optimize the dense form of:

```text
(base | add) &^ remove
```

as one SIMD expression and one destination write.

This is likely more valuable than optimizing isolated binary Boolean operations because it reduces memory traffic as well as instruction count.

### setContainerBits

Consider SIMD only for dense bitmap paths where measurements show a benefit.

## Constraints

Do not introduce any of the following as part of SIMD work:

- Go assembly for the initial implementation;
- AVX-512;
- a SIMD-specific container type;
- Structure of Arrays conversion;
- changes to the 48-bit high / 16-bit low Roar decomposition;
- changes to serialized or mmap formats;
- changes to COW semantics;
- SIMD vector types in persistent structs;
- heap allocations in bitmap kernels;
- intermediate bitmap allocations merely to enable SIMD.

The data format determines the algorithm. CPU capabilities select only the dense bitmap kernel implementation.

## Correctness Tests

Treat the scalar implementation as the reference.

Add tests comparing scalar and architecture-specific backends for:

- all-zero containers;
- all-one containers;
- single-bit inputs;
- sparse random bits;
- dense random bits;
- alternating bit patterns;
- identical operands;
- disjoint operands;
- random 1,024-word containers.

Verify both resulting words and returned cardinality.

For `Intersects`, verify exact agreement with the scalar implementation across intersecting and non-intersecting inputs.

Run existing Roar differential and fuzz tests after integration.

## Benchmarks

Add focused kernel benchmarks for scalar versus SIMD using complete 1,024-word bitmap containers.

Measure at least:

```text
AND
OR
AND-NOT
Intersects
```

Test multiple data shapes:

- no intersection;
- early intersection;
- dense random data;
- sparse-bit data;
- hot repeatedly reused containers.

Also run existing higher-level benchmarks, including where available:

```text
BenchmarkIntersects
BenchmarkAndCardinality
BenchmarkOr
BenchmarkAndNot
BenchmarkAndManyCardinality
BenchmarkSearch
BenchmarkCombinedSearch
```

Report:

```text
ns/op
B/op
allocs/op
```

SIMD should be kept only where it improves representative workloads or clearly improves dense-container kernels without regressions elsewhere.

## Implementation Principle

Keep the architecture simple:

```text
Search / Planner
      |
      v
Roar container operations
      |
      +-- Array container  -> scalar sorted operations
      |
      +-- Bitmap container
              |
              v
       roar/internal/bitmapkernel
          |       |       |
        scalar   AVX2    NEON
```

SIMD is an implementation detail of dense bitmap containers, not a reason to redesign the index or bitmap format.
