## Follow-up hardening for PR #25

This update hardens the Phase 2 formats before merge. It is still pre-V1; legacy experimental sparse formats fail closed and must be rebuilt.

## P0 completed

- **Sparse SimHash search parity**: `SearchPHash`, `SearchPDQ`, `SearchPDQBulk`, `Get` and `Quality` now share sparse override/tombstone behavior. Scalar and bulk PDQ results, prefixes, high IDs, mixed dense/sparse, deletes and zero matches are tested.
- **Bounded sparse publication**: replaced full-slice COW with immutable CRC-checked `sparse-<generation>.seg` files, an atomically replaced `sparse.manifest`, bounded base+overlay reader views, single-pass merge compaction and dense-suppressing tombstones.
- **Replay-safe persistence**: the sparse manifest carries the source generation; replaying an already-published interval is a no-op. Crash seams cover segment write/sync/rename, manifest write/publish, compaction and retirement; database crash tests cover index publication, acknowledgment and cleanup.
- **Durable canonical mode**: mode is stored in schema-owned config bytes in `schema.dat`, enforced on every open, with explicit mismatch/missing-callback failures. `ProjectInto(dst, record)` is additive to `ProjectFromRecord`. Mode changes are rejected; projection-version changes require external mode plus a projector.
- **Journal v2 correctness**: every vector kind and bitmap kind round-trips; malformed family/kind/namespace/count/length fails closed; destination-oriented `appendMutation` reuses commit scratch; randomized sequential-vs-coalesced family coverage added.
- **Migration hardening**: version downgrades rejected; same-count projection-version PREPARED recovery fixed; interrupted prepare/activate covered.
- **Capability contract**: static per-family coalescing/delete/replay/batch guarantees used by coalescing dispatch.

## Benchmarks

Journal encoder reuse: 51.7 ns/op, 0 B/op, 0 allocs vs 61.9 ns/op, 80 B/op, 1 alloc.

Photographic logical-write matrix and sparse 10K/100K/1M scale results are in the local follow-up report and benchmark files. Metrics labelled `journal-B/op` and `store-B/op` are logical application counters, not physical device writes.

## Validation

- `go build ./...`
- `go test -count=1 ./...`
- `go test -race -count=1 ./...`
- `go vet ./...`
- `sigmap validate`
- Targeted sparse crash/corruption, canonical mode, projection migration, journal family/vector/bitmap and random coalescing tests.

## Explicit deferrals

- Full photographic matrix including embedding-change workload, Pebble WAL/flush/compaction metrics and ingest p50/p95/p99/statistical comparison.
- 10M sparse tier remains opt-in via `IMAGEIDX_BENCH_LARGE`.
- Sparse search uses a scanned base+overlay; no persistent sparse ID lookup index.


---

## Summary

Phase 2 replaces the full-projection journal with a typed, CRC-protected delta envelope, adds explicit canonical projection storage modes, introduces per-namespace projection versions with a restart-only rebuild migration, and removes SimHash's dense `Len()+1` external-ID constraint.

- Journal v2 records only removed and added typed associations, with explicit insert, no-op and delete flags.
- Canonical values use a magic-tagged versioned header. Inline mode (A) retains projection bytes; external mode (B) stores only the application record and uses `ProjectFromRecord`.
- `schema.NamespaceDef` carries a `ProjectionVersion` in definitions-blob v3. v1/v2 definitions read as version 1.
- `Options.ProjectionMigration` permits an explicit version-only migration through the existing PREPARED/ACTIVATE rebuild path.
- SimHash keeps dense appends for contiguous IDs and adds a durable sparse overlay for gaps, high IDs and out-of-order upserts. Sparse searches merge with dense results and preserve ascending ImageID order.
- Coalescing continues over typed deltas; SimHash, vector and delete→reinsert remain on ordered fallback.
- Coalesced update→revert groups use `index.Batch.Touch` to preserve liveness after a tombstone without changing associations.

## API and format changes

- Added `database.Options.CanonicalProjection` (`CanonicalProjectionInline` / `CanonicalProjectionExternal`).
- Added `database.Options.ProjectFromRecord`.
- Added `database.Options.ProjectionMigration`.
- Added `schema.NamespaceDef.ProjectionVersion`.
- Added `simhash.Store.ApplyUpdates` for arbitrary nonzero external IDs.
- Added `index.Batch.Touch` for explicit liveness publication without association changes.
- Journal v2 and canonical record v2 replace the unreleased development formats; older development databases must be recreated or explicitly rebuilt. Unsupported versions fail closed.
- Dense `simhash.AppendBatch`/`UpsertBatch` are rejected after the sparse overlay is active to prevent two sources of truth.

## Performance

Measured on Apple M4 Pro, Go 1.27.0:

| Workload | Result | Allocations |
|---|---:|---:|
| Projection diff, 1 association | 45.8 ns/op | 0 |
| Projection diff, 16 associations | 548 ns/op | 0 |
| Projection diff, 256 associations | 31.2 µs/op | 0 |
| Commit+index, batch 16 (cold) | 20.6 ms/op, 36.9 KiB | 210 |
| Commit+index, batch 64 (cold) | 20.0 ms/op, 101.6 KiB | 509 |
| Commit+index, batch 16 (warm) | 20.5 ms/op, 28.7 KiB | 210 |
| Commit+index, batch 64 (warm) | 20.4 ms/op, 94.2 KiB | 510 |
| Canonical inline, 16 updates | 1152 store-value B/op | 210 |
| Canonical external, 16 updates | 1008 store-value B/op | 212 |
| Sparse SimHash apply, batch 64 | 4.8 µs/op, 7.6 KiB | 3 |
| Sparse SimHash search, 10k records | 7.3 µs/op, 16 B | 1 |
| Sparse SimHash reopen, 10k records | 4.5 ms/op | 112 |

Journal/combined logical size for a two-association update with a 100-byte record: v1 journal 223 bytes, v2 journal 63 bytes; inline total 270 bytes, external total 170 bytes. Tiny 9-byte projections do not benefit because the v2 envelope is larger; realistic projections do. Physical device writes were not measured; `Stats()` remains application-level and logical.

## Validation

- `go build ./...`
- `go test -count=1 ./...`
- `go test -race -count=1 ./...`
- `go vet ./...`
- `sigmap validate`
- Journal v2 golden/corruption tests, canonical A/B reopen/rebuild tests, projection-version migration tests, sparse SimHash reopen/rebuild tests, coalescing regression/crash tests.

## Remaining limitations

- Sparse SimHash uses a sorted scanned overlay; no separate persistent ID lookup index.
- SimHash/vector coalescing remains disabled.
- Vector batch API remains design/benchmark-only.
- External canonical mode must be used consistently after projection migration; the mode is not persisted in database metadata.
- Pre-V1 databases from earlier development branches must be recreated or explicitly rebuilt.
