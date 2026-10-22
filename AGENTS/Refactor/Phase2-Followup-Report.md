# Phase 2 Follow-up Report

**Baseline:** `b7bb790` (PR #25)
**Branch:** `phase2` (`51bcbd8`)
**PR:** https://github.com/evanoberholster/imageIdx/pull/25
**Environment:** Go 1.27.0, darwin/arm64, Apple M4 Pro

## P0 changes

### Sparse SimHash

- Replaced the experimental append-only `sparse.bin`/`*.sparse` overlay with a
  new segmented format:
  - immutable CRC-checked `sparse-<generation>.seg` files;
  - atomically replaced CRC-checked `sparse.manifest`;
  - source generation in the manifest for replay idempotence;
  - atomic segment replacement, directory sync, orphan cleanup, explicit legacy
    format rejection.
- Ordinary updates write one bounded delta segment and republish a base+overlay
  reader view without copying the full sparse population. Segment-count pressure
  merges only the delta tier; a full base+delta merge is reserved for the
  stale/delta-record threshold. The full merge is the only whole-population copy
  boundary.
- Tombstones that suppress dense records survive compaction; tombstones above
  the dense high-water mark are retired.
- `SearchPHash`, `SearchPDQ`, `SearchPDQBulk`, `Get` and `Quality` now apply
  sparse overrides consistently. Scalar and bulk PDQ equivalence, prefixes,
  high IDs, dense+sparse mixes, tombstones, zero matches and corruption are
  covered.
- Crash seams cover segment write, sync, rename, manifest write, publish,
  compaction and manifest replacement. Database crash tests cover durable index,
  acknowledgment and cleanup boundaries.

### Canonical projection mode

- The mode is persisted in schema-owned config bytes in `schema.dat`.
- `CanonicalProjectionUnset` reads the durable mode, defaulting to inline for an
  unconfigured database. Explicit mismatches fail with
  `ErrCanonicalModeMismatch`.
- External mode accepts `ProjectFromRecord` or the additive
  `ProjectInto(dst, record)` form.
- Mode changes are rejected. Projection-version changes require external mode
  and a projector callback.

### Journal v2

- Added destination-oriented `appendMutation`; `Commit` reuses a
  `journalScratch` buffer. Pebble copies the value before the next mutation.
- Added round-trip tests for every vector element kind, bitmap enum/bool/int/BSI,
  invalid family/kind/namespace/count/length and header flag reuse.
- Added randomized time/geo/hash sequential-vs-coalesced equivalence coverage.

### Projection migration

- Fixed schema recovery for a PREPARED target that changes projection versions
  without adding namespaces.
- Downgrades are rejected.
- External mode and a valid projector are required for projection-semantic
  changes.
- Added interrupted prepare/activate tests.

### Capability contract

`index.FamilyCapabilitiesFor` records coalescing, delete-by-ID, replay
idempotence and batching per family; database coalescing dispatch uses it.

## Benchmarks

### Journal encoder

| Path | ns/op | B/op | allocs/op |
|---|---:|---:|---:|
| Alloc (`encodeMutation`) | 61.9 | 80 | 1 |
| Reused scratch (`appendMutation`) | 51.7 | 0 | 0 |

### Photographic logical-write matrix

`BenchmarkPhotoMetadataWorkloads`, 36-byte projection, logical application
counters only (not physical device writes):

Selected batch-64 logical bytes (36-byte projection):

| Workload | journal B/op | store B/op |
|---|---:|---:|
| insert | 145.0 | 188.5 |
| rating | 63.0 | 106.5 |
| tags | 77.0 | 120.5 |
| GPS | 42.1 | 85.6 |
| AI | 63.0 | 106.5 |
| unchanged | 27.0 | 70.5 |

Focused output: `AGENTS/Refactor/bench/phase2-followup-workloads.txt`. These are
logical application counters; device I/O and fsync are not inferred from them.

### Sparse scale

Latest measured (Apple M4 Pro, sparse IDs, batch 64):

| Live IDs | pHash search | PDQ scalar | PDQ bulk | Hot apply batch 64 | Segments |
|---:|---:|---:|---:|---:|---:|
| 10K | 14.7 µs | 35.9 µs | 36.1 µs | 411 µs | 29 |
| 100K | 165 µs | 357 µs | 356 µs | 1.34 ms | 51 |
| 1M | 1.51 ms | 3.43 ms | 3.41 ms | 5.19 ms | 2–11 |

The segmented publisher is now bounded at 64 immutable segments. Ordinary
updates do not copy the base; the remaining apply cost is dominated by durable
segment/manifest syscalls. Sparse search is a bounded base+overlay scan; it is
intentionally specialized rather than a general LSM. Full history/stale-ratio
tuning remains a follow-up.

## Validation

- `go build ./...` passed.
- `go test -count=1 ./...` passed on all packages.
- `go test -race -count=1 ./...` passed on all packages.
- `go vet ./...` passed.
- `sigmap validate` passed; 88% indexed-file coverage (372/421).
- Targeted journal, sparse corruption/publication, canonical-mode, projection
  migration, random coalescing, and sparse crash-boundary tests passed.

## Deferred

- Full requested photographic matrix including embedding change, Pebble
  WAL/flush/compaction metrics, ingest p50/p95/p99 and statistical comparison.
- 10M sparse tier remains opt-in behind `IMAGEIDX_BENCH_LARGE`.
- Sparse search uses a scanned base+overlay; no persistent sparse ID lookup index.
