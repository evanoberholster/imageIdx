# Implement the `ranking` Package

Implement a small, high-performance ranking package for `imageIdx`.

Before writing code, inspect the repository and existing conventions for `ImageID`, package layout, SIMD kernels, zero-allocation APIs, benchmarks, and tests. Reuse existing types and conventions rather than creating parallel abstractions.

## Goals

The ranking system operates on a candidate set of approximately 100–100,000 `ImageID`s.

It must support ranking candidates using independent signals such as:

- time distance
- geographic distance
- SigLIP/vector similarity
- BM25/Bleve relevance
- intrinsic image properties

Multiple independent ranking signals can then be fused by an algorithm into one final rank.

Optimize for:

- simple Go APIs
- zero allocations on hot paths
- contiguous SoA storage
- deterministic behavior
- SIMD-friendly data
- low memory bandwidth
- easy benchmarking
- extensibility to future rankers

Do not over-engineer the initial implementation.

---

# Core Semantics

All ranking signals exposed to the fusion layer are `uint16`.

The universal convention is:

```go
type Rank uint16

const (
    BestRank  Rank = 0
    WorstRank Rank = 65535
)
```

Lower is always better.

However, values from different rankers are **not directly comparable**.

For example:

```text
Time rank   = 5000
Geo rank    = 5000
SigLIP rank = 5000
```

do not represent equivalent relevance.

Each ranker only guarantees monotonic ordering within its own signal.

For a distance ranker:

```text
distance(A) < distance(B)
    =>
rank(A) <= rank(B)
```

For a similarity/relevance ranker:

```text
similarity(A) > similarity(B)
    =>
rank(A) <= rank(B)
```

Fusion algorithms are responsible for combining heterogeneous signals.

---

# Results

Final public results should remain simple.

Use the repository's existing `ImageID` type.

Conceptually:

```go
type Results struct {
    ImageIDs []imageidx.ImageID
    Ranks    []Rank
}
```

The slices are parallel:

```text
ImageIDs[i] corresponds to Ranks[i]
```

`Ranks` contains the **final fused rank**, not individual component signals.

Do not expose `Rank1`, `Rank2`, etc. as permanent fields on final results.

---

# Signals

Introduce an intermediate SoA representation for ranking signals.

Support up to four active ranking signals per query.

Conceptually:

```go
const MaxSignals = 4

type Signals struct {
    ImageIDs []imageidx.ImageID
    Values   [MaxSignals][]Rank
    Kinds    [MaxSignals]Kind
    Count    uint8
}
```

The four arrays are execution lanes, not permanently assigned meanings.

For example, one query could use:

```text
Signal 0 = Time
Signal 1 = Geo
Signal 2 = SigLIP
Signal 3 = BM25
```

while another could use:

```text
Signal 0 = SigLIP
Signal 1 = Aesthetic Quality
```

Define a small `Kind` enum for diagnostics/configuration. Do not make fusion fundamentally dependent on hard-coded signal kinds unless required by a particular algorithm.

Validate/document:

```text
len(Values[i]) == len(ImageIDs)
```

for every active signal.

---

# Memory Layout

Signals must use Structure of Arrays.

Do NOT implement:

```go
type RankedImage struct {
    ImageID ImageID
    Time    uint16
    Geo     uint16
    Vector  uint16
    Text    uint16
}
```

Prefer:

```text
ImageIDs: []ImageID

Signal 0:
uint16 uint16 uint16 uint16 ...

Signal 1:
uint16 uint16 uint16 uint16 ...

Signal 2:
uint16 uint16 uint16 uint16 ...

Signal 3:
uint16 uint16 uint16 uint16 ...
```

This is required for cache efficiency and future SIMD fusion kernels.

Where practical, allow the four signal slices to be backed by one caller-owned contiguous allocation:

```text
[N Signal0][N Signal1][N Signal2][N Signal3]
```

Do not allocate these buffers inside ranking hot paths.

---

# Ranker Boundary

Rankers should output `[]Rank` / `[]uint16` directly.

Native values do NOT need to be stored in intermediate arrays unless required by the underlying index implementation.

Conceptually:

```go
type Ranker interface {
    Rank(dst []Rank, ids []imageidx.ImageID) error
}
```

Do not force this interface if repository inspection reveals that concrete functions or another pattern would be simpler and faster.

Avoid interfaces inside per-image loops.

The important boundary is:

```text
native metric
    ↓
ranker-specific encoding
    ↓
uint16 signal
```

---

# Time Ranking

Time ranking means temporal distance from a specified target date/time.

Calculate native distance using an unsigned integer representation equivalent to:

```text
abs(imageTime - targetTime)
```

Prefer seconds as the native distance unit.

A `uint32` native distance is sufficient:

```go
type TimeDistance uint32
```

It represents roughly 136 years at one-second resolution.

Do not create a `[]uint32` intermediate unless necessary.

Prefer:

```text
timestamp lookup
    ↓
uint32 distance
    ↓
encode
    ↓
dst[i] uint16
```

The encoding should be monotonic and should provide greater useful resolution for nearby timestamps than extremely distant timestamps.

Investigate a cheap integer/logarithmic encoding rather than calling `math.Log` for every candidate.

Benchmark before adding complicated bit-level encodings.

---

# Geographic Ranking

Geographic ranking means physical distance from a specified query location.

S2 CellIDs may be used by the geographic index, but:

**Do not treat numeric S2 CellID difference as geographic distance.**

Calculate an appropriate angular/geodesic/S2 distance.

A native `uint32` representation is sufficient.

Centimeters are a reasonable conceptual unit because the Earth's maximum surface distance still fits within `uint32`.

Conceptually:

```go
type GeoDistance uint32 // centimeters
```

Again, avoid storing a complete `[]uint32` intermediate if ranking can be performed as:

```text
coordinate lookup
    ↓
distance calculation
    ↓
uint32 native distance
    ↓
encode
    ↓
dst[i] uint16
```

Use a monotonic encoding that provides more useful resolution at short distances.

---

# SigLIP / Vector Ranking

SigLIP similarity should be quantized directly into `uint16`.

Do NOT create an intermediate `[]float32` similarity buffer merely for ranking.

The preferred pipeline is:

```text
embedding
    ↓
SIMD dot product / cosine similarity
    ↓
float32 similarity in register/local
    ↓
quantize
    ↓
uint16 signal
```

Assuming normalized embeddings and cosine similarity in approximately `[-1, +1]`, provide a monotonic encoder where:

```text
similarity +1 → rank 0
similarity -1 → rank 65535
```

Conceptually:

```go
func QuantizeCosine(sim float32) Rank
```

Clamp out-of-range floating-point values safely.

The important invariant is:

```text
higher cosine similarity
    =>
lower or equal Rank
```

Do not reinterpret float bits as integers.

Quantize numerically.

Provide focused tests for boundaries, monotonicity, NaN behavior if applicable, and representative similarity values.

---

# BM25 / Bleve

Design the package so BM25/Bleve relevance can be added without changing the core representation.

BM25 relevance scores are query-dependent and should NOT be assumed to have a globally comparable absolute scale.

A future BM25 ranker should transform its query's relevance scores into a monotonic `uint16` signal where:

```text
higher BM25 relevance
    =>
lower Rank
```

Do not attempt to make a BM25 value numerically comparable to Time, Geo, or SigLIP.

If Bleve integration is outside the current scope, provide the appropriate extension point but do not introduce a dependency merely to demonstrate it.

---

# Missing Values

Define explicit behavior for missing rankable properties.

Examples:

- image has no timestamp
- image has no GPS
- image has no embedding
- image is absent from a text index

Prefer treating missing values as worst unless an algorithm explicitly handles them differently.

If `65535` is reserved for missing, document that decision and use `65534` as the maximum ordinary encoded value.

Do not silently conflate missing data with an exact match.

---

# Fusion

Fusion is separate from rank calculation.

Conceptually:

```go
type Fusion interface {
    Fuse(dst []Rank, src *Signals)
}
```

The fusion implementation produces one final `Rank` for each `ImageID`.

Do not assume that raw signal values are inherently comparable.

Support the architecture needed for multiple fusion strategies.

Initially implement only fusion algorithms whose semantics can be clearly defined and tested.

Potential strategies include:

### Weighted/Normalized Fusion

Normalize/calibrate individual signals before applying weights.

Do not simply calculate:

```text
Time + Geo + BM25 + SigLIP
```

unless the algorithm explicitly defines how those heterogeneous values become comparable.

### Ordinal Fusion

Convert each signal to its ordering among the candidate set before combining it.

This removes dependence on the numeric scales of individual rankers.

### Reciprocal Rank Fusion

Design for RRF as an important future/initial algorithm, particularly for combining text and vector retrieval.

RRF operates on ordering rather than assuming compatible score scales.

Keep sorting/order-based algorithms separate from simple O(N) value-fusion kernels internally.

Do not force all fusion algorithms through unnecessary sorting.

---

# Final Ranking and Sorting

Do not reorder `ImageIDs` while individual rankers execute.

Maintain positional correspondence:

```text
ImageIDs[i]
Signal0[i]
Signal1[i]
Signal2[i]
Signal3[i]
```

Fusion produces:

```text
FinalRank[i]
```

Only after fusion should the result ordering be changed.

Lower final rank sorts first.

Provide deterministic tie behavior.

Prefer `ImageID` as the final deterministic tie-breaker unless repository conventions indicate otherwise.

---

# Top-K

Design so search does not require sorting all candidates when only a small result limit is requested.

For example:

```text
10,000 candidates
       ↓
ranking
       ↓
fusion
       ↓
Top-K selection
       ↓
sort selected K
```

rather than:

```text
sort 10,000
       ↓
take first 100
```

Do not prematurely implement a complicated Top-K algorithm if the repository already provides suitable primitives.

Benchmark realistic sizes:

```text
N = 100
N = 1,000
N = 10,000
```

and useful limits such as:

```text
K = 10
K = 100
K = 1,000
```

---

# SIMD

The representation must be SIMD-friendly from the beginning.

Do not expose SIMD details through the public API.

Keep architecture-specific implementations behind internal kernels.

Conceptually:

```text
ranking/internal/kernel/
    scalar
    amd64
    arm64
```

Follow existing repository conventions for architecture dispatch.

Likely SIMD candidates include:

- weighted fusion
- normalization
- uint16 signal transformations
- vector similarity calculation where appropriate

Do not write assembly/SIMD simply because it is possible.

First implement the scalar reference implementation and benchmark it.

Only add AVX2/NEON kernels where benchmarks demonstrate a meaningful improvement.

Every SIMD implementation must produce results equivalent to the scalar implementation.

---

# Allocation Requirements

After buffers have sufficient capacity, ranking and fusion hot paths should perform:

```text
0 allocs/op
```

Use caller-owned destination slices.

Prefer APIs shaped like:

```go
func Rank(dst []Rank, ...)
func Fuse(dst []Rank, ...)
```

rather than allocating and returning new slices.

Do not use:

- per-result heap objects
- `interface{}` / `any` in hot paths
- maps in per-image loops
- temporary slices per candidate
- unnecessary closures
- reflection

---

# Tests

Add tests covering at least:

1. Lower rank always means better.
2. Time encoding monotonicity.
3. Geo encoding monotonicity.
4. SigLIP/cosine quantization monotonicity.
5. Exact-match boundaries.
6. Maximum-distance boundaries.
7. Missing values.
8. Fusion determinism.
9. Equal-rank tie handling.
10. Empty input.
11. Single candidate.
12. Maximum expected candidate count.
13. Signal slice length validation.
14. Scalar/SIMD equivalence if SIMD is implemented.

For encoders, add property-style tests where useful:

```text
a < b
=>
EncodeDistance(a) <= EncodeDistance(b)
```

and:

```text
similarityA > similarityB
=>
EncodeSimilarity(similarityA) <= EncodeSimilarity(similarityB)
```

---

# Benchmarks

Create focused benchmarks rather than one giant end-to-end benchmark.

Benchmark at:

```text
100
1,000
10,000
```

candidates.

Measure:

- time ranking
- geo ranking where practical
- cosine quantization
- individual fusion algorithms
- Top-K selection
- final sorting

Report allocations.

Use realistic data distributions rather than only sequential synthetic values.

---

# Initial Package Shape

Use repository conventions first, but the conceptual organization can resemble:

```text
ranking/
    rank.go
    signals.go
    results.go

    encode/
        distance.go
        cosine.go

    fusion/
        weighted.go
        ordinal.go
        rrf.go

    internal/
        kernel/
            scalar.go
            amd64.go
            arm64.go
```

Do not create many tiny packages if that makes the Go API cumbersome. Simplicity is more important than matching this exact tree.

Time/Geo rankers may belong closer to their respective indexes if that avoids dependency cycles.

---

# Important Design Constraint

Keep these three concepts separate:

```text
1. Native metric

   Time   → uint32 seconds
   Geo    → uint32 physical distance
   SigLIP → float32 cosine similarity
   BM25   → relevance score

2. Ranking signal

   uint16
   lower = better
   meaningful only within that signal

3. Final rank

   uint16
   lower = better
   produced by a fusion algorithm
```

Do not introduce persistent native-score arrays merely for architectural symmetry.

Calculate native values and immediately encode to `uint16` wherever possible.

---

# Implementation Order

Implement incrementally:

1. `Rank`, `Results`, and `Signals`.
2. Caller-owned signal-buffer construction.
3. Scalar cosine/SigLIP quantization.
4. Scalar integer distance encoding.
5. Time ranker or time encoding integration.
6. Geo ranker/encoding integration if dependencies are already available.
7. One simple, well-defined fusion implementation.
8. Deterministic final sorting.
9. Top-K selection if justified by benchmarks.
10. Additional fusion algorithms.
11. SIMD only after scalar benchmarks establish useful targets.

After each stage, run tests and benchmarks.

Do not make unrelated repository changes.

---

# Deliverables

When finished, provide:

- implementation
- unit tests
- benchmarks
- benchmark allocation counts
- brief documentation of ranking invariants
- explanation of any deviations from this proposal
- benchmark evidence for any SIMD implementation
- any unresolved design questions discovered while integrating with the existing indexes

Most importantly, prefer a small understandable implementation over abstractions intended for hypothetical future rankers.