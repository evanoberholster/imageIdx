# VectorIdx Design

> **Design note / experimental.** Vector APIs, performance, and file formats are
> experimental in V1. Current implementation docs:
> `index/vectoridx/README.md`. Do not treat this proposal as a V1 API contract.

## 1. Purpose

`VectorIdx` is a persistent, mmap-oriented approximate-nearest-neighbor vector index for the image database. It is designed for Go, HNSW graph search, fixed-width vectors, direct application NodeIDs, predictable memory use, pooled search state, and SIMD-accelerated Euclidean/L2 distance calculations.

The initial target is up to approximately **10 million vectors per VectorIdx** while keeping the implementation simple enough to benchmark, reason about, recover, compact, and optimize incrementally.

The design deliberately avoids a second application-level ID mapping. A VectorIdx `SpaceID` defines the meaning of its `NodeID`:

- Image vector index: `NodeID == ImageID`
- Face vector index: `NodeID == FaceID`
- Future index types may assign another application identifier to `NodeID`

The VectorIdx stores and operates on that NodeID directly.

## 2. Design Goals

1. Persistent HNSW ANN search over mmap-backed data.
2. Support approximately 10M vectors per index.
3. Direct `uint32` NodeIDs and `uint32` HNSW neighbor references.
4. Fixed dimensions of 512, 768, or 1024 per index.
5. One fixed element type per index.
6. Support `int8`, `uint8`, `int16`, `uint16`, and `float32` vectors.
7. Euclidean/L2 distance.
8. Scalar, AVX2, and NEON distance kernels with runtime dispatch.
9. No AVX-512 requirement in the initial implementation.
10. Single writer per graph and lock-free access to published mmap generations.
11. WAL-backed mutation and explicit checkpoint/rebuild/compaction.
12. Tombstone deletion without immediate graph rewrites.
13. Pooled search scratch state with minimal steady-state allocation.
14. Composition with BitmapIdx filtering after ANN candidate discovery.
15. Single logical file divided into independently addressable regions.
16. Prefer contiguous data, sequential access, low pointer chasing, and simple structures.
17. Add complexity only when benchmarks demonstrate a benefit.

## 3. Non-Goals for V1

The initial implementation should not attempt to solve all future vector-search requirements.

Specifically, V1 does not require:

- Product quantization.
- Scalar quantization of stored vectors.
- INT6 or other packed vector formats.
- Mixed element types inside one VectorIdx.
- Mixed dimensions inside one VectorIdx.
- Cosine similarity or dot-product search.
- AVX-512 kernels.
- Distributed search.
- Multiple concurrent graph writers.
- In-place removal of deleted nodes from HNSW edges.
- Automatic background compaction.
- Bitmap-constrained HNSW graph traversal.
- A duplicate NodeID-to-ImageID/FaceID mapping maintained by VectorIdx.

These can be considered later if benchmarks or product requirements justify them.

---

## 4. Core Types

### 4.1 NodeID

```go
type NodeID uint32
```

NodeID is the identifier stored in HNSW edges and returned by search.

Its semantic meaning comes from the VectorIdx `SpaceID`; VectorIdx does not translate it into another application ID.

NodeIDs are not required to be dense.

The implementation MUST NOT assume that allocating memory proportional to `max(NodeID)` is always acceptable. Dense-ID optimizations may be implemented when benchmarks demonstrate they are worthwhile, but sparse IDs must remain a supported property of the format.

### 4.2 SpaceID

```go
type IndexType uint8
type SpaceID uint16
```

`SpaceID` packs an IndexType (`IndexTypeImage`, `IndexTypeFace`, `IndexTypeCustom`) in its high byte
and a within-type space number in its low byte. It identifies the vector
space and the meaning of its NodeIDs: examples include image semantic
embeddings, face embeddings, landmark embeddings, or future model-specific
vector spaces.

SpaceID does not imply vector dimensions or element representation by itself. Those properties are persisted explicitly in the VectorIdx header.

### 4.3 Dimensions

V1 supports exactly:

```go
const (
    Dim512  = 512
    Dim768  = 768
    Dim1024 = 1024
)
```

A VectorIdx has exactly one dimension for its lifetime. Changing dimensions requires creating/rebuilding an index.

### 4.4 Element Types

```go
type ElementType uint8

const (
    ElementInt8 ElementType = iota + 1
    ElementUint8
    ElementInt16
    ElementUint16
    ElementFloat32
)
```

One VectorIdx contains exactly one element type.

Vectors are stored **as received**. VectorIdx does not normalize or quantize them during insertion.

Changing element type requires rebuilding the index.

---

## 5. Distance Metric

V1 supports Euclidean/L2 distance only.

For ranking, the implementation SHOULD use **squared L2 distance**:

```text
D(a,b) = Σ (a[i] - b[i])²
```

The square root is monotonic and therefore unnecessary for nearest-neighbor ordering. Avoiding it reduces work in the search hot path.

The internal accumulator MUST have sufficient width/precision for the element type and dimensions.

Suggested starting points:

- `int8` / `uint8`: wide integer accumulator, with overflow behavior proven for maximum dimension.
- `int16` / `uint16`: use a sufficiently wide accumulator; do not assume 32-bit accumulation is safe.
- `float32`: float32 SIMD accumulation may be used if ranking accuracy is demonstrated acceptable; benchmark against wider/reference implementations as part of correctness testing.

The SIMD API should expose distance in a representation appropriate to the kernel while keeping HNSW comparison cheap.

---

## 6. Search Result Score

The externally retained result score may be represented as `uint16` or `uint32` rather than exposing the raw accumulator.

Recommended initial API:

```go
type Result struct {
    NodeID NodeID
    Score  uint32
}
```

`uint32` is the preferred V1 default because it provides substantially more ranking resolution while adding only two bytes relative to `uint16` before normal struct alignment is considered.

Score encoding MUST be monotonic with distance:

```text
smaller L2 distance => smaller Score
```

Score quantization is separate from stored-vector quantization. V1 does **not** quantize stored vectors.

The implementation should isolate score encoding behind a small function so `uint16`, `uint32`, or a future score representation can be benchmarked without rewriting HNSW.

Do not quantize intermediate distances prematurely during HNSW traversal. Maintain enough internal precision to preserve graph-search quality and convert to the public score at the result boundary.

---

## 7. High-Level Architecture

A VectorIdx consists conceptually of:

```text
+--------------------------------------------------+
| Superblock / Header                              |
+--------------------------------------------------+
| Region Directory                                 |
+--------------------------------------------------+
| Node / Vector Metadata Region                    |
+--------------------------------------------------+
| Vector Data Region                               |
+--------------------------------------------------+
| HNSW Node Metadata Region                        |
+--------------------------------------------------+
| HNSW Edge / Layer Region                         |
+--------------------------------------------------+
| Tombstone Bitmap Region                          |
+--------------------------------------------------+
| WAL / Checkpoint Metadata                        |
+--------------------------------------------------+
```

These are logical regions. Exact ordering can change as the implementation evolves because offsets and lengths are stored in the region directory.

The design should avoid embedding assumptions such as `edges immediately follow vectors` into the reader.

---

## 8. Single-File Multi-Region Format

Use one logical VectorIdx file per vector space.

Advantages:

- One unit to open, move, validate, checkpoint, and compact.
- Straightforward generation/version validation.
- mmap regions can still be addressed independently.
- Easier crash recovery than coordinating several independently replaced files.

Every persistent structure MUST use explicitly sized integer fields. Do not persist Go structs by directly casting arbitrary in-memory structs onto disk unless layout, alignment, padding, and endian behavior are explicitly controlled.

Use a fixed endian representation, preferably little-endian.

### 8.1 Superblock

A conceptual superblock should contain at least:

```go
type Superblock struct {
    Magic         [8]byte
    Version       uint32
    HeaderSize    uint32

    Generation    uint64
    SpaceID        uint16
    ElementType   uint8
    Metric        uint8
    Dimensions    uint16

    M             uint16
    M0            uint16
    EfConstruction uint32
    DefaultEfSearch uint32

    EntryNode     uint32
    MaxLevel      uint16

    LiveCount     uint64
    TombstoneCount uint64

    RegionDirOffset uint64
    RegionDirLength uint64

    CheckpointLSN uint64

    // checksum / reserved fields
}
```

This is illustrative rather than a required byte-for-byte Go struct.

Reserve sufficient header space for future fields without requiring immediate format migration.

### 8.2 Region Directory

Each region should have a descriptor similar to:

```go
type Region struct {
    Kind       uint16
    Flags      uint16
    Offset     uint64
    Length     uint64
    Capacity   uint64
    Generation uint64
    Checksum   uint64
}
```

Useful region kinds include:

```text
NodeMetadata
VectorData
HNSWMetadata
HNSWEdges
Tombstones
WAL
```

Align hot regions to at least page boundaries where useful. Vector blocks should additionally respect alignment requirements useful to SIMD loads.

---

## 9. Physical Node Lookup

NodeID is externally meaningful and may be sparse, but vector data should remain physically compact.

Therefore the implementation needs a way to resolve:

```text
NodeID -> physical node/vector record
```

without creating a duplicate application identity mapping.

This is a **storage locator**, not a semantic NodeID mapping.

V1 should benchmark two approaches before committing to a complicated structure:

### Option A: Sorted Node Metadata

Store compact node metadata sorted by NodeID and use binary search.

Advantages:

- Simple.
- Compact.
- mmap-friendly.
- No memory proportional to max NodeID.

Disadvantage:

- Additional lookup cost during graph traversal.

### Option B: Dense Locator Table

When the NodeID range is sufficiently dense, maintain a direct locator array indexed by NodeID.

Advantages:

- O(1) lookup.
- Very simple hot path.

Disadvantage:

- Memory proportional to maximum NodeID rather than node count.

### Recommendation

Implement the simple sorted representation first unless existing database invariants guarantee a sufficiently dense NodeID range.

Benchmark lookup cost under realistic HNSW traversal before introducing a more complex sparse hash/radix structure.

The on-disk format should allow the locator strategy to evolve independently from HNSW edges.

---

## 10. Vector Storage

Vectors should be stored densely and contiguously in physical storage even when NodeIDs are sparse.

For a given VectorIdx:

```text
VectorBytes = Dimensions * ElementSize
```

Examples:

| Format | 512D | 768D | 1024D |
|---|---:|---:|---:|
| int8 / uint8 | 512 B | 768 B | 1024 B |
| int16 / uint16 | 1024 B | 1536 B | 2048 B |
| float32 | 2048 B | 3072 B | 4096 B |

At 10M vectors, raw vector data alone is substantial. For example, 768-dimensional float32 vectors require roughly 30.7 GB decimal before graph/metadata overhead. This makes mmap behavior and sequential physical vector packing important.

Each physical vector slot corresponds to one node metadata record. NodeID does not need to determine its physical byte offset directly.

Avoid per-vector heap allocations.

---

## 11. HNSW Graph

VectorIdx uses HNSW as its initial ANN algorithm.

Configurable persistent parameters:

```go
type HNSWConfig struct {
    M              uint16
    M0             uint16
    EfConstruction uint32
    EfSearch       uint32
}
```

Recommended defaults should be selected through benchmarks rather than treated as format constants.

A reasonable benchmark starting point is:

```text
M = 16
M0 = 32
efConstruction = 100-200
efSearch = workload dependent
```

These are starting points only.

### 11.1 Neighbor IDs

Every HNSW edge stores:

```go
type NeighborID uint32
```

and its value is the target NodeID.

Do not store 64-bit edge IDs for V1.

### 11.2 Node Metadata

Graph metadata should identify at least:

- NodeID
- physical vector location
- maximum HNSW level
- location of its neighbor lists

Do not use Go pointers in persistent graph structures.

### 11.3 Edge Storage

Prefer contiguous edge lists over linked allocations.

Conceptually:

```text
Node metadata
    -> Layer descriptor(s)
        -> contiguous uint32 neighbor IDs
```

The exact encoding should be benchmark-driven.

Important properties are:

- Neighbor iteration is sequential.
- No heap allocation during traversal.
- No persistent pointers.
- Neighbor counts are cheaply available.
- Rebuild can rewrite graph storage densely.

Do not prematurely implement compressed edge encodings. `uint32` neighbor IDs provide a simple baseline against which compression can later be measured.

---

## 12. Mutation Model

Only one writer may mutate/build a particular VectorIdx graph at a time.

The public/searchable mmap generation is treated as immutable by readers.

Conceptually:

```text
WAL + writer state
        |
        v
build/checkpoint/rebuild
        |
        v
new consistent generation
        |
        v
atomic publication
        |
        v
lock-free mmap readers
```

Search never mutates persistent index data.

This separation is preferred over attempting to make arbitrary in-place mmap HNSW modifications safe for concurrent readers.

---

## 13. WAL

Adds and deletes are recorded in a write-ahead log.

Minimum logical records:

```text
AddVector(NodeID, vector)
DeleteNode(NodeID)
Checkpoint(...)
```

Each WAL record should include:

- record type
- length
- sequence/LSN
- payload
- checksum

An AddVector payload contains the NodeID and vector bytes in the index's configured representation.

Deletion records only need the NodeID plus WAL framing metadata.

### 13.1 Durability

An acknowledged durable mutation should have its WAL record safely persisted according to the database's durability contract. `Create` and `Open` take a `Durability` (`DurabilityOS` or `DurabilitySync`); it is a runtime policy and is not persisted in the file format. `DurabilitySync` fsyncs each acknowledged mutation; `DurabilityOS` leaves the record to the OS page cache, optionally with `WithSyncInterval(d)` periodic group commit. `Sync` forces a durable boundary; the index path is exclusively locked until `Close`.

Checkpoints are incremental: `Checkpoint` publishes a delta generation and updates the manifest atomically; `Vacuum` merges base and deltas into a fresh base. The WAL is segmented and rotates at `WithWALSegmentBytes`; a checkpoint removes every non-active segment. A delta is durable before the manifest names it, and the WAL is truncated only after both, so orphan deltas are ignored and reclaimed by the next vacuum.

Graph/vector checkpointing may lag WAL persistence.

### 13.2 Recovery

On open:

1. Validate the published checkpoint/generation.
2. Locate `CheckpointLSN`.
3. Scan WAL records after that LSN.
4. Stop safely at the first incomplete/corrupt trailing record.
5. Replay valid Add/Delete operations into writer state.
6. Re-establish a consistent mutable state before accepting new writes.

Recovery must be idempotent.

---

## 14. Add

Conceptual API:

```go
func (idx *VectorIdx) Add(nodeID NodeID, vector []T) error
```

Validation:

- NodeID is valid for the surrounding database.
- Vector dimension exactly matches the index.
- Vector element type matches the index API.
- Duplicate live NodeID behavior is explicit; V1 should reject accidental duplicate adds unless replacement semantics are deliberately implemented.

Logical flow:

```text
Validate
   -> append WAL Add
   -> update writer vector state
   -> HNSW insertion
   -> eventually checkpoint/publish
```

HNSW graph insertion is serialized through the one writer.

Distance calculations during construction may later be parallelized if benchmarks show a benefit, but graph mutation should remain logically serialized in V1.

---

## 15. Delete

Deletion is logical.

```go
func (idx *VectorIdx) Delete(nodeID NodeID) error
```

Flow:

```text
append WAL Delete
    -> add NodeID to tombstone bitmap
```

Do not immediately remove inbound or outbound HNSW edges.

A tombstoned node:

- MAY still be traversed during graph navigation.
- MUST NOT be emitted as a result.
- MUST be removed during rebuild/compaction.

This keeps deletion cheap and avoids complex graph surgery.

---

## 16. Tombstone Bitmap

Tombstones are represented as a bitmap keyed by NodeID.

The bitmap implementation should be compatible with the project's bitmap-index design where practical, but VectorIdx should depend on a narrow bitmap interface rather than graph code knowing the bitmap's internal container format.

Conceptually:

```go
type Tombstones interface {
    Contains(NodeID) bool
}
```

The hot-path `Contains` operation must be allocation-free.

Deletion does not require a separate tombstone flag in every HNSW node record.

---

## 17. Search API

Conceptual public API:

```go
func (idx *VectorIdx) Search(
    query Vector,
    candidates *Bitmap,
    k int,
) ([]Result, error)
```

A lower-level allocation-conscious API should also be considered:

```go
func (idx *VectorIdx) SearchInto(
    dst []Result,
    query Vector,
    candidates *Bitmap,
    k int,
) ([]Result, error)
```

`SearchInto` allows callers to reuse result storage and is preferable for hot paths.

Internally, search borrows a `SearchContext` from a pool.

---

## 18. SearchContext Pool

Search should avoid allocating temporary heaps, visited structures, and buffers on every query.

Use one pooled object that owns the complete scratch state:

```go
type SearchContext struct {
    candidates nodeHeap
    results    nodeHeap

    // One of several benchmarked visited representations.
    visitedBitmap []uint64
    visitedTags   []uint32
    generation    uint32

    // Reusable candidate/result/filter buffers.
    nodes   []searchNode
    output  []Result
}
```

The exact fields should follow implementation benchmarks.

Use a pool such as:

```go
var searchPool sync.Pool
```

The intended lifecycle is:

```text
Get SearchContext
    -> reset logical lengths/state
    -> perform search
    -> copy/return caller-owned results as necessary
    -> Put SearchContext
```

Never return slices whose backing memory is immediately returned to the pool unless ownership is explicitly transferred.

`SearchInto` is useful because it allows the scratch context to remain completely internal.

---

## 19. Visited Set

Two implementations should be benchmarked.

### 19.1 Reusable Bitmap

For 10M nodes, a dense bitset requires approximately:

```text
10,000,000 / 8 ~= 1.25 MB
```

Advantages:

- Compact.
- Cache-friendly relative to a tag array.
- Simple.

Disadvantage:

- Must clear touched/all bits between searches unless using a touched-word strategy.

### 19.2 Generation Array

```go
visited []uint32
queryGeneration uint32
```

A node is visited when:

```go
visited[physicalOrdinal] == queryGeneration
```

Advantages:

- No bitmap clearing.
- Very cheap check/set.

Disadvantages:

- About 40 MB per 10M-entry context.
- Poor fit if multiple SearchContexts exist.
- Requires generation rollover handling.

### 19.3 Benchmark Requirement

Do not choose based solely on theoretical operation count.

Benchmark:

- end-to-end HNSW QPS
- latency distribution
- cache behavior where measurable
- memory per SearchContext
- reset cost
- candidate count
- `efSearch` sensitivity

Also benchmark a touched-word/touched-block bitmap reset strategy before accepting the memory cost of generation arrays.

---

## 20. BitmapIdx Candidate Filtering

`candidatesBitmap` does **not** constrain HNSW graph traversal in V1.

Instead:

```text
HNSW ANN traversal
       |
       v
ranked ANN candidates
       |
       v
remove tombstones
       |
       v
BitmapIdx membership filtering
       |
       v
final top-k
```

This keeps HNSW navigation independent of potentially restrictive metadata filters.

The candidate bitmap is keyed by the NodeID semantics expected by that VectorIdx. If another database index uses different IDs, translation belongs outside VectorIdx.

### 20.1 Oversampling

If `k = 100` but only 20 of the first ANN candidates exist in `candidatesBitmap`, VectorIdx should continue/expand ANN exploration where possible rather than immediately returning 20.

Search should continue until one of the following occurs:

1. `k` valid results have been collected.
2. The HNSW search budget is exhausted.
3. No additional candidates are available.

The effective search budget should be controllable by `efSearch` and potentially an oversampling policy.

Do not promise exactly `k` results when the candidate bitmap contains fewer than `k` reachable/live matching nodes.

---

## 21. HNSW Search

At a high level:

1. Acquire current published generation.
2. Borrow SearchContext.
3. Validate query dimensions/type.
4. Start at the HNSW entry node.
5. Greedily descend upper layers.
6. Perform `efSearch` exploration at layer 0.
7. Skip tombstoned nodes when producing results, but permit them for traversal.
8. Apply candidate BitmapIdx membership filtering to result candidates.
9. Continue/oversample if filtering leaves fewer than `k` results and budget remains.
10. Sort/finalize top-k.
11. Encode public `uint32` score.
12. Return SearchContext to pool.
13. Release generation reference.

The search function itself does not need to be internally lock-free. Only one process is expected to use a search operation at a time under the current workload assumptions. However, search MUST NOT require locks that mutate or serialize access to the immutable published mmap index.

This distinction should remain explicit:

```text
Search scratch state: mutable and pooled
Published mmap state: immutable and lock-free to read
```

---

## 22. SIMD Distance Kernels

Distance calculation is one of the primary hot paths and should have architecture-specific kernels.

Provide a common internal interface with implementations for:

```text
Scalar
AVX2
NEON
```

AVX-512 is intentionally deferred.

Suggested package structure:

```text
vectoridx/
    distance.go
    distance_scalar.go
    distance_amd64.go
    distance_arm64.go
```

If assembly or generated code is required, keep dispatch and correctness reference implementations separate from architecture-specific kernels.

### 22.1 Kernel Matrix

Implement/benchmark L2 kernels for:

```text
int8    x 512 / 768 / 1024
uint8   x 512 / 768 / 1024
int16   x 512 / 768 / 1024
uint16  x 512 / 768 / 1024
float32 x 512 / 768 / 1024
```

Fixed supported dimensions allow kernels to reduce general-purpose loop overhead and make unrolling easier.

Do not create 15 completely independent implementations if a shared kernel with fixed-size dispatch generates equally good code. Benchmark generated assembly before deciding.

### 22.2 Runtime Dispatch

Resolve the best implementation once rather than checking CPU capabilities inside every distance call.

Conceptually:

```go
type distanceFn func(a, b unsafe.Pointer) internalDistance
```

At index open/init:

```text
amd64 + AVX2 -> AVX2 kernel
arm64        -> NEON kernel
otherwise    -> scalar kernel
```

The scalar implementation is the correctness reference.

### 22.3 Safety

SIMD kernels must be tested against scalar implementations using randomized vectors, edge values, and every supported dimension/type.

Integer overflow must be explicitly analyzed and tested.

---

## 23. HNSW Construction

HNSW construction uses the same distance dispatch as search.

One writer owns structural mutation.

Insertion flow follows standard HNSW behavior:

```text
assign level
    -> enter at current top node
    -> descend/search layers
    -> select neighbors
    -> write reciprocal links as required
    -> publish writer state through checkpoint/generation mechanism
```

Neighbor-selection heuristics should initially follow a known/simple HNSW implementation rather than introducing custom graph heuristics.

Keep the neighbor-selection logic isolated so it can be benchmarked independently later.

---

## 24. Checkpoint and Publication

Published generations are immutable from the reader's perspective.

A checkpoint should produce a self-consistent set of regions plus a superblock that identifies them.

Publication must ensure readers see either:

```text
old valid generation
```

or:

```text
new valid generation
```

and never a partially written combination.

Use an atomic/superblock publication strategy appropriate to the surrounding database format. A dual-superblock scheme with generation numbers and checksums is a good candidate if the database does not already provide an equivalent primitive.

The exact fsync ordering must be specified during implementation and covered by crash-recovery tests.

---

## 25. Generation Lifetime

A search acquires a stable generation before touching mmap-backed graph/vector regions.

When a newer generation is published, the old generation remains mapped until no search can reference it.

Conceptually:

```go
type Generation struct {
    // immutable mmap views
    refs atomic.Int64
}
```

or use the project's existing generation/epoch mechanism if one already exists.

Do not unmap old data immediately after swapping the current root.

Because current search concurrency is limited, keep reclamation simple. Avoid introducing a sophisticated general-purpose epoch reclamation system unless required elsewhere by the database.

---

## 26. Compact / Rebuild

Compaction is explicit in V1:

```go
func (idx *VectorIdx) Compact() error
```

A rebuild:

1. Acquires a stable source generation.
2. Enumerates live vectors.
3. Omits tombstoned NodeIDs.
4. Packs vector storage densely.
5. Rebuilds HNSW from live vectors.
6. Rebuilds node locator/metadata structures.
7. Creates an empty/new tombstone bitmap.
8. Establishes a new checkpoint LSN.
9. Persists and validates the new generation.
10. Atomically publishes it.
11. Retires the old generation after readers finish.

### Critical invariant

**Compaction MUST NOT renumber NodeIDs.**

Physical vector positions and graph storage locations may change freely, but NodeID remains the application identifier.

Future versions may trigger compaction based on tombstone percentage, graph degradation, WAL size, or storage fragmentation, but V1 should keep compaction explicit and observable.

---

## 27. Sparse NodeID Considerations

Direct NodeIDs simplify integration but create an important storage constraint.

Do not accidentally turn:

```text
10M vectors
```

into memory proportional to:

```text
max(NodeID)
```

without measuring the actual NodeID density.

This particularly affects:

- node locator
- visited sets
- presence tracking
- tombstone representation

Where practical, search scratch structures should be indexed by **physical node ordinal**, not external NodeID. This is not a duplicate semantic ID mapping; it is the physical location already required to store vectors densely.

HNSW edges still store NodeID as required by the design. Resolving an edge to its vector therefore passes through the node locator.

Benchmark whether this lookup becomes sufficiently expensive to justify an optimized locator representation.

---

## 28. Presence vs Tombstones

A tombstone means:

```text
NodeID previously existed and is deleted
```

It does not necessarily prove that an arbitrary sparse NodeID ever existed.

Node existence can be established through the node metadata/locator structure rather than requiring a second dense presence bitmap.

Do not add a separate presence bitmap in V1 unless it improves a measured operation.

The authoritative live condition is conceptually:

```text
node exists in node metadata AND node not in tombstones
```

---

## 29. Memory Allocation Rules

Hot search paths should target zero heap allocations after pool warm-up, excluding caller-requested result ownership when unavoidable.

Avoid:

- per-node allocations
- per-edge allocations
- interface calls inside inner distance loops
- maps in graph traversal
- rebuilding heaps every query
- allocating visited sets per query
- copying vectors out of mmap solely to calculate distance

Prefer:

- slices over contiguous buffers
- reusable binary heaps
- pooled SearchContext
- fixed vector layouts
- pre-resolved distance function
- direct mmap reads where safe

Allocation goals must be verified with Go benchmarks using `-benchmem`.

---

## 30. Suggested Go API

Exact API details can evolve, but keep the public surface small.

```go
type Config struct {
    SpaceID         SpaceID
    Dimensions     uint16
    ElementType    ElementType

    M              uint16
    M0             uint16
    EfConstruction uint32
    EfSearch       uint32
}

type Result struct {
    NodeID NodeID
    Score  uint32
}

type VectorIdx struct {
    // private implementation
}

func Create(path string, cfg Config, durability Durability) (*VectorIdx, error)
func Open(path string, durability Durability) (*VectorIdx, error)
func (v *VectorIdx) Close() error

func (v *VectorIdx) Add(nodeID NodeID, vector any) error
func (v *VectorIdx) Delete(nodeID NodeID) error

func (v *VectorIdx) Search(query any, candidates *Bitmap, k int) ([]Result, error)
func (v *VectorIdx) SearchInto(dst []Result, query any, candidates *Bitmap, k int) ([]Result, error)

func (v *VectorIdx) Checkpoint() error
func (v *VectorIdx) Vacuum() error
func (v *VectorIdx) Compact() error
```

Avoid `any` in the final performance-sensitive implementation if it causes runtime type switching in hot paths. It is shown only to illustrate a common API.

A better implementation may expose typed insertion/search wrappers or an internal specialization selected when the index opens.

For example:

```go
func (v *VectorIdx) SearchFloat32(dst []Result, query []float32, candidates *Bitmap, k int) ([]Result, error)
```

versus a generic convenience layer above it.

Benchmark API dispatch cost before over-engineering generic abstractions.

---

## 31. Error Handling and Validation

Open should validate at least:

- magic
- format version
- dimensions
- element type
- metric
- region bounds
- region alignment where required
- checksums required for structural integrity
- HNSW entry node validity
- checkpoint metadata

Do not perform an O(index-size) full validation on every open unless explicitly requested. Separate fast structural validation from an offline/full integrity checker.

Provide a future command/API equivalent to:

```text
VectorIdx.Verify()
```

for deep validation.

---

## 32. Crash Testing

Persistence correctness is as important as ANN performance.

Build tests that terminate the writer at controlled points around:

- WAL append
- WAL sync
- vector write
- graph edge write
- checkpoint creation
- superblock publication
- old-generation retirement

After restart, the index must recover to a valid prefix of acknowledged operations according to the chosen durability contract.

No crash point should cause a reader to follow arbitrary offsets or corrupt lengths from an incomplete generation.

---

## 33. Benchmark Plan

Do not optimize individual kernels without measuring end-to-end search.

### 33.1 Distance Microbenchmarks

For every supported element type and dimension:

```text
Scalar
AVX2
NEON
```

Measure:

- ns/vector comparison
- vectors/sec
- bytes/sec
- allocations

Test realistic alignment and mmap-backed buffers in addition to ideal heap slices.

### 33.2 HNSW Search Benchmarks

Dataset sizes:

```text
100K
1M
10M where practical
```

Measure:

- p50/p95/p99 latency
- QPS
- recall@k against exact L2 baseline
- distance evaluations/query
- graph nodes visited
- bytes read / page-fault behavior where available
- allocations/query

Vary:

```text
M
M0
efConstruction
efSearch
k
```

### 33.3 Bitmap Filtering Benchmarks

Candidate selectivity should include approximately:

```text
100%
50%
10%
1%
0.1%
```

Measure how much ANN oversampling is required to return `k` filtered results and whether post-filtering remains acceptable for highly restrictive filters.

This benchmark determines whether a future bitmap-aware graph strategy is warranted.

### 33.4 Visited-Set Benchmarks

Compare:

- full-clear bitmap
- touched-word bitmap
- generation/tag array

Use end-to-end HNSW queries, not just isolated bit operations.

### 33.5 Node Locator Benchmarks

Measure:

- sorted metadata + binary search
- dense direct locator when IDs are dense
- any later sparse locator candidate

Test with realistic NodeID distributions.

The metric that matters is end-to-end HNSW latency/recall/memory, not lookup nanoseconds in isolation.

### 33.6 Tombstone Benchmarks

Measure search quality and latency at:

```text
0%
1%
5%
10%
20%
```

tombstoned nodes.

Use results to establish a future recommended compaction threshold.

---

## 34. Exact Search Reference

Implement a simple scalar exact L2 scan for tests and benchmarking.

It provides:

- correctness oracle for distance kernels
- ground truth for HNSW recall
- validation of result ordering
- small-dataset fallback/testing utility

It does not need to be the production search strategy.

A later SIMD exact scan may also be valuable as a benchmark because contiguous scanning can outperform graph traversal at small candidate counts or small datasets. It is explicitly secondary to the initial HNSW design.

---

## 35. Implementation Phases

### Phase 1 — Format and Scalar Core

Implement:

- Superblock and region directory.
- Fixed vector formats/dimensions.
- Dense physical vector storage.
- Node metadata/locator.
- Scalar L2 kernels.
- Exact scan reference.
- Open/create/validation tests.

Do not begin with assembly.

### Phase 2 — In-Memory HNSW Correctness

Implement:

- HNSW insertion.
- HNSW search.
- configurable M/M0/efConstruction/efSearch.
- direct uint32 NodeID edges.
- scalar distance dispatch.
- recall tests against exact search.

Establish graph correctness before mmap optimization.

### Phase 3 — Persistent mmap Graph

Implement:

- persistent graph regions.
- mmap readers.
- generation publication.
- stable old-generation lifetime.
- no-allocation graph traversal where possible.

### Phase 4 — WAL and Tombstones

Implement:

- WAL framing/checksums.
- Add/Delete recovery.
- tombstone bitmap.
- deleted-node traversal/non-return semantics.
- checkpoint LSN.
- crash tests.

### Phase 5 — SearchContext Pool

Implement:

- pooled heaps.
- pooled visited representation.
- `SearchInto`.
- allocation benchmarks.

Target zero steady-state search allocations where practical.

### Phase 6 — BitmapIdx Filtering

Implement:

- post-ANN membership filtering.
- tombstone filtering.
- oversampling/continued search.
- selectivity benchmarks.

Do not implement bitmap-aware graph traversal yet.

### Phase 7 — SIMD

Implement in order:

1. scalar reference
2. AVX2
3. NEON

for supported types/dimensions.

Require bit-for-bit or tolerance-defined correctness against the scalar/reference implementation as appropriate for each numeric type.

### Phase 8 — Compact/Rebuild

Implement:

- live-vector enumeration.
- dense physical rewrite.
- graph rebuild.
- tombstone removal.
- atomic new-generation publication.
- delayed old-generation unmap.

### Phase 9 — Benchmark-Guided Optimization

Only after the above is stable, investigate:

- alternative node locators
- visited-set alternatives
- HNSW edge layout changes
- prefetching
- vector alignment changes
- score uint16 vs uint32
- specialized fixed-dimension kernels
- automatic compaction thresholds
- SIMD exact scanning for small candidate sets
- bitmap-aware ANN strategies

---

## 36. Key Invariants

The implementation should document and test these invariants explicitly.

1. A VectorIdx has one SpaceID, dimension, element type, and metric.
2. NodeID is `uint32` and has application semantics defined by SpaceID.
3. HNSW edges contain NodeIDs, not pointers.
4. Compaction never changes NodeID.
5. Physical vector locations may change between generations.
6. Published generations are immutable to search readers.
7. A reader holds a generation alive for the entire search.
8. Tombstoned nodes may be traversed but never returned.
9. WAL recovery cannot expose a partially published generation.
10. VectorIdx never normalizes or quantizes stored vectors in V1.
11. Search filtering does not modify HNSW traversal semantics in V1.
12. Public score quantization does not replace full-precision/internal-distance comparison during graph traversal.
13. One writer owns graph mutation.
14. Search scratch memory is never stored in persistent mmap regions.
15. No persistent structure contains process-local Go pointers.

---

## 37. Design Decisions to Leave to Benchmarks

The following should intentionally remain benchmark decisions rather than architectural assumptions:

- `M` default.
- `M0` default.
- `efConstruction` default.
- `efSearch` default.
- visited bitmap vs generation array.
- exact visited-bitmap reset strategy.
- sorted vs dense/other NodeID locator.
- `uint16` vs `uint32` public score.
- vector alignment beyond required machine/page alignment.
- HNSW edge packing optimizations.
- compaction threshold.
- whether highly selective BitmapIdx queries justify an alternate search path.
- whether SIMD exact scan should replace HNSW below some candidate/index size.

The benchmark suite should make these decisions reproducible.

---

## 38. Recommended Initial Implementation Choices

Where a choice is needed to begin coding before benchmarks exist, use the simplest baseline:

```text
NodeID:              uint32
Dimensions:          512 / 768 / 1024
Metric:              squared L2
Public Score:        uint32
HNSW M:              16
HNSW M0:             32
efConstruction:      128 or 200 benchmark baseline
efSearch:            configurable, start around 64-128
Edges:               raw uint32 NodeIDs
Node locator:        sorted metadata + binary search
Visited:             reusable bitmap
Deletion:            tombstone only
Filtering:           post-HNSW BitmapIdx membership
Compaction:          explicit
Search memory:       pooled SearchContext
Persistence:         single file, multi-region
Reader model:        immutable mmap generation
Writer model:        one writer + WAL
SIMD:                scalar + AVX2 + NEON
AVX-512:             deferred
```

These are starting points, not permanent format requirements where avoidable.

---

## 39. Implementation Guidance for Codex

When implementing this design:

1. **Preserve simplicity first.** Do not introduce radix trees, elaborate compressed graph formats, custom allocators, or complex lock-free mutation algorithms without a benchmark demonstrating need.
2. **Keep persistent layout separate from Go object layout.** Explicitly encode/decode persistent structures.
3. **Build the scalar implementation first.** Scalar distance is the correctness reference for SIMD.
4. **Keep NodeID semantics direct.** Do not create an unnecessary duplicate ImageID/FaceID mapping inside VectorIdx.
5. **Do allow physical ordinals internally.** Dense vector storage naturally has physical positions; these are storage locations, not new application identities.
6. **Never renumber NodeIDs during compaction.**
7. **Do not constrain HNSW traversal with BitmapIdx in V1.** Filter/oversample results after ANN discovery.
8. **Keep deleted nodes traversable.** Tombstones suppress results; rebuild removes nodes.
9. **Avoid allocations in search.** Use SearchContext and `SearchInto`.
10. **Make SIMD dispatch occur outside the hot loop.**
11. **Do not add AVX-512 yet.**
12. **Benchmark mmap data, not only heap-resident synthetic vectors.**
13. **Measure recall as well as speed.** An HNSW optimization that reduces recall is not automatically an improvement.
14. **Measure memory amplification.** At 10M vectors, seemingly small per-node fields become large.
15. **Keep the file format extensible.** Regions and versioned headers should allow layout changes without coupling every structure together.
16. **Add corruption bounds checks before unsafe mmap access.** Fast paths may rely on validation performed when a generation is opened.
17. **Do not expose pooled memory to callers after returning it to the pool.**
18. **Use benchmarks to decide visited representation and NodeID lookup strategy.**
19. **Keep WAL recovery deterministic and crash-testable.**
20. **Prefer a working, measurable HNSW baseline before specialized optimizations.**

---

## 40. Summary

VectorIdx should begin as a deliberately straightforward persistent HNSW implementation built around five principles:

**Direct identity.** `NodeID` is the ImageID, FaceID, or other application identifier defined by the VectorIdx SpaceID. VectorIdx does not maintain redundant semantic ID lists.

**Dense physical data.** Vectors and graph records remain physically compact even when NodeIDs are sparse. Physical storage locations are implementation details and can change during compaction.

**Immutable reads, simple writes.** One writer records changes through the WAL and builds consistent generations. Searches read immutable mmap generations without modifying persistent state.

**Cheap search state.** Search uses a reusable pooled SearchContext, fixed-width graph edges, and architecture-selected SIMD L2 kernels. Allocation and pointer chasing should be minimized in the hot path.

**Benchmark-driven complexity.** HNSW parameters, visited representation, NodeID locator, score width, compaction policy, and future filtered-search optimizations should be selected using realistic 100K/1M/10M datasets rather than assumptions.

This provides a simple baseline that can later absorb more sophisticated vector formats, quantization, filtering strategies, or graph optimizations without making those features prerequisites for the first correct and performant implementation.
