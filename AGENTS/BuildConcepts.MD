# Build Concepts

Operational guide for building, testing, and extending `imageIdx`. The
`BitmapIdxProposal.MD`, `TimestampIdx.MD`, and `GeoIdxProposal.MD` documents are
the design sources of truth; this document records how the implemented code is
actually organized, the cross-package contracts, and the commands an agent must
run before considering work complete.

## Status

All three index subsystems are implemented, tested, and benchmarked. They are
scalar-only: SIMD kernels are intentionally reserved and must not be added
before scalar correctness and benchmarks are stable.

## Module and Dependencies

- Module path: `github.com/evanoberholster/imageIdx`
- Go: `1.27.0`
- `github.com/dgraph-io/sroar` — roaring bitmaps (bitmapidx, geoindex)
- `github.com/golang/geo/s2` — S2 geometry (geoindex)
- `github.com/pkg/errors` — indirect

## Build, Test, Benchmark

```sh
go build ./...
go test ./...
go test -race ./...
go run .
```

Per-package benchmarks:

```sh
go test ./timestamp  -run '^$' -bench 'Benchmark(Search|Update|DeltaCapacity|BlockSize)' -benchmem
go test ./bitmapidx  -run '^$' -bench 'Benchmark(Contains|And4|WriteBatch1000)' -benchmem
go test ./geoindex   -run '^$' -bench 'Benchmark(SingleCell|BoundingBox|BatchIngest1000)' -benchmem
go test ./search     -run '^$' -bench 'BenchmarkCombinedSearch' -benchmem
```

Set `TIMESTAMP_BENCH_LARGE=1` to enable the synthetic 10M and 100M-entry
timestamp benchmark builds (they are skipped by default).

Every change must pass `go build ./...`, `go test ./...`, and
`go test -race ./...` before it is considered done.

## Package Layout and Dependencies

```text
bitmapidx/   foundation: ImageID, IndexType, BitmapRef, Epoch
timestamp/   standalone block-local timestamp index
geoindex/    S2 level-16 spatial index (depends on bitmapidx)
internal/indexutil/ shared non-domain mechanisms used by all three indexes
search/      cross-index planner and conjunctive query executor
main.go      placeholder ("imageIdx")
```

Dependency direction remains one-way: `geoindex` imports `bitmapidx`, while all
three index packages may import `internal/indexutil`. `timestamp` remains
independent of the other public index packages. `search` imports all three
indexes; index packages must never import `search`. Never introduce a reverse
dependency.

### Shared mechanisms live in `internal/indexutil`

- `ReaderEpoch` provides the common two-slot reader-counting algorithm for
  package-local epoch adapters. `bitmapidx.Epoch` keeps its equivalent loop
  inline because adding an adapter branch changes its benchmark-critical query
  path; `geoindex` already shares that public epoch.
- mmap/unmap, little-endian fixed-width mapped slices, alignment, and checked
  offset arithmetic are shared here.
- exact writes and fsync/rename/directory-sync manifest publication are shared
  here. Package-specific manifest encodings and WAL record formats remain in
  their owning packages.
- The package is internal implementation detail. It must not define database
  IDs, index metadata, format constants, or public index policy.

### Shared primitives live in `bitmapidx`

- `bitmapidx.ImageID` (`uint64`) is the database-wide ID type.
- `bitmapidx.BitmapRef` is the immutable roaring handle. `geoindex` reuses it for
  dense S2 cells; it must not be copied into other packages.
- `bitmapidx.Epoch` is the query-wide RCU reclamation primitive. `geoindex`
  accepts one through `geoindex.Config.Epoch` so a multi-index query enters once.

### `ImageID` is not one type everywhere

This is the most common cross-package trap:

- `bitmapidx.ImageID` is the canonical type (`bitmapidx/types.go`).
- `geoindex.ImageID` is an **alias**: `type ImageID = bitmapidx.ImageID`.
- `timestamp.ImageID` is a **separate distinct type** (`timestamp/types.go`).
  Values must be explicitly converted between `timestamp.ImageID` and the
  `bitmapidx`/`geoindex` `ImageID`.

`ImageID(0)` is reserved/invalid in `geoindex` only; `timestamp` and `bitmapidx`
do not reserve zero.

## Core Build Concepts

Every index follows the same conceptual skeleton. New code must conform.

1. **Immutable, copy-on-write publication.** Reader-visible state is reachable
   from one struct published through an `atomic.Pointer[T]`. Readers do one
   `Load()`; writers build a complete replacement and `Store()` it. Nothing
   reachable from a published view is ever mutated.
2. **No locks on the search path.** `Mutex`/`RWMutex` are forbidden in search
   code. `bitmapidx` and `geoindex` use `Epoch`; `timestamp` uses its own
   `readerEpoch`.
3. **Epoch/RCU reclamation.** Atomic publication alone does not make old memory
   reusable. Retired views, deltas, and mmap regions are only reclaimed after
   `Synchronize()` guarantees no reader still references them.
4. **Bounded read layers.** `Base + Frozen + Active` (bitmapidx/timestamp);
   `Sparse + Bitmap` (geoindex). Unbounded LSM-style delta chains are forbidden;
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
   with the package name (e.g. `bitmapidx: ...`, `timestamp: ...`,
   `geoindex: ...`).

## Package Contracts

### bitmapidx

- `New(key BitmapKey, indexID uint64, ids []uint64, config Config)` and
  `NewBitmapIndex` are equivalent.
- `Write(BitmapMutation)` is the canonical API; `Add`/`Remove` are wrappers.
  A 1,000-ID `Add` publishes once, never 1,000 times.
- If an ID appears in both `Additions` and `Removals`, removal wins.
- `And`/`Or` materialize `effectiveBitmap()` today. A future delta-aware
  correction path (Base-only ops + re-evaluating delta-touched IDs) is proposed
  but not implemented.
- `PlanAND` orders by Base cardinality only; delta cardinalities are ignored.
- `Store` (`NewPersistent`/`OpenPersistent`) wraps a `BitmapDirectory` and a
  segment file; `Checkpoint`/`Vacuum` publish new generation files.

### timestamp

- Single write-owner goroutine fed by a bounded batch channel. `Submit` returns
  `ErrBackpressure` when a block's delta is awaiting compaction; callers retry.
- `TimestampDelta` is a fixed-capacity, single-writer append structure with a
  lock-free hash state table for readers. It must not allocate on append.
- `Search` is compressed-index only (zero-allocation). `SearchExact` re-verifies
  lossy boundary candidates via a caller-supplied canonical timestamp lookup.
- Persistence is a SoA `markers.<gen>.base` + `ids.<gen>.base` + `blocks.<gen>.idx`
  layout with an atomic manifest.

### geoindex

- One coordinate per image, normalized to S2 level 16 (`cellID >> 28`).
- Sparse cells (1-4 IDs) use a fixed 4-slot SoA; dense cells (5+) use
  `bitmapidx.BitmapRef`.
- Searches load one `GeoIndexView` and never inspect writer-side mutations.
  Visibility is threshold/timer driven; `Flush()` forces a boundary.
- `Nearest` is currently a full scan over `All()`; the expanding-S2-cap executor
  is future work. Keep its API compatible.
- `RangesFromCellUnion` + `NormalizeRanges` convert S2 coverage to sorted,
  non-overlapping, inclusive `S2KeyRange`s.

### search

- `Query` is conjunctive: every bitmap, timestamp range, and geo range must
  match. An empty query is rejected rather than interpreted as every image.
- `Scheduler.Plan` orders immutable stage descriptors by live cardinality
  estimates. Estimates affect execution order only, never correctness.
- Bitmap predicates remain one group so the first-stage path uses
  `bitmapidx.And`; after another stage has produced candidates,
  `bitmapidx.FilterInto` tests those candidates under one set of epoch guards.
- Timestamp IDs are converted explicitly to the canonical
  `bitmapidx.ImageID`; do not change `timestamp.ImageID` into a cross-package
  alias.
- Combined results are sorted and unique. A caller-owned `Workspace` reuses
  intermediate ID buffers.
- Combined queries do not create a database-wide snapshot. Each stage observes
  one independently published index view, matching the existing per-index
  consistency contracts.

## Persisted Format Constants

| Package | Kind | Magic | Version |
|---|---|---|---|
| timestamp | index header | `0x54534958` TSIX | 1 |
| timestamp | manifest | `0x54534d46` TSMF | 1 |
| timestamp | WAL | `0x5453574c` TSWL | 1 |
| bitmapidx | format | `0x424d4958` BMIX | 1 |
| bitmapidx | segment | `0x424d5347` BMSG | 1 |
| bitmapidx | manifest | `0x424d4d46` BMMF | 1 |
| bitmapidx | WAL | `0x424d574c` BMWL | 1 |
| geoindex | storage generation | `0x47454f49` GEOI | 1 |
| geoindex | manifest | `0x47454f4d` GEOM | 1 |
| geoindex | WAL | `0x4745574c` GEWL | 1 |

All multi-byte integers are little-endian. All payloads are CRC32/IEEE
checksummed. Page/block alignment is 4,096 bytes.

## Tunable Defaults (not part of the persisted format)

| Package | Constant | Value |
|---|---|---|
| timestamp | `DefaultBlockSize` | 65,536 entries |
| timestamp | `DefaultDeltaCapacity` | 2,048 |
| timestamp | `TimestampShift` | 24 |
| bitmapidx | `DefaultActiveTarget` | 2,048 |
| bitmapidx | `DefaultHardDeltaLimit` | 4,095 |
| geoindex | `GeoS2Level` | 16 |
| geoindex | `GeoSparseSlots` | 4 |
| geoindex | `DefaultPublishThreshold` | 256 |
| geoindex | `DefaultCoverCells` | 32 |

These are benchmark-driven; change them only with supporting evidence.

## Conventions When Extending

- Add new functionality to an existing package; do not duplicate roaring, epoch,
  WAL, or mmap code into a new package.
- Mirror the existing `*_test.go` split: unit tests, storage tests
  (`storage_test.go`), and benchmarks (`benchmark_test.go`). Use
  `b.ReportAllocs()` and `b.ResetTimer()` in benchmarks.
- Use `t.TempDir()` for persistent-storage tests.
- Scalar kernels first; SIMD only where a profile demonstrates value.
- Keep `ImageID` `uint64` in every persisted structure and WAL record.
- When a new persisted type is introduced, add a magic + version and a CRC, and
  validate them on every read path.
