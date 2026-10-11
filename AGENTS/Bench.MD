# imageIdx Benchmarking Implementation Instructions

Implement a shared benchmarking and performance-testing framework for imageIdx, inspired by the methodology used by kvbench/tamnd.

The goal is not to copy kvbench code. Copy the important benchmarking concepts and adapt them to imageIdx's specialized indexes.

## Goals

The benchmark suite must measure more than average `ns/op`.

For BitmapIdx, TimeIdx, HashIdx, and eventually GeoIdx/VectorIdx/Search, measure:

- throughput / ops/sec
- latency
- p50
- p95
- p99
- p99.9 where practical
- maximum latency
- allocations/op
- bytes allocated/op
- memory used by the index
- bytes per indexed ImageID
- on-disk size
- startup/open latency
- recovery latency where applicable
- write amplification where measurable
- read/write mixed-workload behavior

Benchmarks must be deterministic and reproducible.

Do not optimize implementations as part of this work unless necessary to make the benchmark harness usable.

---

# 1. Shared Benchmark Package

Create a reusable internal benchmark package rather than implementing unrelated benchmark infrastructure inside every index.

Follow existing repository conventions when choosing the exact package path.

Conceptually:

```text
internal/bench/
    dataset.go
    distribution.go
    latency.go
    workload.go
    metrics.go
```

Individual packages should then contain their actual benchmarks:

```text
bitmapidx/*_test.go
timeidx/*_test.go
hashidx/*_test.go
```

Do not introduce unnecessary abstraction.

The shared package should contain only functionality genuinely shared by multiple indexes.

---

# 2. Deterministic Datasets

All generated datasets must be reproducible.

Use fixed seeds.

Support at least these dataset sizes where practical:

```text
100
1,000
10,000
100,000
1,000,000
10,000,000
```

Do not automatically run extremely large benchmarks during normal `go test`.

Large benchmarks should require explicit benchmark execution.

ImageIDs should be generated deterministically.

Test both:

```text
dense IDs
1, 2, 3, 4, ...

sparse IDs
large gaps / realistic uint64 values
```

Do not use ImageID 0 if imageIdx reserves it.

---

# 3. Workload Distributions

Do not benchmark only uniform random data.

Support deterministic versions of:

```text
Sequential
Uniform random
Zipf / hot-set
Clustered
```

The hot-set workload is particularly important.

Example:

```text
80% of queries access 20% of indexed data
```

This approximates repeated searches over common people, albums, dates, etc.

Avoid expensive random-number generation inside the measured hot loop when it would distort the operation being benchmarked.

Precompute workloads where appropriate.

---

# 4. Separate Construction and Query Benchmarks

Do not mix index construction cost into lookup benchmarks.

Measure separately:

```text
Build
Insert
Batch Insert
Update
Delete
Compaction
Open
Recovery

Lookup
Range
Intersection
Search
```

The benchmark name must clearly identify what is being measured.

---

# 5. Allocation Testing

Hot read/query operations should target:

```text
0 allocs/op
```

where the API is designed to permit this.

Use:

```go
b.ReportAllocs()
```

for benchmarks.

Also add explicit allocation tests for critical operations using:

```go
testing.AllocsPerRun(...)
```

where appropriate.

Examples:

```text
Bitmap intersection
Time range query
CryptoHash exact lookup
pHash/PDQ search
```

A regression from zero allocations should be detectable by tests where zero allocation is an intended invariant.

---

# 6. Caller-Owned Destination Buffers

Benchmark APIs using reusable caller-owned buffers.

Prefer APIs conceptually like:

```go
Range(dst []ImageID, ...)
Lookup(dst []ImageID, ...)
Search(dst []ImageID, ...)
```

Do not allocate a new result slice on every operation when the production API supports buffer reuse.

Benchmark both:

```text
adequately sized destination buffer
destination requiring growth
```

but treat the preallocated case as the hot-path performance measurement.

---

# 7. Latency Distribution

Go benchmark `ns/op` is not sufficient for realistic mixed workloads.

Add an optional workload runner capable of recording latency distributions.

Measure at minimum:

```text
p50
p95
p99
max
```

and preferably:

```text
p99.9
```

for sufficiently large workloads.

Do not make histogram collection part of ordinary microbenchmarks if its overhead materially changes the operation being measured.

Keep:

```text
microbenchmarks
```

and:

```text
workload/latency benchmarks
```

separate.

---

# 8. Avoid Coordinated Omission

For realistic latency/load testing, use an open-loop workload where appropriate.

Requests should be scheduled according to the target arrival rate rather than issuing the next request only after the previous request finishes.

This ensures a 50 ms stall appears as a latency problem rather than merely reducing the number of generated requests.

Use this primarily for:

```text
mixed read/write workloads
compaction-under-load
background maintenance
```

Do not complicate simple CPU microbenchmarks with open-loop scheduling.

---

# 9. BitmapIdx Benchmarks

Benchmark at least:

```text
Lookup bitmap
Contains
AND / intersection
OR / union
AND-NOT
multi-way intersection
cardinality
iteration
```

Test bitmap/container characteristics:

```text
very sparse
sparse
medium density
dense
```

Test:

```text
1 bitmap
2 bitmaps
4 bitmaps
8 bitmaps
16 bitmaps
```

where relevant.

For multi-way intersection, include cases where:

```text
first bitmap is smallest
middle bitmap is smallest
last bitmap is smallest
```

This helps evaluate planner/order sensitivity.

Benchmark the existing container types independently where possible.

Compare:

```text
scalar
AVX2
NEON
```

against the same deterministic data.

SIMD and scalar implementations must return identical results.

---

# 10. TimeIdx Benchmarks

`timeidx` supports exact half-open `[start, end)` search and `EstimateRange`;
it has no nearest-before/after API. Benchmark:

```text
exact hit
exact miss
small range
medium range
large range
full range
repeated time spanning many shards
memory Apply (1 / 8 / 32 / 64 mutations)
persistent Apply (durable append, historical move, new-ID insert)
clean reopen
concurrent search during commits
```

Test time distributions:

```text
uniform
clustered
bursty
mostly recent
multi-year
dense repeated timestamps
```

Test ID distributions:

```text
sequential
permuted (random)
```

Sweep `MaxShardBytes` (4 KiB default through 1 MiB) for historical rewrite
volume, wide-search cost and directory size.

Measure both:

```text
number of records examined
number of results returned
```

where practical.

Test queries that:

```text
miss entirely
hit one shard
cross two shards
cross many shards
cover entire index
```

Shard `MinID`/`MaxID` prune unaffected scans; timestamp columns use binary
search, not a block min/max walk.

---

# 11. CryptoHashIdx Benchmarks

For SHA-256/BLAKE3 exact lookup benchmark:

```text
existing key
missing key
hot key
cold key
```

Test multiple dataset sizes.

Measure:

```text
lookup latency
allocations
resident index memory
bytes/hash
```

If implementing sharding/fingerprints, separately measure:

```text
shard lookup
fingerprint hit
fingerprint collision requiring full comparison
miss
```

Correctness must always verify the complete cryptographic hash before returning an ImageID.

A fingerprint collision must never produce a false match.

---

# 12. Perceptual Hash Benchmarks

Treat pHash64/PDQ separately from cryptographic hashes.

Benchmark:

```text
exact Hamming match
small Hamming threshold
medium threshold
no match
Top-K nearest
```

Compare:

```text
scalar
AVX2
NEON
```

where implementations exist.

Measure:

```text
hashes examined/sec
ns/hash
query latency
allocations
```

Use deterministic hashes with controlled Hamming distances so correctness is independently verifiable.

---

# 13. Mixed Workloads

Create longer-running workload tests approximating real image-library behavior.

Examples:

```text
95% reads / 5% writes
90% reads / 10% writes
80% reads / 20% writes
```

Also test:

```text
bulk import while searching
background compaction while searching
deletion while searching
```

The primary metric here is not maximum throughput.

Measure whether maintenance causes tail-latency spikes:

```text
p50
p95
p99
max
```

Readers must continue seeing valid published snapshots throughout these tests.

---

# 14. Concurrency Scaling

Benchmark read workloads at:

```text
1 goroutine
2
4
8
16
GOMAXPROCS
```

where meaningful.

Report scaling rather than assuming more goroutines improve performance.

Specifically look for:

```text
locks
cache-line contention
false sharing
atomic contention
memory-bandwidth saturation
```

Do not introduce parallelism solely to improve benchmark numbers.

---

# 15. Memory Measurements

For each index type, provide a way to estimate or report:

```text
total resident index bytes
bytes per ImageID
bytes per entry
directory/index overhead
```

For relevant structures distinguish:

```text
hot/resident metadata
```

from:

```text
mmap/on-disk immutable data
```

For example:

```text
HashIdx

resident fingerprint table: X MB
mmap hash data:             Y MB
total:                      Z MB
bytes/hash:                 N
```

This is important because reducing RAM can be more valuable than improving an already-fast lookup by a few nanoseconds.

---

# 16. Disk Space

For persistent indexes measure:

```text
logical data size
physical file size
bytes/ImageID
space amplification
```

Measure after:

```text
initial build
updates
deletes
compaction
```

This should make storage regressions visible.

---

# 17. Startup and Recovery

Persistent indexes must have benchmarks/tests for:

```text
clean open
large-index open
open after unclean shutdown
replay mutations
recover from previous generation
```

Measure recovery time as dataset size increases.

Test at least representative sizes such as:

```text
100K
1M
10M
```

when practical.

Do not include giant recovery benchmarks in ordinary unit-test execution.

---

# 18. Correctness Before Performance

Every optimized implementation must have a simple reference implementation.

Examples:

```text
Bitmap scalar reference
Time range reference
Hash linear/reference lookup
Hamming-distance scalar reference
```

Use deterministic and randomized differential tests:

```text
optimized(input) == reference(input)
```

SIMD implementations must be bit-for-bit equivalent to scalar semantics.

Performance improvements are not acceptable if they change results.

---

# 19. Adversarial Cases

Include deliberately hostile datasets:

```text
empty index
single entry
all values identical
all timestamps identical
maximum uint64 values
very sparse IDs
very dense IDs
huge bitmap
tiny bitmap
all misses
all hits
fingerprint collisions
range exactly on block boundary
range one unit across boundary
deleted IDs
```

Include boundary conditions around SIMD vector widths and container/page sizes:

```text
N-1
N
N+1
```

where N is the relevant width/block boundary.

---

# 20. Benchmark Naming

Use consistent benchmark names so results can be compared with `benchstat`.

For example:

```text
BenchmarkBitmap/Intersect/N=1000/Density=Sparse
BenchmarkBitmap/Intersect/N=100000/Density=Dense

BenchmarkTime/Range/N=1000000/Range=Small
BenchmarkTime/Range/N=1000000/Range=Large

BenchmarkHash/SHA256/N=1000000/Hit
BenchmarkHash/SHA256/N=1000000/Miss

BenchmarkPDQ/N=1000000/Threshold=8
```

Make benchmark output machine-comparable across commits.

---

# 21. Benchstat-Friendly Workflow

Design benchmarks so results can be collected with:

```bash
go test -run='^$' -bench=. -benchmem -count=10 ./...
```

and compared using `benchstat`.

Do not put nondeterministic timestamps, random labels, or other unstable values into benchmark names.

---

# 22. Prevent Compiler Elimination

Ensure benchmark results are consumed so the compiler cannot optimize the operation away.

Use package-level sinks only when necessary.

Prefer validating/consuming meaningful result values rather than introducing unnecessary global state.

---

# 23. Keep Setup Outside Timed Region

For microbenchmarks:

```go
// construct index
// generate workload
// allocate dst

b.ResetTimer()

for b.Loop() {
    // operation being measured
}
```

Do not accidentally benchmark:

```text
random data generation
index construction
allocation of test data
file setup
validation
```

unless that is explicitly the benchmark's purpose.

---

# 24. CPU and Memory Profiles

Make benchmarks easy to run with:

```bash
-cpuprofile
-memprofile
```

Do not optimize based only on intuition.

Before introducing complex SIMD, custom hash tables, sharding, prefetching, or cache-layout changes, establish:

```text
baseline
profile
hypothesis
implementation
benchmark
comparison
```

---

# 25. Performance Regression Philosophy

Do not encode extremely tight nanosecond thresholds into ordinary tests.

CI machines vary too much.

Use correctness tests for hard invariants such as:

```text
zero allocations
correct result
no corruption
```

Use benchmark history/benchstat for throughput and latency regressions.

---

# 26. Required Initial Benchmark Matrix

Start with these high-value benchmarks rather than attempting every possible combination.

```text
BitmapIdx
---------
N = 1K, 10K, 100K, 1M
sparse + dense
2-way + 4-way intersection
scalar + available SIMD


TimeIdx
-------
N = 1K, 10K, 100K, 1M
small + medium + large range
uniform + clustered timestamps
memory + persistent 64-mutation Apply


CryptoHashIdx
-------------
N = 1K, 10K, 100K, 1M
hit + miss
SHA256 + BLAKE3 if both implemented


Perceptual HashIdx
------------------
N = 1K, 10K, 100K, 1M
pHash64
PDQ256
exact + representative Hamming threshold
scalar + available SIMD
```

Expand only after these benchmarks are stable.

---

# 27. Primary Design Principle

The benchmark suite should answer:

```text
How fast is it?
How predictable is it?
How much RAM does it use?
How much disk does it use?
Does it allocate?
How does it scale?
What happens during writes/compaction?
How quickly does it recover?
Is the optimized implementation exactly correct?
```

Do not optimize imageIdx solely for maximum operations/sec.

For an interactive photo-search system, predictable search latency, zero-allocation hot paths, compact resident state, and reliable recovery are at least as important as peak throughput.

The benchmark framework should make those tradeoffs visible.