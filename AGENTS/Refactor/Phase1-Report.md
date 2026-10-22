# Phase 1 Implementation Report

**Baseline:** `11f1fc8` on `refactor-index-design`
**Candidate:** working tree on `refactor-index-design` (uncommitted)
**Environment:** Go 1.27.0, darwin/arm64, Apple M4 Pro
**Commands:** `go test -count=1 ./...`, `go test -race -count=1 ./database/... ./store/... ./index/... ./search/...`, `go vet ./...`, `sigmap validate`, focused `benchstat`-style runs in `AGENTS/Refactor/bench/`.

## Implemented

- PR 2: replaced `store.Store`'s KV membership map with an unexported 8 KiB
  `kvNamespaceBits` (`1024 × uint64`). `schema.Definitions` order, maps,
  `schema.dat`, golden bytes, migration checks and index routes are unchanged.
- PR 3: added a single coordinator workspace with stable 64 KiB projection
  blocks, one bounded reusable oversized buffer, `dataRef` epochs, fixed-width
  queued descriptors, and a single contiguous copy of each journal
  before+after payload. Added optional `Options.DecodeInto`/`DecodeScratch`
  while retaining the existing `Decoder`.
- PR 4: replaced the O(N×M) `changed` helper with deterministic typed
  `slices.SortFunc` + linear merge, explicit exhausted-side handling, duplicate
  compaction, and NaN/signed-zero-consistent equality.
- PR 5: bounded non-adjacent same-entity coalescing. Groups are discovered with
  a linear scan over at most 64 entries; no map. SimHash and vector groups and
  delete→reinsert groups fall back to ordered sub-batches. Coalesced groups are
  acknowledged only at the last original sequence.
- PR 1/6: added typed cumulative counters for store commits, index applies,
  manifest writes, checkpoints, canonical/journal bytes, generation intent,
  acknowledgments and cleanup. Added focused benchmarks and lifetime/aliasing
  tests.

## Measurements

### KV namespace membership

| Lookup | ns/op | B/op | allocs/op |
|---|---:|---:|---:|
| map, 8 namespaces | 5.97 | 0 | 0 |
| map, 64 namespaces | 5.68 | 0 | 0 |
| map, 256 namespaces | 5.59 | 0 | 0 |
| bitmap, 8/64/256 | 0.26 | 0 | 0 |

The bitmap adds 8 KiB to each `store.Store`. This is a memory-layout change, not
an allocation reduction: both map and bitmap lookups are allocation-free.

### Projection diff (one family, 1/16/64/256 associations)

| Associations | old reference | sort-merge | old allocs | new allocs |
|---:|---:|---:|---:|---:|
| 1 | 67 ns | 121 ns | 2 | 0 |
| 16 | 1.46 µs | 633 ns | 2 | 0 |
| 64 | 16.9 µs | 6.22 µs | 2 | 0 |
| 256 | 245 µs | 31.8 µs | 2–3 | 0 |

The sort-merge path has a small-set crossover; for one association it is slower
in absolute terms but allocates nothing. Large projections are materially faster.

### Commit + index batches

| Batch | Baseline ns/op | After ns/op | Baseline B/op | After B/op | Allocs |
|---:|---:|---:|---:|---:|---:|
| 1 | 17.46 ms | 18.24 ms | 10.6 KiB | 14.6 KiB | 98 → 97 |
| 16 | 20.58 ms | 20.08 ms | 29.4 KiB | 40.2 KiB | 228 → 229 |
| 64 | 19.82 ms | 20.46 ms | 95.9 KiB | 112.2 KiB | 574 → 570 |

The B/op difference is the one-time coordinator workspace and first 64 KiB
block, not per-entry journal copies. Allocation counts are effectively
unchanged. Batch 32 was added: 20.0 ms, 61.9 KiB, 345 allocs.

### Index staging

`BenchmarkStoreApply` and `BenchmarkStoreApplyUpdate` remain within run-to-run
noise and keep the same allocation counts as baseline (for example 64 entities:
27.3 KiB/114 allocs vs 27.6 KiB/114 allocs).

### Pebble deferred encoding

Population-only benchmark, 64 records (not a full commit):
- `Batch.Set`: 531 ns/op, 0 allocs
- `SetDeferred` + direct copy: 414 ns/op, 0 allocs
- `store.Batch.Set` (validation + key encode): 763 ns/op, 0 allocs
- `SetDeferred` + `Commit(NoSync)`: 54.4 µs/op

No direct encoding was adopted: the generic database does not know the
application's encoded length, and the measured benefit is for the raw Pebble
population primitive rather than the full record path.

## Correctness and compatibility

- `go test -count=1 ./...` passes.
- `go test -race -count=1 ./database/... ./store/... ./index/... ./search/...`
  passes.
- `go vet ./...` passes.
- `sigmap validate` passes.
- Escape analysis (`go test -gcflags='-m=2' ./database ./store ./index`) shows
  escaping only on error/panic formatting and scratch-slice growth; the normal
  membership, staged-diff and coordinator copy paths do not allocate per entry.
- New tests cover block boundaries, empty/oversized payloads, byte budgets,
  stale refs, decoder input aliasing, duplicate associations, exhausted merge
  sides, NaN/signed-zero vectors, coalesced add/update, interleaved entities,
  time/geo/exact-hash coalescing, delete→reinsert fallback, no-op generation
  advancement, unsupported engine-family fallback, and a deterministic
  randomized bitmap comparison of coalesced vs sequential application including
  liveness and final generations.
- `schema.dat`, journal bytes, Pebble key shape, persisted index files and
  public `database.Commit` duplicate semantics are unchanged.

## Deliberate exclusions

- SimHash and vector coalescing remain sequential; their dense-ID/graph
  preconditions were not considered proven safe for this phase.
- No 128/256 database batch sizes: the existing 64 maximum is preserved.
- No MessagePack/EXIF codec or benchmark fixture was introduced; benchmarks use
  the repository's existing projection fixtures.
- Pebble direct encoding remains a measured option, not an adopted path.
- Sparse SimHash IDs, vector batching, journal delta encoding and cost-based
  search planning remain Phase 2 work.
