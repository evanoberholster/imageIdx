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
