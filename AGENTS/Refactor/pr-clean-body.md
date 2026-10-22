## Summary

This PR implements Phase 2 and its follow-up hardening for the pre-V1 index format:

- Typed, CRC-protected journal v2 deltas instead of full before/after projections.
- Durable per-namespace projection versions with an explicit restart migration.
- Inline and external canonical projection modes, persisted in `schema.dat`.
- Arbitrary external-ID support for SimHash through a durable segmented sparse overlay.
- Conservative delta coalescing with correct liveness and ordered fallback.

The follow-up also closes sparse search parity, replay/idempotence, crash-boundary, canonical-mode persistence and journal-family validation gaps.

## Journal v2

- Journal entries carry only typed removed/added associations, with explicit insert, no-op and delete flags.
- All association families round-trip deterministically: time, geo, exact hash, SimHash, bitmap enum/bool/int/BSI and every vector element kind.
- Unsupported versions, unknown families/kinds, invalid namespaces, bad counts/lengths and corrupt headers fail closed.
- `Commit` uses a destination-oriented `appendMutation` and reuses a commit-owned buffer; Pebble copies values before reuse.
- Coalescing merges typed deltas rather than re-decoding full projections. SimHash, vector and delete→reinsert sequences retain ordered fallback.

## Canonical projection modes

- Canonical values use a magic-tagged versioned header.
  - **Inline**: canonical values retain projection bytes plus the application record.
  - **External**: canonical values retain only the application record; `ProjectFromRecord` or `ProjectInto` reconstructs projection bytes.
- The selected mode is persisted in schema-owned config bytes in `schema.dat`; reopening with a conflicting explicit mode fails with `ErrCanonicalModeMismatch`.
- External mode requires a projector callback on every path that needs reprojection.
- Mode changes are rejected. Projection-version changes require external mode and a projector and use the existing PREPARED/ACTIVATE rebuild path.
- Version downgrades and unrelated schema changes are rejected.

## Sparse SimHash

- Dense contiguous IDs keep the existing append path.
- Non-dense and high `uint64` external IDs use an immutable segmented sparse overlay:
  - CRC-checked `sparse-<generation>.seg` files;
  - atomically replaced CRC-checked `sparse.manifest`;
  - replay source generation in the manifest;
  - bounded base+overlay reader views;
  - tiered compaction, with a full base+delta merge only at the stale/delta-record threshold;
  - dense-suppressing tombstones retained across compaction.
- `SearchPHash`, `SearchPDQ`, `SearchPDQBulk`, `Get` and `Quality` share sparse override and tombstone semantics.
- Scalar/bulk PDQ equivalence, caller prefixes, high IDs, dense+sparse mixes, duplicate updates, deletes and zero-match cases are covered.
- Crash seams cover segment write/sync/rename, manifest publication, compaction and retirement; database crash tests cover durable index, acknowledgment and cleanup boundaries.
- The old experimental `sparse.bin`/`*.sparse` format fails closed with an explicit rebuild error.

## API and format changes

- Added `database.Options.CanonicalProjection` with `CanonicalProjectionUnset`, `CanonicalProjectionInline` and `CanonicalProjectionExternal`.
- Added `database.Options.ProjectFromRecord` and additive `ProjectInto`.
- Added `database.Options.ProjectionMigration`.
- Added `schema.NamespaceDef.ProjectionVersion`.
- Added `simhash.Store.ApplyUpdates`, `ApplyUpdatesAt` and `DeleteUpdates`.
- Added `index.Batch.Touch` for liveness publication without association changes.
- Added the static `index.FamilyCapabilitiesFor` contract.
- Journal v2 and canonical record v2 replace the unreleased experimental formats. Pre-V1 databases created by earlier branches must be recreated or explicitly rebuilt; unsupported versions fail closed.

## Performance

Measured on Apple M4 Pro, Go 1.27.0. Logical byte counters from `Stats()` are application-level and are not device writes or fsync counts.

### Encoding and commit paths

| Workload | Result | Allocations |
|---|---:|---:|
| Journal encode, allocated | 61.9 ns/op, 80 B/op | 1 |
| Journal encode, reused scratch | 51.7 ns/op, 0 B/op | 0 |
| Commit+index, batch 64 warm | 20.4 ms/op, 94.2 KiB/op | 510 |

### Logical size: two-association update, 100-byte record

| Format | Bytes |
|---|---:|
| Journal v1 | 223 |
| Journal v2 | 63 |
| Inline canonical total | 270 |
| External canonical total | 170 |

Tiny 9-byte projections do not benefit from the v2 envelope; realistic larger projections do.

### Sparse SimHash scale

| Live IDs | pHash search | PDQ scalar | PDQ bulk | Hot apply, batch 64 |
|---:|---:|---:|---:|---:|
| 10K | 14.7 µs | 35.9 µs | 36.1 µs | 411 µs |
| 100K | 165 µs | 357 µs | 356 µs | 1.34 ms |
| 1M | 1.51 ms | 3.43 ms | 3.41 ms | 5.19 ms |

Sparse hot-update cost is dominated by durable segment/manifest syscalls. The overlay is intentionally a bounded base+overlay scan rather than a general LSM or persistent sparse-ID lookup index.

## Validation

- `go build ./...`
- `go test -count=1 ./...`
- `go test -race -count=1 ./...`
- `go vet ./...`
- `sigmap validate` — 88% indexed-file coverage
- GitHub CI: all 12 Linux AMD64/ARM64 jobs pass
  https://github.com/evanoberholster/imageIdx/actions/runs/39852688190

Targeted coverage includes sparse publication/corruption/crash boundaries, scalar-vs-bulk PDQ, canonical mode mismatch/reopen, projection migration interruption, journal family and vector/bitmap round trips, randomized coalescing equivalence, and database restart/replay.

## Deferred

- Full photographic workload matrix including embedding-change updates, Pebble WAL/flush/compaction metrics, ingest p50/p95/p99 and statistical comparison.
- 10M sparse tier remains opt-in behind `IMAGEIDX_BENCH_LARGE`.
- Deeper sparse compaction/stale-history tuning; current work bounds segment count and avoids base copies on ordinary updates.
- Vector batch API remains benchmark/design-only; SimHash/vector coalescing remains disabled.
