# Phase 2 Implementation Report

**Base:** `main` after PR #24 (`0e37a4b`)
**Branch:** `phase2` (`b7bb790`)
**PR:** https://github.com/evanoberholster/imageIdx/pull/25
**Environment:** Go 1.27.0, darwin/arm64, Apple M4 Pro

## Implemented

- Journal v2: typed, CRC32-protected delta envelope owned by `database`.
  - Explicit insert/no-op/delete flags.
  - Before set = associations removed; after set = associations added.
  - Typed encoding for time, geo, SimHash, exact hash, vector and bitmap
    associations.
  - Old journal versions fail closed with `ErrUnsupportedJournalVersion`.
- Canonical value v2: magic-tagged, versioned header with inline (A) or external
  (B) projection storage.
  - External mode uses `Options.ProjectFromRecord` and is exercised through
    commit, reopen and rebuild.
- Projection-semantic versioning:
  - `schema.NamespaceDef.ProjectionVersion` in definitions-blob v3.
  - v1/v2 definitions read with projection version 1.
  - `Options.ProjectionMigration` + `ProjectFromRecord` allows a version-only
    restart migration through the existing PREPARED/ACTIVATE rebuild path.
- Sparse SimHash external IDs:
  - Dense contiguous IDs retain the existing append path.
  - Non-dense/high IDs use a durable CRC-protected sparse overlay.
  - Sparse searches merge with dense results, filter overridden IDs and preserve
    ascending ImageID order.
  - Rebuild preserves sparse external IDs.
- Coordinator coalescing continues over typed journal deltas; SimHash, vector
  and delete→reinsert remain on ordered fallback.
- Added an explicit index liveness touch for coalesced update→revert groups,
  so a revert after a tombstone cannot leave the entity dead.

## Size comparison

Synthetic update with two bitmap associations, 100-byte record:

| Application projection | v1 journal | v2 journal | A inline total | B external total |
|---:|---:|---:|---:|---:|
| 9 | 41 | 63 | 179 | 170 |
| 100 | 223 | 63 | 270 | 170 |
| 1000 | 2023 | 63 | 1170 | 170 |

For tiny projections the v2 envelope is larger; for realistic larger
projections it materially reduces journal bytes, and external canonical storage
reduces total logical bytes.

## Benchmarks

Measured on the same Apple M4 Pro, Go 1.27.0, with 150–200 ms runs:

| Workload | Result | Allocations |
|---|---:|---:|
| KV namespace bitmap lookup | 0.25–0.26 ns/op | 0 |
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

The Phase 1 baseline remains the comparison point for unchanged paths. Physical
device writes were not measured; `Stats()` counters are application-level and
logical.

## Validation

- `go build ./...` passed.
- `go test -count=1 ./...` passed (all packages).
- `go test -race -count=1 ./...` passed (all packages).
- `go vet ./...` passed.
- `sigmap validate` passed; coverage 87% (361/415 indexed files).
- targeted crash/recovery, golden fixture, journal corruption, projection
  migration, sparse SimHash, and coalescing tests.

## Remaining limitations

- Sparse SimHash uses a scanned sorted overlay; no separate persistent lookup
  index.
- SimHash/vector coalescing remains disabled.
- Vector batch API remains design/benchmark-only.
- External canonical projection mode must be used consistently after a
  projection migration; the mode is not persisted in the database metadata.
- Pre-V1 databases from earlier development branches must be recreated or
  explicitly rebuilt.
