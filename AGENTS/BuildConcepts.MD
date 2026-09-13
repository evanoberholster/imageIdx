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
go test ./bitmapidx  -run '^$' -bench 'Benchmark(Contains|And4|ApplyBatch1000)' -benchmem
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
./           imageidx: shared ImageID and durability domain types
roar/        immutable zero-copy bitmap sets used directly by bitmapidx and geoidx
bitmapidx/   logical bitmap predicates and bounded deltas
timeidx/   standalone block-local timestamp index
geoidx/    S2 level-16 spatial index (dense cells are roar bitmaps)
internal/indexutil/ shared non-domain mechanisms used by all three indexes
search/      cross-index planner and conjunctive query executor
cmd/imageidx/ runnable one-shot and HTTP-server combined-search example
```

Dependency direction remains one-way: the root `imageidx` package imports no
subpackages; `roar` is standalone and imports only `internal/indexutil` (plus
its own `internal/bitmapkernel`) and the standard library; all indexes may
import the root, `roar`, and `internal/indexutil` as appropriate.
Specialized index packages remain independent of one another. `search` imports
all three indexes; index packages must never import `search`. Never introduce a
reverse dependency.

### Shared mechanisms live in `internal/indexutil`

- `ReaderTracker` provides two-slot read-side lifetime tracking for all three
  index packages. It is an internal mechanism, owned by an index or persistent
  store rather than supplied through public configuration.
- mmap/unmap, little-endian fixed-width mapped slices, alignment, and checked
  offset arithmetic are shared here.
- exact writes and fsync/rename/directory-sync manifest publication are shared
  here. Package-specific manifest encodings and WAL record formats remain in
  their owning packages.
- The package is internal implementation detail. It must not define database
  IDs, index metadata, format constants, or public index policy.

### Shared domain primitives live in `imageidx`

- `imageidx.ImageID` is the database-wide `uint64` alias. Every index package
  exposes the same alias, keeping storage and batch APIs zero-conversion.
- `imageidx.Durability` is shared by persistent index APIs; package constants
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

- `bitmapidx.ImageID`, `geoidx.ImageID`, and `timeidx.ImageID` are aliases
  of `imageidx.ImageID`.
- Cross-index search never converts or copies IDs solely because of package
  type differences.

`ImageID(0)` is reserved/invalid in `geoidx` only; `timeidx` and `bitmapidx`
do not reserve zero.

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
4. **Bounded read layers.** `Base + Frozen + Active` (bitmapidx/timeidx);
   `Sparse + Bitmap` (geoidx). Unbounded LSM-style delta chains are forbidden;
   apply backpressure or compact instead.
5. **SoA and fixed-width storage.** Persisted data is structure-of-arrays,
   little-endian, page-aligned, and mmap-friendly. No Go pointers are persisted;
   offsets + counts are.
6. **Checksummed formats.** Every persisted file has a `Magic uint32` +
   `Version uint16` header and CRC32 (IEEE) checksums. See format constants
   below.
7. **WAL + manifest + generation files.** Durable ordering is: append WAL,
   sync per durability policy, mutate/publish, acknowledge. Checkpoint writes a
   new generation, atomically publishes a manifest, then replays the WAL tail on
   reopen.
8. **`Config` with `withDefaults()`.** Every constructor validates and fills
   defaults through `(Config) withDefaults() (Config, error)`. Constants are
   tunable and must not be baked into the persisted format.
9. **Errors.** Each package defines its own exported sentinel errors prefixed
   with the package name (e.g. `bitmapidx: ...`, `timeidx: ...`,
   `geoidx: ...`).

## Package Contracts

### bitmapidx

- `New(Spec, Config)` is the only constructor. `Spec` contains the stable key,
  index ID, flags, and initial IDs.
- `Apply(Mutation)` is the canonical mutation API. A 1,000-ID mutation
  publishes once, never 1,000 times.
- If an ID appears in both `Mutation.Add` and `Mutation.Remove`, removal wins.
- `And`/`Or` materialize per-operand corrections before the N-way combination.
  `Count` skips materialization for delta-free operands via fused cardinality.
- And-planning is private and orders by Base cardinality only; delta
  cardinalities are ignored.
- Reader-visible views, deltas, segment entries, and raw segment operations are
  private. Public callers use `Metadata`, `Contains`, `AppendTo`, and the set
  operations instead.
- `Store` (`NewPersistent`/`OpenPersistent`) owns a private `Directory` and
  segment. It forwards `Lookup`, `LookupID`, `Indexes`, and `Len` without
  exposing internal storage fields; `Checkpoint`/`Vacuum` publish generations.
  All indexes in one store share its private reader tracker; standalone indexes
  own independent trackers.

### timeidx

- `New(Spec, Config)` follows the same stable `IndexID`/flags pattern as the
  other indexes. `Metadata` exposes a copied summary rather than mutable
  persisted headers.
- A single write-owner goroutine consumes reusable `Batch` values. Public
  mutations are sealed values made by `Insert`, `Delete`, and `Move`; `Apply`
  returns `ErrBackpressure` when a block's delta is awaiting compaction.
- The private delta is a fixed-capacity, single-writer append structure with a
  lock-free hash state table for readers. It must not allocate on append.
- `Search` is compressed-index only (zero-allocation). `SearchExact` re-verifies
  lossy boundary candidates via a caller-supplied canonical timestamp lookup.
- Persistence is a SoA `markers.<gen>.base` + `ids.<gen>.base` + `blocks.<gen>.idx`
  layout with an atomic manifest. Markers remain `uint32`; image IDs remain
  `uint64` in a separate column.

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

- `Compile(clauses...)` creates a conjunctive plan: every bitmap, timestamp,
  and geo clause must match. No clauses is rejected rather than interpreted as
  every image.
- Use `AllBitmaps`, `During`, and `WithinCells` to build sealed, validated
  clause values instead of exposing a parallel-slice query struct. Geographic
  query input is S2 cell IDs, never latitude/longitude bounds.
- `During` accepts `ExactTimestamp`. Exact geographic verification remains in
  the canonical-data layer rather than the candidate-index scheduler.
- `Compile` coalesces all bitmap clauses and orders typed execution stages by
  live cardinality estimates. Estimates affect execution order only, never
  correctness.
- Bitmap predicates remain one group. The first-stage path streams the
  smallest bitmap into caller storage and filters it in place; after another
  stage has produced candidates, `bitmapidx.Filter` tests them under
  one registration per distinct private tracker. Timestamp exact clauses can
  filter candidates in place; geographic stages accumulate cell ranges into
  the workspace-owned `roar.MutableBitmap` and materialize once, and the
  executor intersects.
- Combined results are sorted and unique. A caller-owned result slice and
  `Workspace` reuse all result/intermediate buffers, allowing zero-allocation
  steady-state searches when capacities are sufficient.
- Combined queries do not create a database-wide snapshot. Each stage observes
  one independently published index view, matching the existing per-index
  consistency contracts.

## Persisted Format Constants

| Package | Kind | Magic | Version |
|---|---|---|---|
| timeidx | manifest | `0x54534d46` TSMF | 2 |
| timeidx | WAL | `0x5453574c` TSWL | 1 |
| bitmapidx | segment | `0x424d5347` BMSG | 2 |
| bitmapidx | manifest | `0x424d4d46` BMMF | 1 |
| bitmapidx | WAL | `0x424d574c` BMWL | 1 |
| geoidx | storage generation | `0x47454f49` GEOI | 2 |
| geoidx | manifest | `0x47454f4d` GEOM | 1 |
| geoidx | WAL | `0x4745574c` GEWL | 1 |

All multi-byte integers are little-endian. All payloads are CRC32/IEEE
checksummed. Fixed records and structural boundaries are multiples of eight;
large payload starts use the stricter 4,096-byte page alignment. Native element
widths are unchanged: in particular, timestamp markers remain four-byte values
in their own SoA column, with padding only between structural regions or at a
file boundary.

## Tunable Defaults (not part of the persisted format)

| Package | Constant | Value |
|---|---|---|
| timeidx | `DefaultBlockSize` | 65,536 entries |
| timeidx | `DefaultDeltaCapacity` | 2,048 |
| timeidx | `TimestampShift` | 24 |
| bitmapidx | `DefaultActiveTarget` | 2,048 |
| bitmapidx | `DefaultHardDeltaLimit` | 4,095 |
| geoidx | `GeoS2Level` | 16 |
| geoidx | sparse slot count (format-private) | 4 |
| geoidx | `DefaultPublishThreshold` | 256 |
| geoidx | `DefaultCoverCells` | 32 |

These are benchmark-driven; change them only with supporting evidence.

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
