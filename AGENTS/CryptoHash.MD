# CryptoHash Index Proposal

> **Historical proposal.** This document is retained for design context only.
> It does not describe the current implementation. Use `index/radixhash` for
> exact hashes and `database/FORMAT.md` for V1 persistence.

## 1. Purpose

`cryptohash` is a persistent, mmap-oriented exact-content hash index for `imageIdx`.

Its only job is to map a cryptographic content hash to one or more database-wide `imageidx.ImageID` values:

```text
[Hash][ImageID]
```

The index supports two hash algorithms:

```text
SHA-256
BLAKE3-256
```

Both produce a fixed 256-bit digest, so they share exactly the same persistent record layout and search implementation.

The design deliberately favors simplicity, predictable mmap access, fixed-width records, no duplicate lookup structures, no per-record heap allocation, immutable reader-visible generations, one serialized writer, WAL-backed recovery, explicit checkpoint/vacuum, and benchmark-driven optimization.

`cryptohash` is for **exact byte/content identity**, not perceptual similarity. Use pHash, PDQ, vector search, or another perceptual index for near-duplicate image matching. A cryptographic hash match means the input bytes used for hashing are identical.

## 2. Goals

1. Exact lookup from a 256-bit hash to `ImageID`.
2. Support either SHA-256 or BLAKE3-256 per index.
3. Keep one physical record format for both algorithms.
4. Persist records as fixed-width sorted `[Hash32][ImageID8]` records.
5. Allow multiple `ImageID` values for one hash.
6. mmap the immutable base directly.
7. Keep reads lock-free using the same immutable-publication model as the other indexes.
8. Use a small bounded mutable delta rather than rewriting the base on every insert.
9. Use WAL + checkpoint + manifest for durability.
10. Keep steady-state exact lookup allocation-free when caller storage is sufficient.
11. Reuse project `imageidx.ImageID`, `imageidx.Durability`, reader tracking, mmap helpers, and publication conventions.
12. Do not add SIMD, custom hash tables, bloom filters, prefix indexes, or other complexity until benchmarks justify them.

## 3. Non-Goals

V1 does not provide perceptual hashing, Hamming-distance search, pHash or PDQ search, partial-file hashing, chunk-level deduplication, content-defined chunking, cryptographic verification beyond normal digest equality, mixed SHA-256 and BLAKE3 records inside one index, variable-length hashes, hash truncation in persistent records, distributed lookup, lock-free multi-writer mutation, or a general-purpose key/value database.

The package answers one question efficiently:

```text
Which ImageID values have this exact 256-bit digest?
```

## 4. Package

Suggested package:

```text
cryptohash/
```

It should remain independent of `bitmapidx`, `timeidx`, `vectoridx`, and `search`.

Allowed dependencies should follow the project direction:

```text
cryptohash
    -> imageidx
    -> internal/indexutil
    -> standard library
```

`cryptohash` must not import `search`.

## 5. Core Types

### 5.1 Hash

Both supported algorithms produce 256 bits.

```go
type Hash [32]byte
```

The persistent representation is exactly the 32 digest bytes. Do not store hashes as strings or hex. Do not byte-swap or reinterpret digest words for persistence.

The serialized sort order is lexicographic byte order over the 32 digest bytes. That ordering is stable across architectures and matches `bytes.Compare`.

### 5.2 ImageID

Use the shared database type:

```go
type ImageID = imageidx.ImageID
```

`ImageID` remains 64 bits in memory, WAL records, and persistent records. Do not narrow it to `uint32`.

### 5.3 Algorithm

```go
type Algorithm uint8

const (
    AlgorithmSHA256 Algorithm = iota + 1
    AlgorithmBLAKE3
)
```

One `Index` contains exactly one algorithm. The algorithm is persisted in the manifest and generation header. Opening an index does not require callers to independently specify the algorithm; persisted metadata is authoritative.

### 5.4 Entry

```go
type Entry struct {
    Hash    Hash
    ImageID imageidx.ImageID
}
```

The logical ordering is:

```text
Hash ascending
then ImageID ascending
```

The same `(Hash, ImageID)` pair MUST NOT appear more than once in the effective index.

## 6. Why SHA-256 and BLAKE3 Share One Format

The persistent index does not care how a digest was produced. Both algorithms produce a 32-byte digest, therefore SHA-256 and BLAKE3 use the same `[Hash32][ImageID8]` record layout and share the same search, sorting, mmap, WAL, checkpoint, and compaction code.

The algorithm only affects hash computation before lookup/insertion. This is intentionally simpler than maintaining algorithm-specific storage engines.

## 7. Hash Computation

Keep hashing separate from index lookup.

Suggested helpers:

```go
func SumSHA256(data []byte) Hash
func SumBLAKE3(data []byte) Hash
```

and streaming variants:

```go
func HashReaderSHA256(r io.Reader) (Hash, error)
func HashReaderBLAKE3(r io.Reader) (Hash, error)
```

The index API should primarily accept an already-computed `Hash`.

Example:

```go
sum, err := cryptohash.HashReaderBLAKE3(file)
if err != nil {
    return err
}

ids := index.Lookup(dst[:0], sum)
```

This separation matters because image ingest may already be reading the file once, callers may want to hash while decoding/copying, index lookup should never perform file I/O, and SHA-256 versus BLAKE3 implementation details stay outside the mmap search path.

## 8. Recommended Public API

Keep the public surface small.

```go
type Spec struct {
    IndexID   uint64
    Flags     uint32
    Algorithm Algorithm
    Entries   []Entry
}

type Config struct {
    ActiveTarget   int
    HardDeltaLimit int
}

type Index struct {
    // private
}
```

Constructors should follow project conventions:

```go
func New(spec Spec, cfg Config) (*Index, error)

func NewPersistent(
    dir string,
    spec Spec,
    cfg Config,
    durability imageidx.Durability,
) (*Index, error)

func OpenPersistent(
    dir string,
    cfg Config,
    durability imageidx.Durability,
) (*Index, error)
```

Metadata:

```go
type Metadata struct {
    IndexID    uint64
    Flags      uint32
    Algorithm  Algorithm
    Generation uint64
    BaseCount  uint64
}

func (i *Index) Metadata() Metadata
```

Lookup:

```go
func (i *Index) Contains(hash Hash) bool
func (i *Index) Lookup(dst []imageidx.ImageID, hash Hash) []imageidx.ImageID
func (i *Index) Count(hash Hash) uint64
```

Mutation:

```go
type Mutation struct {
    Add    []Entry
    Remove []Entry
}

func (i *Index) Apply(m Mutation) error
```

Persistence:

```go
func (i *Index) Checkpoint() error
func (i *Index) Vacuum() error
func (i *Index) Close() error
```

For one-record ingest convenience:

```go
func (i *Index) Add(hash Hash, imageID imageidx.ImageID) error
func (i *Index) Remove(hash Hash, imageID imageidx.ImageID) error
```

These should internally route through the same mutation path rather than creating a second write implementation.

## 9. Lookup Semantics

`Lookup` returns all `ImageID` values associated with the exact digest. Results MUST be ascending and unique.

Example:

```text
Hash A -> ImageID 12
Hash A -> ImageID 41
Hash A -> ImageID 900
```

`Lookup(hashA)` returns:

```text
[12, 41, 900]
```

This matters because exact duplicate files may legitimately exist as multiple database records. The index MUST NOT assume one hash maps to one image.

## 10. Exact Deduplication Semantics

A matching cryptographic digest is the database's exact-content candidate.

Typical ingest flow:

```text
read file
   |
   +-> compute BLAKE3 or SHA-256
   |
   +-> Lookup(hash)
           |
           +-> no IDs: new exact content
           |
           +-> IDs: exact-content duplicate candidates
```

For ordinary photo-library deduplication, comparing the full 256-bit digest is sufficient. If the application requires an absolute byte-for-byte verification step despite the cryptographic digest match, that verification belongs in the canonical file layer, not inside `cryptohash`.

Do not use the cryptographic index for near-duplicate decisions.

## 11. Record Format

The fundamental record remains fixed-width:

```text
Offset +0   Hash[32]
Offset +32  ImageID uint64
-------------------------
Record size: 40 bytes
```

Conceptually:

```go
type Entry struct {
    Hash    [32]byte
    ImageID imageidx.ImageID
}
```

Do not persist the Go struct directly. Encode fields explicitly.

The logical ordering is always:

```text
Hash ascending
then ImageID ascending
```

The 40-byte record format is identical for SHA-256 and BLAKE3-256.

---

## 12. Primary Storage Model: Immutable COW Blocks

Because cryptographic hashes will change continuously as new images are added, V1 should not use one monolithic sorted Base that must periodically be rewritten in full.

Instead, store records in independently replaceable immutable sorted blocks:

```text
Block Directory
    |
    +--> Block 0: sorted [Hash][ImageID] records
    +--> Block 1: sorted [Hash][ImageID] records
    +--> Block 2: sorted [Hash][ImageID] records
    ...
```

Each block is immutable after publication.

Updates use copy-on-write:

```text
old block
   |
copy + merge mutation batch
   |
new replacement block(s)
   |
append to generation storage
   |
publish replacement directory
   |
retire old block after prior readers finish
```

Never modify reader-visible mmap bytes in place.

This keeps the project's existing immutable-reader model while bounding write amplification during continuous ingest.

---

## 13. Block Size

Start by benchmarking:

```text
16 KiB
32 KiB
64 KiB
```

With 40-byte records, approximate maximum raw capacities are:

```text
16 KiB -> ~409 records
32 KiB -> ~819 records
64 KiB -> ~1,638 records
```

Recommended initial baseline:

```text
16 KiB or 32 KiB
```

Do not make the chosen block size a semantic property of the hash algorithm.

Block size is a format/runtime design choice that should be selected by benchmarks measuring:

- lookup latency;
- write amplification;
- mmap locality;
- batch rewrite cost;
- split frequency;
- total storage amplification.

A block should contain a header plus a sorted record payload. The block may be padded to its physical allocation size where useful.

---

## 14. Block Directory

Maintain a small sorted directory describing the current live block set.

A conceptual descriptor is:

```go
type BlockRef struct {
    FirstHash [32]byte
    Offset    uint64
    Count     uint16
    Flags     uint16
    _         uint32
}
```

The exact layout may evolve, but descriptors should remain fixed-width and eight-byte aligned.

The directory is sorted by `FirstHash`.

A block's upper hash boundary is normally implied by the next block's `FirstHash`.

The directory should remain small enough to stay hot in memory. For example, at 10M records and roughly 500 records per block:

```text
~20,000 blocks
```

Even a 48- or 64-byte descriptor keeps the directory around 1 MiB.

Do not add a tree above this directory initially. Binary search is sufficient until measurements show otherwise.

---

## 15. Lookup Algorithm

Lookup is two-stage:

```text
Hash
  |
  +--> binary-search Block Directory
          |
          +--> binary-search selected immutable block
                  |
                  +--> scan adjacent equal-hash records
```

Within a block, records are sorted by:

```text
Hash
then ImageID
```

The normal lookup algorithm is:

1. binary-search the directory for the greatest `FirstHash <= query`;
2. open the mapped block referenced by that descriptor;
3. lower-bound binary-search its records for `query`;
4. if equal, emit adjacent ImageIDs until the hash changes;
5. if the equal-hash run continues into the following block, continue there;
6. merge any current writer-side published correction state if one exists.

This preserves exact lookup while reducing random probes into the large record region.

---

## 16. Duplicate Hash Runs

Multiple ImageIDs may share one hash:

```text
Hash A -> 12
Hash A -> 41
Hash A -> 900
```

Prefer not to split an equal-hash run across blocks when performing a normal split.

When splitting an overflowing block, choose a split boundary between distinct hashes when practical.

However, a single hash may theoretically have more ImageIDs than fit in one block. Therefore the format MUST support the same hash spanning multiple adjacent blocks.

Lookup must continue to the next block while its first records contain the same hash.

Do not impose an arbitrary duplicate-count limit.

---

## 17. Block Occupancy and Splits

Blocks should not be packed to 100% target occupancy after every split.

Recommended starting target after a split:

```text
75% to 85% occupancy
```

This reduces immediate re-splitting under sustained inserts.

When a block overflows:

```text
old block
   |
merge all pending entries for that block
   |
choose split boundary
   |
new left block
new right block
   |
append both
   |
publish directory containing both replacements
```

The old block becomes unreachable from the newly published directory but remains available until prior readers finish.

Do not aggressively merge underfull blocks after deletes. Vacuum handles repacking.

---

## 18. Batched Mutation Model

Continuous image ingest makes batching important.

Do not rewrite a block once for every image.

The canonical mutation path should accept a batch:

```go
type Mutation struct {
    Add    []Entry
    Remove []Entry
}

func (i *Index) Apply(m Mutation) error
```

Conceptual flow:

```text
incoming mutation batch
        |
validate + canonicalize
        |
append one WAL batch
        |
sort/group by affected hash range
        |
locate affected blocks
        |
rewrite each affected block once
        |
split overflowing blocks
        |
append replacement blocks
        |
build replacement directory
        |
atomically publish one new view/root
```

A batch of 1,000 images should publish once, not 1,000 times.

If many entries in one batch target the same block, rewrite that block once.

This is a central performance requirement.

---

## 19. Small Writer Buffer

A small writer-side buffer may coalesce tiny inserts before block rewrite, but it must remain bounded.

Recommended baseline:

```text
small Active batch buffer
-> flush by count, explicit Flush, checkpoint, or bounded timer at a higher layer
```

Do not introduce a long chain of immutable runs.

The reader-visible structure should remain conceptually:

```text
current immutable block directory
+ at most one small published correction layer if needed
```

Prefer direct block replacement for accepted write batches when latency/durability policy allows it.

If a published correction layer is retained for throughput, keep it bounded and fold it into affected blocks frequently.

The index MUST NOT become an unbounded LSM tree.

---

## 20. Remove-Wins Semantics

If the same `(Hash, ImageID)` pair appears in both `Add` and `Remove` in one mutation:

```text
Remove wins
```

Canonicalization should also remove duplicate add/remove operations inside the same batch.

The effective index must contain at most one copy of each `(Hash, ImageID)` pair.

---

## 21. Reader Model

Follow the existing project publication contract.

Conceptually:

```go
type indexView struct {
    metadata  Metadata
    directory []BlockRef
    mapping   mappedGeneration
    correction *delta // optional and bounded
}
```

The index publishes one immutable view using an atomic pointer.

Readers:

1. register with the private `ReaderTracker`;
2. load one view;
3. binary-search its immutable directory;
4. search immutable mapped blocks;
5. apply the bounded correction layer if present;
6. release registration.

Readers take no mutex.

Nothing reachable from a published view is mutated.

Old mappings/directories are reclaimed only after `WaitForPriorReaders()` proves they are no longer referenced.

Reuse `internal/indexutil.ReaderTracker` rather than introducing another reclamation mechanism.

---

## 22. No In-Place mmap Mutation

Do not update mapped blocks in place.

Even if a block has physical slack, an insert must not overwrite reader-visible bytes.

Use:

```text
copy old block
apply modifications
write replacement block
publish new directory/root
```

Physical slack is useful because a replacement may fit in the same nominal block class and avoid a split, but it is not permission to mutate the old block.

This invariant keeps reader synchronization simple and consistent with the rest of `imageIdx`.

---

## 23. Persistence Layout

Use one generation file per published storage generation.

Conceptually:

```text
+--------------------------------------------------+
| Generation Header                                |
+--------------------------------------------------+
| Block Directory                                  |
+--------------------------------------------------+
| immutable block                                  |
+--------------------------------------------------+
| immutable block                                  |
+--------------------------------------------------+
| replacement / split blocks appended later        |
+--------------------------------------------------+
| ...                                              |
+--------------------------------------------------+
```

The directory for a new publication may itself be appended as a new directory image or written as part of a new generation/root structure, depending on the final atomic-publication mechanism.

The important requirements are:

- old blocks remain immutable;
- replacement blocks are written before publication;
- the newly published root/directory references only complete validated blocks;
- readers see either the old valid view or the new valid view.

---

## 24. Append-Only Replacement Within a Generation

V1 should avoid a free-page allocator.

When a block is replaced:

```text
old block remains physically present
new block is appended
new directory points to new block
```

The old block becomes dead storage.

Do not immediately reuse its bytes.

This deliberately trades some temporary storage amplification for much simpler:

- crash recovery;
- reader lifetime management;
- block publication;
- WAL semantics;
- corruption analysis.

`Vacuum` reclaims dead blocks later.

---

## 25. Generation Header

Use a fixed-size header, for example 64 or 128 bytes.

Persist at least:

```text
Magic
Version
Generation
IndexID
Algorithm
Flags
LiveEntryCount
BlockCount
DirectoryOffset
DirectoryLength
CheckpointWALOffset
CRC32
```

All structural offsets and lengths are explicit.

All multi-byte integers are little-endian.

Use CRC32/IEEE to match existing project persistence conventions.

The exact byte layout must be explicitly encoded and versioned rather than inferred from Go struct padding.

---

## 26. Block Format

Each block should have a small fixed header followed by fixed-width records.

Conceptually:

```text
Block Header
    RecordCount
    PayloadLength
    Flags/reserved
    CRC32

Record[0] = Hash32 + ImageID8
Record[1] = Hash32 + ImageID8
...
```

A block should validate independently enough that a corrupted directory cannot cause arbitrary mmap reads.

Useful validation includes:

- block offset and length within mapping;
- alignment;
- record count fits payload;
- CRC where required;
- sorted `(Hash, ImageID)` order;
- no duplicate pair inside the block.

A replacement block is immutable after publication.

---

## 27. WAL

Use a write-ahead log for accepted mutations.

Suggested filename:

```text
cryptohash.wal
```

Logical records:

```text
Add(Hash, ImageID)
Remove(Hash, ImageID)
```

Batch framing should contain:

```text
Magic
Version
RecordCount
PayloadLength
Sequence / LSN
CRC32
```

An `Apply` batch must be recoverable atomically.

### Mutation ordering

For a durable write:

```text
1. encode WAL batch
2. append WAL
3. sync according to imageidx.Durability
4. construct replacement blocks + directory/root
5. publish immutable view
6. acknowledge according to durability contract
```

The exact acknowledgement point must match the repository's durability semantics.

---

## 28. Recovery

On open:

1. validate manifest/root metadata;
2. mmap the published generation;
3. validate the directory and structural block bounds;
4. locate `CheckpointWALOffset`;
5. replay complete valid WAL batches after that offset;
6. stop safely at an incomplete trailing WAL batch;
7. reject non-trailing structural corruption;
8. reproduce the same block-replacement state deterministically;
9. publish recovered immutable state;
10. resume writes.

Recovery must never expose a partially written replacement block.

Because blocks are immutable and old storage is not reused immediately, recovery is simpler than an in-place page-update design.

---

## 29. Atomic Publication

A completed write batch should publish one new immutable root/view.

The root identifies:

```text
Generation
DirectoryOffset
DirectoryLength
BlockCount
LiveEntryCount
CheckpointWALOffset / applied LSN
```

Use the project's existing atomic publication helpers where possible.

Readers must observe either:

```text
old directory + old blocks
```

or:

```text
new directory + replacement blocks
```

and never an incomplete mixture.

---

## 30. Checkpoint

Checkpoint establishes a durable clean boundary.

It should:

1. ensure accepted mutations are represented in the current block directory;
2. persist/validate all referenced replacement blocks;
3. persist the current directory/root;
4. record the covered WAL offset/LSN;
5. atomically publish the checkpoint manifest;
6. permit older WAL records to be trimmed during vacuum/rotation.

Checkpoint does not need to rewrite all live records into a single flat Base.

That is an important change from the original proposal.

---

## 31. Vacuum

`Vacuum` performs full physical reclamation.

Algorithm:

```text
current directory
      |
enumerate live blocks in hash order
      |
rewrite only live records/blocks contiguously
      |
optionally rebalance block occupancy
      |
write compact directory
      |
write new clean generation
      |
atomically publish
      |
wait for prior readers
      |
remove old generation
```

Vacuum reclaims:

- dead replaced blocks;
- fragmented space;
- obsolete generation data;
- old WAL data covered by checkpoint.

V1 should keep vacuum explicit.

Do not add online free-space reuse before benchmarks show that vacuum storage amplification is unacceptable.

---

## 32. Deletes

Deletes remove a specific pair:

```text
(Hash, ImageID)
```

A delete batch rewrites only the affected block(s).

Do not immediately merge underfull neighboring blocks.

That would increase write amplification and implementation complexity for little benefit.

Allow sparse blocks to remain until:

- a later write naturally rewrites them;
- a split/rebalance operation touches the range;
- `Vacuum` repacks the generation.

---

## 33. Hash Algorithm Configuration

Algorithm is a persistent index property.

Creation:

```go
idx, err := cryptohash.NewPersistent(
    dir,
    cryptohash.Spec{
        IndexID:   8,
        Algorithm: cryptohash.AlgorithmBLAKE3,
    },
    cryptohash.Config{},
    durability,
)
```

SHA-256 uses:

```go
Algorithm: cryptohash.AlgorithmSHA256
```

One index MUST NOT mix algorithms.

Changing algorithms requires rebuilding the index.

Do not store the algorithm per entry.

---

## 34. Why Not Store Algorithm Per Record

Do not use:

```text
[Algorithm][Hash][ImageID]
```

for every entry.

The algorithm applies to the whole index.

Persisting it once in metadata:

- saves space;
- preserves the 40-byte record;
- simplifies ordering;
- avoids cross-algorithm comparisons;
- keeps block code shared by SHA-256 and BLAKE3.

---

## 35. Why Not Truncate the Hash

Store all 256 bits.

Do not persist only a 64- or 128-bit prefix.

The index is for exact-content identity; truncation would require collision verification and another authoritative representation.

At 10M records, raw record bytes are:

```text
40 * 10,000,000
= 400,000,000 bytes
≈ 381 MiB
```

Block headers and temporary COW dead space add overhead, but the live logical dataset remains compact.

---

## 36. Memory and Storage Amplification

The live logical record set is approximately:

```text
40 bytes * entry count
```

Additional memory/storage consists of:

```text
small block directory
block headers/padding
current writer batch
WAL
obsolete COW blocks awaiting Vacuum
```

Track two different metrics:

```text
LiveBytes
PhysicalBytes
```

and expose/benchmark storage amplification:

```text
PhysicalBytes / LiveBytes
```

This ratio can later drive an operational recommendation for when to call `Vacuum`.

Do not add automatic vacuum in V1.

---

## 37. Hash Computation Pipeline

Hashing should remain separate from index storage.

During ingest, compute the digest while the file is already being read when possible:

```text
file read
   |
   +--> SHA-256 or BLAKE3 hasher
   |
   +--> decoder / metadata extraction / copy
```

Avoid an extra complete file read solely for CryptoHash when the ingest pipeline can fan the bytes into the hasher.

Use the standard library for SHA-256.

Keep the selected Go BLAKE3 implementation isolated behind helper functions/interfaces so the persistent index has no dependency on BLAKE3 internals.

---

## 38. Search Planner Integration

Do not add CryptoHash to the general conjunctive `search` planner initially.

Exact hash lookup is a direct key lookup:

```go
ids := hashIdx.Lookup(dst[:0], hash)
```

The caller decides how to use the exact duplicates.

This keeps `cryptohash` independent of `search`, matching the repository dependency direction.

---

## 39. Comparison Implementation

Start with a portable scalar comparator for the 32 digest bytes.

For mapped storage, compare directly against record bytes without constructing heap objects.

Ordering must be stable and explicitly defined as lexicographic byte order.

Do not reinterpret the digest as little-endian `uint64` numeric words for ordering because that produces a different sort order.

Word loads may be used for equality after alignment/bounds validation.

Do not add SIMD unless profiling proves 32-byte comparisons are a bottleneck.

---

## 40. Bulk Build

Initial creation of a large index should not replay all records through ordinary incremental block updates.

Bulk build:

1. accept entries;
2. sort by `(Hash, ImageID)`;
3. canonicalize duplicate pairs;
4. partition into target-occupancy blocks;
5. avoid splitting equal-hash runs when practical;
6. write blocks sequentially;
7. write directory;
8. publish initial manifest/root;
9. begin with an empty WAL.

This provides a dense initial generation while normal operation uses COW block replacement.

---

## 41. Config

Use the project `withDefaults()` pattern.

Conceptually:

```go
type Config struct {
    BlockSize      uint32
    TargetFillPct  uint8
    MaxBatchEntries uint32
}
```

Recommended benchmark baselines:

```text
BlockSize       = 16 KiB or 32 KiB
TargetFillPct   = 80
MaxBatchEntries = workload-driven
```

These are runtime/build policies where possible, not semantic properties of SHA-256/BLAKE3.

If block size affects persistent decoding, persist the actual format value in the generation header rather than assuming a compile-time constant.

---

## 42. Errors

Use package-specific sentinel errors, for example:

```go
var (
    ErrInvalidAlgorithm = errors.New("cryptohash: invalid algorithm")
    ErrDuplicateEntry   = errors.New("cryptohash: duplicate entry")
    ErrCorrupt          = errors.New("cryptohash: corrupt index")
    ErrUnsupported      = errors.New("cryptohash: unsupported format")
    ErrClosed           = errors.New("cryptohash: index closed")
)
```

Follow repository naming conventions in the implementation.

---

## 43. Concurrency

### Readers

Readers:

- register through the private reader tracker;
- load one immutable view;
- binary-search the directory;
- search immutable blocks;
- allocate nothing when caller storage is sufficient;
- take no mutex.

### Writer

One serialized writer owns:

- WAL ordering;
- mutation batching;
- block replacement;
- split decisions;
- directory construction;
- checkpoint;
- vacuum publication.

Do not introduce per-block reader/writer locks.

Copy-on-write replaces locking.

---

## 44. Invariants

The implementation must maintain:

1. One index contains one cryptographic algorithm.
2. SHA-256 and BLAKE3 hashes are exactly 32 bytes.
3. Every logical record is exactly 40 bytes.
4. Records inside every block are sorted by `(Hash, ImageID)`.
5. The directory is sorted by block `FirstHash`.
6. Effective `(Hash, ImageID)` pairs are unique.
7. Multiple ImageIDs may share one Hash.
8. A duplicate-hash run may span adjacent blocks.
9. ImageID remains `uint64`.
10. Published blocks and directories are immutable.
11. Readers take no mutex.
12. Reader lifetime tracking protects retired mmap storage.
13. Blocks are replaced by COW, never edited in place.
14. One mutation batch publishes once.
15. Remove wins over add for the same pair within one mutation.
16. Old physical blocks are not immediately reused.
17. WAL recovery is deterministic.
18. Vacuum never changes logical `(Hash, ImageID)` contents.
19. Algorithm choice does not change within an index.
20. The full 256-bit digest is persisted.
21. CryptoHash represents exact content identity, not perceptual similarity.
22. No process-local Go pointers are persisted.

---

## 45. Tests

### Basic lookup

Test:

- empty index;
- one block;
- multiple blocks;
- first/middle/last hash;
- missing hashes;
- one hash with multiple ImageIDs;
- duplicate-hash run spanning blocks;
- ascending unique returned IDs.

### Mutation

Test:

- insert into block start/middle/end;
- multiple inserts into one block in one batch;
- one batch touching many blocks;
- block overflow/split;
- split around duplicate hashes;
- delete;
- add + remove same pair;
- remove wins;
- no duplicate pair after repeated add.

### Publication

Verify readers can hold an old directory while a writer publishes replacement blocks.

Verify old mappings are not reclaimed before prior readers finish.

### Persistence/recovery

Test:

- create/open;
- checkpoint;
- WAL replay;
- incomplete trailing WAL batch;
- replacement block written but root not published;
- root publication;
- CRC failures;
- invalid offsets/counts;
- unsorted block records;
- invalid directory ordering;
- vacuum and reopen.

### Algorithms

Run equivalent index behavior tests for SHA-256 and BLAKE3.

Hash helpers should use known-answer vectors and confirm one-shot/streaming equality.

### Allocation

With sufficient destination capacity, verify steady-state:

```text
Lookup
Contains
Count
```

perform zero heap allocations.

---

## 46. Benchmarks

Benchmark at:

```text
100K
1M
10M
```

records where practical.

### Lookup

Measure:

- hit with one ID;
- hit with 2/4/16 duplicates;
- duplicate run crossing blocks;
- random miss;
- hot repeated hit;
- random hits across large mmap.

Report:

```text
ns/op
B/op
allocs/op
```

### Block size

Compare:

```text
16 KiB
32 KiB
64 KiB
```

Measure both read and write behavior.

### Writes

Measure:

- one insert;
- batches of 10/100/1,000/10,000;
- all entries targeting one block;
- entries uniformly spread across blocks;
- split-heavy workload;
- delete-heavy workload.

Report:

```text
entries/sec
bytes written / logical entry
blocks rewritten / batch
splits / batch
B/op
allocs/op
```

### Storage amplification

After sustained random inserts, measure:

```text
PhysicalBytes / LiveBytes
```

before and after `Vacuum`.

### Hash throughput

Separately benchmark SHA-256 versus BLAKE3 on representative image sizes:

```text
64 KiB
256 KiB
1 MiB
4 MiB
16 MiB
```

on representative amd64 and arm64 hardware.

---

## 47. Performance Principles

V1 should deliberately avoid turning CryptoHash into a general storage engine.

Do not initially add:

- a B+tree;
- a general page allocator;
- free lists;
- in-place page updates;
- unbounded LSM runs;
- bloom filters;
- persistent hash tables;
- cuckoo hashing;
- radix trees;
- prefix-compressed digests;
- hash truncation;
- SIMD digest search;
- per-record algorithm tags.

The preferred structure is only:

```text
small sorted directory
       |
immutable sorted blocks
       |
append replacement blocks
       |
Vacuum for reclamation
```

This captures most of the benefit of page-oriented indexing without the complexity of a full B+tree/page manager.

---

## 48. Why Blocks Instead of a B+Tree

At the expected scale, a small directory can address all blocks directly.

For example:

```text
10M records / ~500 records per block
≈ 20K block descriptors
```

A binary search across ~20K directory entries is cheap and the directory should remain cache-resident.

A B+tree would add:

- internal-node update logic;
- root/branch split handling;
- more page-format variants;
- more recovery states;
- potentially more random reads.

Do not pay that complexity unless the one-level directory becomes measurably insufficient.

---

## 49. Why Append-Only COW Instead of Free-Page Reuse

Immediate page reuse looks space-efficient but introduces substantial complexity:

- free-list persistence;
- ABA/reuse lifetime concerns;
- reader interaction;
- allocator crash recovery;
- partial overwrite handling;
- corruption amplification.

Append-only COW gives a simpler rule:

```text
published bytes never change
```

Dead blocks are reclaimed only by Vacuum.

This is a strong fit for the project's immutable reader model.

---

## 50. Recommended Initial Implementation

Use:

```text
Package:                cryptohash
Algorithms:             SHA-256, BLAKE3-256
Hash width:             256 bits
Hash Go type:           [32]byte
ImageID:                uint64 alias
Record:                 40 bytes [Hash32][ImageID8]
Record ordering:        Hash, then ImageID
Physical organization:  immutable sorted COW blocks
Directory:              small sorted BlockRef array
Block size baseline:    16 KiB / 32 KiB benchmark
Target split occupancy: ~80%
Lookup:                 directory binary search + block binary search
Duplicate hashes:       adjacent records, may span blocks
Writer:                 one serialized writer
Writes:                 batched
WAL:                    batch-framed + CRC
Mutation publication:   one immutable root/view per Apply
Block mutation:         never in place
Dead block reuse:       none in V1
Checkpoint:             persist current root + WAL boundary
Vacuum:                 rewrite live blocks densely
Reader model:           ReaderTracker + immutable mmap
Checksums:              CRC32/IEEE
Structural alignment:   >= 8 bytes
Planner integration:    none initially
SIMD:                   none initially
Hash truncation:        none
```

---

## 51. Implementation Order

### Phase 1 — Hash Types and Block Format

Implement:

- `Hash`;
- `Algorithm`;
- `Entry`;
- 40-byte encoding;
- block header;
- sorted block validation;
- block lookup.

### Phase 2 — Directory and Multi-Block Lookup

Implement:

- `BlockRef` directory;
- directory binary search;
- duplicate runs across block boundaries;
- `Lookup`, `Contains`, `Count`.

### Phase 3 — Batched COW Mutation

Implement:

- mutation canonicalization;
- grouping by block;
- one rewrite per affected block;
- splitting;
- target occupancy;
- immutable root publication;
- ReaderTracker lifetime handling.

### Phase 4 — Persistence and WAL

Implement:

- generation header;
- mmap opening;
- WAL batches;
- recovery;
- atomic root/manifest publication;
- corruption tests.

### Phase 5 — Checkpoint and Vacuum

Implement:

- checkpoint WAL boundary;
- compact generation rewrite;
- dead-block reclamation;
- old-generation retirement after readers finish.

### Phase 6 — Hash Helpers

Implement:

- SHA-256 one-shot/streaming helpers;
- BLAKE3 one-shot/streaming helpers;
- known-answer tests.

### Phase 7 — Benchmark and Tune

Benchmark:

- 16/32/64 KiB blocks;
- split occupancy;
- batching sizes;
- directory representation;
- mmap lookup;
- storage amplification;
- SHA-256 vs BLAKE3 ingest throughput.

Do not add a B+tree, free-page reuse, or another lookup accelerator until these benchmarks show a need.

---

## 52. Summary

CryptoHash should support continuous image ingest without rewriting a monolithic 400+ MB sorted base.

The persistent logical record remains deliberately simple:

```text
[32-byte Hash][8-byte ImageID]
```

but records are grouped into immutable sorted blocks:

```text
               Block Directory
                     |
          +----------+----------+
          |          |          |
       Block 0    Block 1    Block N
          |          |          |
     sorted 40-byte records
```

Writes are batched and copy-on-write:

```text
Apply batch
   |
WAL
   |
group entries by affected block
   |
rewrite each affected block once
   |
split if needed
   |
append replacement block(s)
   |
publish new immutable directory/root
```

Readers remain simple:

```text
ReaderTracker
   |
binary-search small directory
   |
binary-search immutable mmap block
   |
return exact ImageIDs
```

Old blocks are not reused immediately. `Vacuum` periodically rewrites only the live block set into a compact generation.

This gives CryptoHash bounded write amplification under constant image ingestion while preserving the project's main principles:

- immutable lock-free reads;
- one serialized writer;
- mmap-friendly fixed-width storage;
- WAL-backed recovery;
- explicit reclamation;
- zero-allocation lookup where practical;
- benchmark-driven complexity.

The design intentionally stops short of a full B+tree. A one-level hot directory plus immutable COW blocks should provide most of the update benefit with substantially less code and fewer persistence/recovery states.
