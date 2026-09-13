# HashIdx Implementation Proposal

## Purpose

`hashidx` is a persistent, mmap-backed perceptual-hash index for image similarity and duplicate detection.

V1 stores three fixed-width columns using a shared Structure of Arrays (SoA) ordinal space:

- `ImageID uint64`
- `pHash64 uint64`
- `PDQ256 [4]uint64`

The index is optimized for:

- frequent image inserts,
- append-only writes,
- lock-free reads,
- exact Hamming-distance search,
- compact persistent storage,
- efficient bitmap-restricted search,
- future SIMD acceleration,
- simple crash recovery,
- explicit compaction instead of complex in-place mutation.

Deletes and removals are not represented inside hash pages. Liveness is supplied by `bitmapIdx`.

`hashidx` is the canonical storage for perceptual hashes. Future search accelerators such as MIH, IVF, VP-tree, or binary HNSW must be treated as secondary/rebuildable structures rather than changing the canonical persistent hash layout.

---

## Goals

1. Store `ImageID`, `pHash64`, and `PDQ256` once in one shared ordinal space.
2. Make normal ingestion append-only.
3. Avoid rewriting existing pages when new images are added.
4. Keep sealed pages immutable and mmap-friendly.
5. Keep readers lock-free using immutable published views.
6. Use `bitmapIdx` for liveness, tombstones, and candidate filtering.
7. Support exact pHash64 and PDQ Hamming search.
8. Support a bitmap-driven execution path when another index has already reduced the candidate set.
9. Use fixed-width, explicit persistent encoding.
10. Keep V1 scalar and simple, while making the physical layout SIMD-friendly.
11. Permit `Vacuum` to rewrite live data densely into a new generation.
12. Reuse existing `imageidx.ImageID`, durability, reader tracking, mmap, alignment, and persistence conventions.

---

## Non-Goals for V1

V1 does not include:

- HNSW,
- VP-tree,
- BK-tree,
- MIH,
- binary IVF,
- LSH,
- learned embeddings,
- approximate nearest-neighbor search,
- mutable mmap pages,
- in-place deletion,
- page free lists,
- page reuse,
- per-page locks,
- variable-width records,
- generic byte-array hash formats.

These may be evaluated later as secondary search accelerators.

---

# 1. Package

Suggested package:

```text
hashidx/
```

Suggested public index type:

```go
type Index struct {
    // private
}
```

Use the existing shared image ID type:

```go
type ImageID = imageidx.ImageID
```

`ImageID` remains the externally meaningful identity.

Internal physical location is an ordinal.

```go
type Ordinal uint64
```

A `uint64` ordinal is preferred for simplicity and to avoid imposing a 4.29B-record persistent-format limit. Hot loops may use native `int` after checked conversion where safe.

Ordinals are internal only and are not stable across `Vacuum`.

---

# 2. Canonical SoA Layout

All three values use the same ordinal:

```text
ordinal i

ImageIDs[i]
PHash64[i]
PDQ0[i]
PDQ1[i]
PDQ2[i]
PDQ3[i]
```

Conceptually:

```go
type pageView struct {
    imageIDs []imageidx.ImageID

    phash64 []uint64

    pdq0 []uint64
    pdq1 []uint64
    pdq2 []uint64
    pdq3 []uint64
}
```

This is deliberately SoA rather than:

```text
[ImageID][pHash64][PDQ256]
[ImageID][pHash64][PDQ256]
...
```

The SoA representation allows hot search loops to stream only the fields required by the query.

For a pHash search:

```text
PHash64[]
```

is the primary hot column.

For a PDQ search:

```text
PDQ0[]
PDQ1[]
PDQ2[]
PDQ3[]
```

are the primary hot columns.

`ImageIDs[]` only needs to be loaded for surviving candidates/results, or when liveness must be tested through an ImageID-keyed bitmap.

---

# 3. Space Per Image

Raw canonical data per image:

```text
ImageID       8 bytes
pHash64       8 bytes
PDQ256       32 bytes
--------------------
Total        48 bytes
```

Approximate raw storage:

```text
1M images     48 MB
10M images   480 MB
100M images    4.8 GB
```

This avoids storing `ImageID` once for pHash and again for PDQ.

If the two perceptual hashes were independent AoS indexes:

```text
pHash64 + ImageID = 16 bytes
PDQ256  + ImageID = 40 bytes
Total             = 56 bytes/image
```

The shared SoA layout saves 8 bytes per image.

At 10M images this saves approximately 80 MB of raw ID storage.

---

# 4. Fixed Logical Page Size

Start with:

```text
RecordsPerPage = 8192
```

This maps cleanly onto 64 KiB uint64 planes:

```text
8192 × 8 bytes = 65536 bytes
```

One logical page therefore contains:

```text
ImageIDs    64 KiB
pHash64     64 KiB
PDQ0        64 KiB
PDQ1        64 KiB
PDQ2        64 KiB
PDQ3        64 KiB
------------------
Total      384 KiB
```

A full logical page contains 8,192 images.

The logical page is the append/seal/publication unit.

Each individual SoA plane remains contiguous.

---

# 5. Page States

Pages have two runtime states.

## 5.1 Active Page

The active page is:

- writer-owned,
- append-only,
- not modified by readers,
- not required to be mmap-backed,
- partially filled.

Suggested writer-side representation:

```go
type activePage struct {
    count uint32

    imageIDs [RecordsPerPage]imageidx.ImageID
    phash64  [RecordsPerPage]uint64

    pdq0 [RecordsPerPage]uint64
    pdq1 [RecordsPerPage]uint64
    pdq2 [RecordsPerPage]uint64
    pdq3 [RecordsPerPage]uint64
}
```

Whether these are inline arrays or separately allocated aligned buffers should be benchmarked.

The active page must not be mutated after it becomes reader-visible.

## 5.2 Sealed Page

Once full, or explicitly flushed:

1. encode page,
2. append it to persistent storage,
3. satisfy configured durability,
4. map or expose immutable bytes,
5. publish a new immutable index view,
6. create a new active page.

A sealed page is immutable for its lifetime.

No in-place changes are allowed.

---

# 6. Append Path

The dominant write operation is adding a new image.

Suggested public type:

```go
type Entry struct {
    ImageID imageidx.ImageID
    PHash64 uint64
    PDQ     [4]uint64
}
```

Suggested APIs:

```go
func (i *Index) Add(entry Entry) error
func (i *Index) Apply(entries []Entry) error
```

`Apply` should be the primary implementation path.

`Add` is convenience syntax around a single-entry batch.

Append flow:

```text
Entry
  |
  v
validate
  |
  v
WAL batch append
  |
  v
append all six SoA values at same ordinal
  |
  v
publish active immutable snapshot if required
  |
  +-- page not full --> continue
  |
  `-- page full --> seal page --> new active page
```

A batch must preserve a strict one-to-one ordinal relationship across all six columns.

No column may advance independently.

---

# 7. Duplicate ImageID Policy

V1 should define a clear invariant:

```text
At most one currently live perceptual-hash record per ImageID.
```

Normal image import should add each ImageID once.

If an existing image needs new hashes, use a logical replacement:

```text
remove old ImageID from live bitmap
append new hash record
restore/new live state as part of higher-level transaction
```

If duplicate physical records with the same ImageID can temporarily exist, search correctness requires a way to distinguish the newest record.

To keep V1 simple, prefer enforcing:

```text
Add(existing ImageID) -> error
```

unless the caller explicitly uses a future replacement API.

The uniqueness check does not need to be embedded in the page format. It can be enforced by higher-level image metadata or an existing authoritative ImageID registry.

---

# 8. Deletion and Tombstones

`hashidx` does not physically delete records during normal operation.

Deletion is managed through `bitmapIdx`.

Preferred model:

```text
LiveImages bitmap
```

rather than a negative tombstone bitmap when practical.

Then:

```go
if !liveImages.Contains(imageID) {
    continue
}
```

Advantages of a positive live bitmap:

- directly intersects with other query filters,
- naturally expresses active images,
- avoids inverted semantics,
- can be reused by other indexes.

A negative deleted bitmap is also compatible if that better matches existing application semantics.

The important rule is:

> Hash pages themselves remain append-only and immutable.

Physical removal happens only during `Vacuum`.

---

# 9. Search Modes

`hashidx` supports exact Hamming-distance search.

Two execution modes are required.

---

## 9.1 Index-Driven Sequential Scan

Use when:

- no candidate bitmap exists, or
- candidate bitmap cardinality is large enough that sequential scanning is cheaper.

Flow:

```text
scan hash ordinals sequentially
      |
calculate Hamming distance
      |
if distance qualifies
      |
load ImageID
      |
check liveness/candidate bitmap
      |
emit result
```

Whether liveness should be checked before or after Hamming distance must be benchmarked.

For dense live sets, calculate distance first to avoid a bitmap lookup for every image.

For sparse live sets, testing liveness first may win.

The planner may choose dynamically based on bitmap density.

---

## 9.2 Bitmap-Driven Search

Use when an existing bitmap filter is selective.

Example:

```text
geo
AND time
AND metadata
AND live
   |
candidate bitmap
   |
iterate ImageIDs / candidate ordinals
   |
load perceptual hash
   |
exact Hamming distance
```

This path can avoid scanning millions of unrelated hashes.

A requirement follows:

> Efficient bitmap-driven search needs an efficient way to locate the hash ordinal for a given ImageID.

V1 should not automatically add a large reverse index unless needed.

Possible choices:

1. If ImageID allocation is sufficiently dense, use an ImageID-to-ordinal table.
2. Reuse an existing shared database locator if one already exists.
3. Add an optional compact `ImageID -> Ordinal` locator later.
4. If no locator exists, use index-driven scan with bitmap membership instead.

The canonical persistent hash layout must not depend on which locator strategy is selected.

---

# 10. pHash64 Distance

pHash64 uses exact 64-bit Hamming distance.

Scalar reference:

```go
func phashDistance(a, b uint64) uint8 {
    return uint8(bits.OnesCount64(a ^ b))
}
```

Hot sequential loop:

```go
for i := range hashes {
    d := bits.OnesCount64(query ^ hashes[i])

    if d <= maxDistance {
        // liveness and output handling
    }
}
```

The scalar implementation is the correctness reference for all future optimized kernels.

---

# 11. PDQ256 Distance

Represent PDQ as four uint64 words:

```go
type PDQHash struct {
    W0 uint64
    W1 uint64
    W2 uint64
    W3 uint64
}
```

or:

```go
type PDQHash [4]uint64
```

Persistent order must be explicitly defined and never depend on Go struct layout.

Scalar reference:

```go
func pdqDistance(a, b PDQHash) uint16 {
    return uint16(
        bits.OnesCount64(a[0]^b[0]) +
        bits.OnesCount64(a[1]^b[1]) +
        bits.OnesCount64(a[2]^b[2]) +
        bits.OnesCount64(a[3]^b[3]),
    )
}
```

SoA scan:

```go
for i := 0; i < n; i++ {
    d :=
        bits.OnesCount64(q0^pdq0[i]) +
        bits.OnesCount64(q1^pdq1[i]) +
        bits.OnesCount64(q2^pdq2[i]) +
        bits.OnesCount64(q3^pdq3[i])

    if d <= maxDistance {
        // liveness and output handling
    }
}
```

---

# 12. Search APIs

Suggested result type:

```go
type Result struct {
    ImageID  imageidx.ImageID
    Distance uint16
}
```

Radius search:

```go
func (i *Index) SearchPHashRadius(
    dst []Result,
    query uint64,
    maxDistance uint8,
    candidates *bitmapidx.Bitmap,
) []Result
```

```go
func (i *Index) SearchPDQRadius(
    dst []Result,
    query PDQHash,
    maxDistance uint16,
    candidates *bitmapidx.Bitmap,
) []Result
```

Top-K search:

```go
func (i *Index) SearchPHashTopK(
    dst []Result,
    query uint64,
    k int,
    candidates *bitmapidx.Bitmap,
) []Result
```

```go
func (i *Index) SearchPDQTopK(
    dst []Result,
    query PDQHash,
    k int,
    candidates *bitmapidx.Bitmap,
) []Result
```

If importing `bitmapidx` directly would create package layering problems, define the lowest-level APIs around an internal candidate abstraction and let `search` adapt bitmap results into it.

The existing project rule that specialized indexes must not import `search` should remain intact.

---

# 13. Allocation Requirements

Search should be zero-allocation when the caller provides sufficient output/scratch capacity.

Preferred style:

```go
dst = idx.SearchPDQRadius(dst[:0], query, radius, candidates)
```

Top-K may need scratch storage for a bounded heap.

Provide a reusable workspace if needed:

```go
type Workspace struct {
    // private scratch buffers
}
```

and:

```go
func (i *Index) SearchPDQTopKInto(
    dst []Result,
    query PDQHash,
    k int,
    candidates CandidateSet,
    ws *Workspace,
) []Result
```

Do not allocate one object per result.

---

# 14. Runtime View

Readers see an immutable published view.

Suggested private structure:

```go
type indexView struct {
    generation uint64

    pages []pageView

    active *activePageView

    count uint64
}
```

The exact active-page representation may differ, but the reader-visible state must never mutate after publication.

Publication uses an atomic pointer.

Readers:

1. register through `internal/indexutil.ReaderTracker`,
2. load current immutable view,
3. search,
4. release reader registration.

No mutex is allowed on the normal search path.

Writer serialization may use:

```go
writerMu sync.Mutex
```

or the same one-writer mechanism established by the other indexes.

---

# 15. Persistent Layout

Use explicit fixed-width encoding.

Suggested files:

```text
hashidx.<generation>.data
hashidx.wal
manifest
```

A generation contains:

```text
Header
Page Directory
Page 0
Page 1
...
```

Pages are immutable after the generation is published.

---

# 16. Generation Header

Suggested conceptual fields:

```text
Magic
Version
Generation
Flags
RecordsPerPage
PageCount
RecordCount
PageDirectoryOffset
DataOffset
CRC32
```

Use:

- little-endian integer encoding,
- CRC32/IEEE,
- checked offsets,
- 8-byte structural alignment,
- page-aligned major regions where useful.

Do not persist raw Go structs.

---

# 17. Page Directory

A page directory entry should contain only what is required to locate and validate a page.

Suggested conceptual format:

```go
type pageRef struct {
    Offset uint64
    Count  uint32
    Flags  uint32
}
```

Possible additions only if benchmarks or validation require them:

```text
CRC32
encoded length
```

Because page ordinal ranges are implicit:

```text
page 0 -> ordinal 0...
page 1 -> next ordinal range...
```

there is no need to store a first ordinal when pages are densely ordered.

---

# 18. Encoded Page Layout

For a full 8,192-record page:

```text
Page Header

ImageIDs  [8192]uint64
pHash64   [8192]uint64

PDQ0      [8192]uint64
PDQ1      [8192]uint64
PDQ2      [8192]uint64
PDQ3      [8192]uint64
```

For the final partial page, only `Count` records from each plane are valid.

Suggested page header fields:

```text
Magic
Version
Count
Flags
PayloadLength
CRC32
```

The page payload should remain directly mmap-readable without decoding into heap objects.

---

# 19. Plane Alignment

Align each SoA plane to at least 8 bytes.

Consider 64-byte alignment for the start of each hot hash plane:

```text
pHash64
PDQ0
PDQ1
PDQ2
PDQ3
```

This makes later SIMD kernels easier and prevents accidental cross-field misalignment.

Do not overcomplicate V1 with per-cache-line padding.

---

# 20. WAL

The WAL protects appended records that are not yet represented in a durable published generation.

Use logical batches.

Suggested WAL record:

```text
ImageID uint64
pHash64 uint64
PDQ0 uint64
PDQ1 uint64
PDQ2 uint64
PDQ3 uint64
```

Payload size:

```text
48 bytes / entry
```

Suggested batch framing:

```text
Magic
Version
RecordCount
PayloadLength
CRC32
Payload
```

A batch must be atomically replayable.

Recovery:

1. open and validate manifest,
2. mmap generation,
3. replay complete valid WAL batches after checkpoint,
4. stop at an incomplete trailing WAL batch,
5. reject structural corruption,
6. reconstruct active page state,
7. publish recovered immutable view.

---

# 21. Durability

Reuse `imageidx.Durability`.

The same durability policy used by the other persistent indexes should apply.

Suggested publication sequence for a new generation:

1. write new generation file,
2. flush according to durability,
3. fsync when required,
4. write temporary manifest,
5. fsync temporary manifest,
6. rename manifest,
7. directory sync when required,
8. publish new runtime view,
9. retire old mmap only after old readers exit.

---

# 22. Checkpoint

`Checkpoint()` converts current append state into a durable generation.

It does not need to rewrite existing sealed pages if the persistent format supports carrying them forward safely.

However, V1 may choose the simpler implementation:

```text
existing live/sealed generation
+
replayed WAL/active records
        |
        v
new generation
```

Benchmark whether whole-generation rewrite becomes too expensive before adding incremental generation chaining.

The normal append path must not require a checkpoint per inserted image.

---

# 23. Vacuum

`Vacuum()` is the operation that physically removes dead ImageIDs.

Input:

```text
hashidx generation
+
live ImageID bitmap
```

Output:

```text
new dense hashidx generation
```

Flow:

```text
for each physical ordinal:
    imageID := imageIDs[i]

    if !live.Contains(imageID):
        continue

    append imageID
    append pHash64
    append PDQ words
```

Vacuum:

- rewrites only live records,
- creates new dense ordinals,
- packs pages,
- publishes a new generation,
- retires old mmap after prior readers complete.

No ordinal stability is promised across vacuum.

This avoids:

- per-page delete rewriting,
- page merging,
- free lists,
- free-space management,
- in-place compaction.

---

# 24. Bitmap Integration

`bitmapIdx` is responsible for logical liveness/removal.

`hashidx` should not duplicate this state.

At search time:

```text
requested candidate bitmap
AND
live bitmap
```

should ideally be combined before expensive hash work when the resulting set is selective.

For a full scan, the search implementation may choose between:

```text
A. bitmap membership -> Hamming
```

and:

```text
B. Hamming -> bitmap membership
```

based on candidate density.

This should be benchmark-driven.

---

# 25. Candidate Selectivity

Search should eventually use a simple cost heuristic.

Example:

```text
candidateCount / totalCount
```

If the candidate set is very small and an ImageID-to-ordinal locator exists:

```text
iterate candidates
```

If candidate density is high:

```text
scan hashes sequentially
```

Do not hard-code a threshold without benchmarks.

Suggested benchmarks:

```text
0.01%
0.1%
1%
5%
10%
25%
50%
100%
```

candidate density.

---

# 26. SIMD Strategy

V1 must have scalar correctness kernels first.

pHash64 scalar:

```text
XOR + popcount64
```

PDQ scalar:

```text
4 × XOR + 4 × popcount64
```

Then benchmark architecture-specific kernels.

Target architectures:

```text
amd64 AVX2
arm64 NEON
```

Do not add AVX-512 initially.

Important caveat:

Classic AVX2 has no native general-purpose packed 64-bit popcount instruction.

Therefore:

- do not assume AVX2 automatically beats scalar `POPCNT`,
- benchmark carefully,
- consider batched scalar popcount with improved memory scheduling,
- NEON implementations may have different tradeoffs,
- future architecture-specific paths can be added without changing storage layout.

The SoA format deliberately keeps these options open.

---

# 27. Search Early Exit

Radius search can short-circuit PDQ accumulation.

Example:

```go
d := bits.OnesCount64(q0 ^ pdq0[i])
if d > max {
    continue
}

d += bits.OnesCount64(q1 ^ pdq1[i])
if d > max {
    continue
}

d += bits.OnesCount64(q2 ^ pdq2[i])
if d > max {
    continue
}

d += bits.OnesCount64(q3 ^ pdq3[i])
if d > max {
    continue
}
```

Benchmark against always computing all four words.

For small thresholds this may reduce popcount work.

For highly optimized branchless loops it may be slower due to misprediction.

Keep both kernels benchmarkable.

---

# 28. Top-K Implementation

Exact Top-K does not require a secondary index.

For small `k`, use a bounded max-heap of current best results.

Each candidate whose distance is less than the current worst result replaces it.

The heap should reuse caller/workspace memory.

For typical duplicate detection, radius search should be the primary API.

---

# 29. pHash and PDQ Independence

Both hashes share storage ordinals but remain independently searchable.

It must be possible to:

```text
search pHash only
search PDQ only
search both at the application level
```

Do not force every query to calculate both distances.

A higher-level duplicate pipeline can use:

```text
pHash64 candidate threshold
        |
        v
PDQ256 stronger verification
```

or search PDQ directly.

The storage layout supports both.

---

# 30. Validation

On open, validate:

- magic,
- version,
- header length,
- records per page,
- file size,
- directory bounds,
- page offsets,
- page alignment,
- page count,
- record count,
- partial final page constraints,
- payload lengths,
- integer overflow,
- CRC32,
- monotonic non-overlapping page ranges,
- exact matching plane lengths.

For every page:

```text
len(ImageIDs)
==
len(pHash64)
==
len(PDQ0)
==
len(PDQ1)
==
len(PDQ2)
==
len(PDQ3)
==
Count
```

This invariant is critical.

---

# 31. Configuration

Suggested V1 config:

```go
type Config struct {
    RecordsPerPage uint32
}
```

Defaults:

```text
RecordsPerPage = 8192
```

If page size is intended to be a persistent-format invariant, do not make it freely configurable after index creation.

Prefer persisting it in metadata and rejecting incompatible opens.

---

# 32. Metadata

Suggested public metadata:

```go
type Metadata struct {
    Generation     uint64
    RecordCount    uint64
    PageCount      uint64
    RecordsPerPage uint32
}
```

Possible future additions:

```text
LiveCount
VacuumGeneration
```

But avoid copying information already authoritative in `bitmapIdx`.

---

# 33. Public API Sketch

```go
package hashidx

type ImageID = imageidx.ImageID

type PDQHash [4]uint64

type Entry struct {
    ImageID ImageID
    PHash64 uint64
    PDQ     PDQHash
}

type Result struct {
    ImageID  ImageID
    Distance uint16
}

type Config struct {
    RecordsPerPage uint32
}

func New(cfg Config) (*Index, error)

func NewPersistent(
    dir string,
    cfg Config,
    durability imageidx.Durability,
) (*Index, error)

func OpenPersistent(
    dir string,
    cfg Config,
    durability imageidx.Durability,
) (*Index, error)

func (i *Index) Add(entry Entry) error

func (i *Index) Apply(entries []Entry) error

func (i *Index) SearchPHashRadius(
    dst []Result,
    query uint64,
    maxDistance uint8,
    candidates CandidateSet,
) []Result

func (i *Index) SearchPDQRadius(
    dst []Result,
    query PDQHash,
    maxDistance uint16,
    candidates CandidateSet,
) []Result

func (i *Index) SearchPHashTopK(
    dst []Result,
    query uint64,
    k int,
    candidates CandidateSet,
    ws *Workspace,
) []Result

func (i *Index) SearchPDQTopK(
    dst []Result,
    query PDQHash,
    k int,
    candidates CandidateSet,
    ws *Workspace,
) []Result

func (i *Index) Checkpoint() error

func (i *Index) Vacuum(live CandidateSet) error

func (i *Index) Metadata() Metadata

func (i *Index) Close() error
```

`CandidateSet` is intentionally abstract in this proposal so package layering can be resolved without forcing `hashidx` to import `search`.

If importing `bitmapidx` is architecturally acceptable, it may be replaced with the concrete bitmap type.

---

# 34. Reader/Writer Concurrency

## Readers

Readers:

- register with `ReaderTracker`,
- atomically load current `indexView`,
- never take the writer mutex,
- only access immutable state,
- release their reader token after search.

## Writer

One serialized writer:

```text
Add
Apply
Checkpoint
Vacuum
Close
```

Mutating operations are serialized.

This preserves the project model of many lock-free readers plus one commit publisher.

---

# 35. Search Planner Integration

`hashidx` should not import `search`.

The higher-level planner may choose:

```text
metadata/time/geo filters
        |
        v
candidate bitmap
        |
        v
HashIdx exact Hamming search
```

This is particularly useful because perceptual similarity is commonly applied after cheaper metadata filters.

Planner responsibilities:

- build candidate bitmap,
- determine whether candidate-driven or index-driven search is preferable,
- call hashidx,
- combine/rank results.

`hashidx` responsibilities:

- store hashes,
- execute exact Hamming operations efficiently,
- respect candidate/liveness constraints supplied to it.

---

# 36. Benchmarks

Benchmark on both amd64 and arm64.

Dataset sizes:

```text
100K
1M
10M
50M where hardware permits
```

## Insert

Measure:

- single-entry append,
- 100-entry batch,
- 1K-entry batch,
- 10K-entry batch,
- page seal cost,
- WAL cost,
- checkpoint throughput.

## pHash Search

Measure:

- full exact scan,
- radius 0,
- radius 2,
- radius 5,
- radius 10,
- Top-10,
- Top-100.

## PDQ Search

Measure:

- full exact scan,
- small duplicate threshold,
- broader thresholds,
- Top-10,
- Top-100.

Use real PDQ distributions where available rather than only random hashes.

## Candidate Density

Measure both execution paths at:

```text
0.01%
0.1%
1%
5%
10%
25%
50%
100%
```

## Tombstones

Measure:

```text
0%
1%
5%
10%
25%
50%
```

dead records.

## Vacuum

Measure:

- scan throughput,
- rewritten bytes,
- wall-clock throughput,
- peak memory,
- mmap generation retirement behavior.

## CPU Counters Where Available

Record:

- cycles/hash,
- instructions/hash,
- branch misses,
- L1 misses,
- LLC misses,
- memory bandwidth.

---

# 37. Future Secondary Indexes

The canonical SoA storage must remain independent of future search acceleration.

Possible secondary indexes:

```text
MIH
binary IVF
VP-tree
binary HNSW
LSH
```

All must store ordinals rather than duplicate full hashes where practical.

Example:

```text
secondary posting -> Ordinal
Ordinal -> canonical PDQ
Ordinal -> ImageID
```

A candidate accelerator returns ordinals.

The canonical hash is then used for exact Hamming verification.

Secondary indexes may be dropped and rebuilt without losing perceptual-hash data.

---

# 38. Recommended V1

```text
Package:                 hashidx

Stored fields:
    ImageID               uint64
    pHash64               uint64
    PDQ256                4 × uint64

Layout:
    Structure of Arrays

Shared physical identity:
    ordinal

Records per logical page:
    8192

Per-plane full-page size:
    64 KiB

Full logical page:
    384 KiB

Write model:
    append only

Active page:
    writer-owned

Sealed pages:
    immutable

Persistent access:
    mmap

Reader model:
    atomic immutable view
    ReaderTracker
    no read mutex

Writer model:
    one serialized writer

Deletion:
    bitmapIdx liveness/tombstone state

Physical deletion:
    Vacuum only

Search:
    exact Hamming

pHash kernel:
    uint64 XOR + popcount

PDQ kernel:
    4 × uint64 XOR + popcount

Filtered search:
    bitmap-aware

Persistence:
    generation + WAL + manifest

Checksums:
    CRC32/IEEE

Integer encoding:
    little-endian

Structural alignment:
    8 bytes minimum

Hot plane alignment:
    benchmark 64-byte alignment

Search allocation:
    zero with caller-owned output/workspace

SIMD:
    scalar reference first
    AVX2/NEON only after benchmarks

ANN/metric index:
    none in V1

Compaction:
    explicit Vacuum
```

---

# 39. Implementation Phases

## Phase 1 — In-Memory Scalar Index

Implement:

- `Entry`,
- `PDQHash`,
- SoA active pages,
- append path,
- scalar pHash Hamming,
- scalar PDQ Hamming,
- radius search,
- Top-K search,
- candidate filtering,
- tests.

Required invariants:

- all planes always have identical logical length,
- ordinal mapping is deterministic,
- no allocations in steady-state search with supplied buffers.

## Phase 2 — Immutable Published Pages

Implement:

- fixed 8,192-record pages,
- active page,
- sealed page views,
- atomic immutable publication,
- `ReaderTracker`,
- single serialized writer.

Test with race detector.

## Phase 3 — Persistence

Implement:

- generation header,
- page directory,
- mmap page views,
- explicit encoding,
- CRC32,
- manifest,
- generation publication,
- validation.

## Phase 4 — WAL and Recovery

Implement:

- logical append batch WAL,
- durability integration,
- replay,
- incomplete trailing batch handling,
- recovery tests,
- crash/fault tests where practical.

## Phase 5 — bitmapIdx Integration

Implement:

- live/candidate filtering,
- index-driven filtering,
- candidate-driven search where locator support exists,
- selectivity benchmarks.

## Phase 6 — Vacuum

Implement:

- live-record rewrite,
- dense new ordinals,
- generation publication,
- old mmap retirement,
- tombstone-density benchmarks.

## Phase 7 — Performance

Benchmark:

- RecordsPerPage,
- alignment,
- early-exit PDQ kernel,
- branchless PDQ kernel,
- scalar pHash,
- candidate-density thresholds,
- batching,
- prefetching,
- AVX2/NEON prototypes.

Only retain optimizations with measured improvements.

---

# 40. Design Principle

The central V1 decision is:

```text
Perceptual hashes are canonical append-only columnar data.

Deletion is external bitmap state.

Search is exact Hamming over immutable pages.

Acceleration is optional and rebuildable.
```

This keeps the persistent format small, predictable, mmap-friendly, and compatible with the rest of `imageIdx` while leaving room for future binary-search accelerators without committing the database format to one search algorithm.
