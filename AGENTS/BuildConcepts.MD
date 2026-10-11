# Build Concepts

Operational guide for building, testing, and extending `imageIdx`. The
`BitmapIndex.md`, `Timeidx.md`, `Geoidx.md`, `Search.md`, and
`RoarFormat.md` describe the implemented designs. This document records the
cross-package contracts and the commands an agent must run before considering
work complete.

## Status

All three index subsystems are implemented, tested, and benchmarked. They are
scalar-only: SIMD kernels are intentionally reserved and must not be added
before scalar correctness and benchmarks are stable.

## Module and Dependencies

- Module path: `github.com/evanoberholster/imageIdx`
- Go: `1.27.0`
- `github.com/weaviate/sroar` — test-only benchmark and differential baseline for `roar`
- `github.com/golang/geo/s2` — S2 geometry (geoidx)
- `github.com/pkg/errors` — indirect

## Build, Test, Benchmark

```sh
go build ./...
go test ./...
go test -race ./...
go run ./cmd/imageidx
```

Per-package benchmarks:

```sh
go test ./timeidx  -run '^$' -bench 'Benchmark(Search|Update|DeltaCapacity|BlockSize)' -benchmem
go test ./bitmapidx  -run '^$' -bench 'Benchmark(Field|Compiled)' -benchmem
go test ./geoidx   -run '^$' -bench 'Benchmark(SingleCell|BoundingBox|BatchIngest1000)' -benchmem
go test ./search     -run '^$' -bench 'BenchmarkCombinedSearch' -benchmem
go test ./roar       -run '^$' -bench . -benchmem
```

Set `TIMESTAMP_BENCH_LARGE=1` to enable the synthetic 10M and 100M-entry
timestamp benchmark builds (they are skipped by default).

Every change must pass `go build ./...`, `go test ./...`, and
`go test -race ./...` before it is considered done.

## Package Layout and Dependencies

```text
common/        shared ImageID/ValueID, namespace and durability domain types
roar/          immutable zero-copy bitmap sets used by the indexes
index/bitmapidx/  persistent typed fields and bounded per-field deltas (package bitmapidx)
index/timeidx/    namespace-aware exact-timestamp COW index (package timeidx)
internal/storage/   shared mmap, superblock/commit, and reader-tracking mechanisms
internal/pagealloc/ shared 512-byte copy-on-write extent allocator
search/      cross-index planner and conjunctive query executor
cmd/imageidx/ runnable one-shot and HTTP-server combined-search example
```

Dependency direction remains one-way: `common` imports no subpackages; `roar`
is standalone and imports only `internal/storage` (plus its own
`internal/bitmapkernel`) and the standard library; all indexes may import
`common`, `roar`, and the shared `internal/storage`/`internal/pagealloc`
mechanisms as appropriate. Specialized index packages remain independent of one
another. `search` imports the indexes; index packages must never import
`search`. Never introduce a reverse dependency.

### Shared mechanisms live in `internal/storage`

- `ReaderTracker` provides two-slot read-side lifetime tracking for the index
  packages. It is an internal mechanism, owned by an index or persistent store
  rather than supplied through public configuration.
- mmap/unmap, little-endian fixed-width mapped slices, alignment, and checked
  offset arithmetic are shared here.
- exact writes, dual-slot CRC32C superblock commits, extent unions and
  fsync/rename/directory-sync publication are shared here. Package-specific
  payload schemas, manifest encodings and WAL record formats remain in their
  owning packages.
- The package is internal implementation detail. It must not define database
  IDs, index metadata, format constants, or public index policy.

### Shared domain primitives live in `common`

- `common.ImageID` and `common.ValueID` are the database-wide `uint64` aliases.
  Every index package uses them, keeping storage and batch APIs zero-conversion.
- `common.Durability` is shared by persistent index APIs; package constants
  remain aliases for convenient migration.

### Immutable ID sets live in `roar`

- `roar.Bitmap` is the immutable set handle shared by logical bitmap
  predicates and dense S2 cells. Its zero value is an empty set; every method
  is nil-safe.
- `roar.DeltaBuilder` owns mutable replacement construction (deferred ID
  additions/removals plus whole-set Or/And/AndNot). Published `Bitmap` values
  must never be mutated.
- `roar.FromSorted` builds from strictly increasing IDs; `roar.FromUnsorted`
  accepts any order (copying only when canonicalization is needed).
  `roar.Open`/`OpenTrusted` and `Bitmap.MarshalBinary` are the common
  owned/mmap buffer boundary. Package-specific directories and file formats
  remain separate.
- Index packages use `roar.Bitmap` directly; they do not expose representation
  aliases.

### Zero-copy bitmap index lives in `roar`

`roar` implements `AGENTS/RoarFormat.md`: the serialized bytes are the read
representation.

- Format: 16-byte header, sorted 16-byte `{high-48 key, offset}` entries,
  8-byte container headers, sorted `uint16` array containers, and
  1,024-`uint64` bitmap containers. Everything is little-endian.
- The on-disk format is 8-byte aligned throughout: header, key table,
  container headers, and bitmap payloads all begin on 8-byte boundaries and
  every container occupies a multiple of 8 bytes. `Open` enforces this;
  `TestSerializedAlignment` guards it.
- `Open` validates untrusted bytes with the portable safe decoder;
  `OpenTrusted` skips validation for bytes a storage layer has already
  checked. Zero-copy access requires a little-endian host and an 8-byte
  aligned buffer base.
- `Bitmap` methods (`Contains`, `AndCardinality`, `AndManyCardinality`,
  `Intersects`) allocate nothing. `AppendTo` is the fast materialization
  path; `Iterator.Next/Value` is the compatibility API and is slower than
  `AppendTo` because the Go inliner refuses the per-value state machine.
- `FromSorted` builds an immutable `Bitmap` from strictly increasing IDs;
  `FromUnsorted` accepts any order. Construction is two-pass: one
  validation/layout pass, one write pass into a single allocation.
  `DeltaBuilder` accumulates deferred ID edits and whole-set operations;
  `Builder` accumulates operation results; `Compact` assembles them.
  `MutableBitmap` is the transient search-time accumulator over the same
  machinery: single-owner, workspace-resident, never persisted. ID-only
  accumulation materializes with one sort and no bitmap involvement; folds
  apply eagerly so retained bytes stay valid after read guards release, and
  fold assemblies recycle one owned result buffer. Steady-state search
  accumulation allocates nothing.
  Thresholds (`ArrayToBitmapThreshold` 2048,
  `linearKeySearchThreshold` 16, `arrayLinearSearchThreshold` 16) are runtime
  policies, not format guarantees.
- `roar` benchmarks compare against `github.com/weaviate/sroar` on
  photo-DB-shaped distributions (geo cells, people, tags, dense time
  buckets, favorites, mixed). Note: sroar's `And`/`Or`/`AndNot` mutate their
   receiver, so benchmark and differential code must clone first.

### `ImageID` is one type everywhere

- `common.ImageID` is the database-wide `uint64` alias shared by every index
  package; `timeidx` entries and results use the generic `common.ValueID`.
- Cross-index search never converts or copies IDs solely because of package
  type differences.

`common.ValueID(0)` and `MaxUint64` are valid in `timeidx`; other index families
may reserve zero as their own sentinel.

## Core Build Concepts

Every index follows the same conceptual skeleton. New code must conform.

1. **Immutable, copy-on-write publication.** Reader-visible state is reachable
   from one struct published through an `atomic.Pointer[T]`. Readers do one
   `Load()`; writers build a complete replacement and `Store()` it. Nothing
   reachable from a published view is ever mutated.
2. **No locks on the search path.** `Mutex`/`RWMutex` are forbidden in search
   code. Public read operations register and release through their index's
   internal `ReaderTracker`; callers never create a guard.
3. **Read-side lifetime tracking.** Atomic publication alone does not make old
   memory reusable. Retired views, deltas, and mmap regions are only reclaimed
   after `WaitForPriorReaders()` guarantees no earlier reader still references
   them. `Close` performs this wait before unmapping storage.
4. **Bounded read layers.** `Base + Frozen + Active` (bitmapidx) and
   `Sparse + Bitmap` (geoidx) bound their deltas. `timeidx` has no delta layer:
   every accepted batch publishes a complete compacted snapshot. Unbounded
   LSM-style delta chains are forbidden; apply backpressure or compact instead.
5. **SoA and fixed-width storage.** Persisted data is structure-of-arrays,
   little-endian, page-aligned, and mmap-friendly. No Go pointers are persisted;
   offsets + counts are.
6. **Checksummed formats.** Every persisted format carries a `Magic uint32` +
   `Version uint16` identity and CRC checksums: dual-slot CRC32C (Castagnoli)
   superblock envelopes for index generations, CRC32/IEEE for manifest files.
   See format constants below.
7. **Durable ordering.** WAL-based indexes append the WAL, sync per durability
   policy, mutate/publish, then acknowledge; checkpoint writes a new generation
   and atomically publishes a manifest, replaying the WAL tail on reopen.
   `timeidx` instead commits one complete copy-on-write generation behind two
   sync barriers and recovers from one of two checksummed superblocks — no WAL,
   manifest, or generation files.
8. **`Config` with `withDefaults()`.** Every constructor validates and fills
   defaults through `(Config) withDefaults() (Config, error)`. Constants are
   tunable and must not be baked into the persisted format.
9. **Errors.** Each package defines its own exported sentinel errors prefixed
   with the package name (e.g. `bitmapidx: ...`, `timeidx: ...`,
   `geoidx: ...`).

## Package Contracts

### bitmapidx

- `Field` is the only persisted bitmap abstraction. A fixed `Registry` defines
  the typed enum, bool, text, continuous, and geo fields in a store.
- `NewIndex`/`OpenIndex` create or reopen a
  `Index`. The store owns field handles, one WAL, checkpoint generations,
  and all mmap lifetimes. Callers mutate through `Add`, `Remove`, `Move`,
  `ReplaceText`, and `Apply` on the returned fields.
- Field queries use typed operations such as `MatchValues`, `MatchText`,
  `Range`, `GeoRanges`, and their count/filter variants. There is no generic
  predicate `Index`, `Store`, `Mutation`, or bitmap-level `And`/`Or` API.
- Reader-visible views, per-value deltas, segment entries, and raw segment
  operations are private. Field results are immutable `roar.Bitmap` values;
  transient mutable bitmap accumulation belongs to `search` only.

### timeidx

- Namespace-aware, exact 1900-epoch nanosecond index: one timestamp per
  `common.ValueID` per namespace. `NewMemory`, `NewStore`/`OpenStore` (single
  namespace), and the `Store` manager (`NewNamespaceStore`/`InitializeStore` +
  `SealStartup`) cover memory and persistent use. `Spec.NamespaceDef()` yields
  the `common.NamespaceDef` to register in a `common.Schema`.
- One writer per namespace serializes reusable `Batch` values. Mutations are
  sealed values made by `Insert`, `Delete`, and `Move`; `Apply` publishes a
  complete immutable snapshot before returning, and `ApplyWithGeneration`
  commits a DB completeness watermark in the same superblock.
- `Search` returns exact `[start, end)` candidates in `(time, ID)` order with
  zero allocation given caller capacity. `SearchExact` keeps an optional
  canonical callback for compatibility; `EstimateRange` returns the exact
  snapshot-local count.
- Persistence is one COW file per namespace (magic `TIM4`, version 5): a 512-byte
  two-slot CRC32C superblock, a 64-byte payload, and a 56-byte shard directory
  over size-bounded `uint64` time and ID columns (16 bytes/entry). There is no
  WAL, delta layer, or background compactor. See `AGENTS/Timeidx.md` and
  `index/timeidx/README.md`.

### geoidx

- One coordinate per image, normalized to S2 level 16 (`cellID >> 28`).
- Sparse cells (1-4 IDs) use a fixed 4-slot SoA; dense cells (5+) use
  `roar.Bitmap`.
- Searches load one private immutable `indexView` and never inspect writer-side
  mutations. The view contains 256 independently replaceable shards;
  visibility is threshold/timer driven and `Flush()` forces a boundary.
- Public writes use explicit `geoidx.Mutation{ImageID, From, To}` values.
  Zero is the absent/removal sentinel, avoiding a reverse image-to-cell map.
- `Nearest` is currently a full scan over `AppendTo()`; the expanding-S2-cap executor
  is future work. Keep its API compatible.
- `RangesFromCellIDs` converts valid S2 cells at levels 0-16 to sorted,
  non-overlapping, inclusive level-16 `Range`s.
- `CoverRegion(dst, region, options)` accepts reusable `CoverWorkspace` scratch
   and caller-owned output. With sufficient capacities, the geoidx coverer
  adds no allocations; a supplied `s2.Region` implementation may allocate in
  its own methods.

### search

- `Compile(clauses...)` creates a conjunctive plan: every field, timestamp,
  and geo clause must match. No clauses is rejected rather than interpreted as
  every image.
- Use `FieldValues`, `FieldText`, `FieldRange`, `During`, and `WithinCells` to
  build sealed, validated clause values. Geographic query input is S2 cell
  IDs, never latitude/longitude bounds.
- `During` accepts `ExactTimestamp`. Exact geographic verification remains in
  the canonical-data layer rather than the candidate-index scheduler.
- `Compile` orders typed field, timestamp, and geo stages by
  live cardinality estimates. Estimates affect execution order only, never
  correctness.
- The first-stage path streams the smallest field result into caller storage
  and filters it in place; later field stages use their typed filter methods.
  Timestamp exact clauses can filter candidates in place; geographic stages
  accumulate cell ranges into the workspace-owned `roar.MutableBitmap` and
  materialize once, and the executor intersects.
- Combined results are sorted and unique. A caller-owned result slice and
  `Workspace` reuse all result/intermediate buffers, allowing zero-allocation
  steady-state searches when capacities are sufficient.
- Combined queries do not create a database-wide snapshot. Each stage observes
  one independently published index view, matching the existing per-index
  consistency contracts.

## Persisted Format Constants

| Package | Kind | Magic | Version |
|---|---|---|---|
| timeidx | namespace superblock | `0x54494d34` TIM4 | 5 |
| bitmapidx | field segment | `0x424d4653` BMFS | 1 |
| bitmapidx | field manifest | `0x424d464d` BMFM | 1 |
| bitmapidx | field WAL | `0x424d4657` BMFW | 1 |
| geoidx | storage generation | `0x47454f49` GEOI | 2 |
| geoidx | manifest | `0x47454f4d` GEOM | 1 |
| geoidx | WAL | `0x4745574c` GEWL | 1 |

All multi-byte integers are little-endian. Superblock envelopes and their
payloads are CRC32C (Castagnoli) checksummed; manifest files use CRC32/IEEE.
Fixed records and structural boundaries are multiples of eight; large payload
starts use the stricter 4,096-byte page alignment. Native element widths are
preserved: `timeidx` stores exact eight-byte timestamp keys and eight-byte value
IDs in separate SoA columns (16 payload bytes per entry).

## Tunable Defaults (not part of the persisted format)

| Package | Constant | Value |
|---|---|---|
| timeidx | `MaxShardBytes` | 4 KiB |
| timeidx | `MaxFileBytes` (initial arena) | 4 MiB |
| bitmapidx | `DefaultActiveTarget` | 2,048 |
| bitmapidx | `DefaultHardDeltaLimit` | 4,095 |
| geoidx | `GeoS2Level` | 16 |
| geoidx | sparse slot count (format-private) | 4 |
| geoidx | `DefaultPublishThreshold` | 256 |
| geoidx | `DefaultCoverCells` | 32 |

`timeidx` also retains `DefaultBlockSize`, `DefaultDeltaCapacity`, and
`TimestampShift` as deprecated source-compatibility constants; none affects
encoding. These defaults are benchmark-driven; change them only with supporting
evidence.

## Conventions When Extending

- Add new functionality to an existing package; do not duplicate reader
  tracking, WAL, or mmap code into a new package. Specialized indexes use the
  `roar.Bitmap` representation boundary directly; `weaviate/sroar` remains a
  test-only benchmark baseline.
- Mirror the existing `*_test.go` split: unit tests, storage tests
  (`storage_test.go`), and benchmarks (`benchmark_test.go`). Use
  `b.ReportAllocs()` and `b.ResetTimer()` in benchmarks.
- Use `t.TempDir()` for persistent-storage tests.
- Scalar kernels first; SIMD only where a profile demonstrates value.
- Keep `ImageID` `uint64` in every persisted structure and WAL record.
- When a new persisted type is introduced, add a magic + version and a CRC, and
  validate them on every read path.
