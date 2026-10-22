# Phase 2 Follow-up Status Matrix

Baseline: `b7bb790`; follow-up head `51bcbd8` (PR #25)

| Item | Status | Evidence / work |
|---|---|---|
| P0.1 sparse search variants | Implemented | `SearchPHash`, `SearchPDQ`, `SearchPDQBulk`, `Get` and `Quality` use sparse base/overlay; scalar/bulk equivalence, prefixes, high IDs, dense overrides, tombstones and corruption tests. |
| P0.2 bounded sparse publication | Implemented | Immutable CRC-checked segment files, atomic manifest, bounded `sparseView` base+overlay, delta writes without full sparse COW, compaction and tombstone retention for dense suppression. |
| P0.3 replay-safe sparse persistence | Implemented | Manifest carries source generation; replay by generation is idempotent; failpoints cover segment/manifest/compaction boundaries; database crash tests cover index publication/ack/cleanup. |
| P0.4 durable canonical mode | Implemented | Schema-owned opaque config in `schema.dat` superblock; `CanonicalProjectionUnset/Inline/External`; mismatch and missing callback tests; `ProjectInto` additive callback. |
| P0.5 journal v2 correctness | Implemented | All bitmap kinds and vector element kinds round-trip; malformed family/kind/count/namespace rejected; random sequential-vs-coalesced family comparison; replay and crash paths. |
| P1.6 journal encoder reuse | Implemented | `appendMutation` scratch encoder used by `Commit`; benchmark shows 0 allocs versus one allocation. |
| P1.7 realistic workload matrix | Partial | `BenchmarkPhotoMetadataWorkloads` covers insert/rating/tags/GPS/AI/unchanged at 1/16/32/64 with logical bytes and throughput. Embedding-change and full p50/p95/p99 ingest matrix still need a dedicated follow-up. |
| P1.8 sparse scale/tuning | Partial/Measured | 10K/100K/1M pHash/PDQ scalar/bulk/hot-update benchmarks added; immutable segment count bounded at 64, base+overlay keeps ordinary updates from copying the base. Hot-update cost is still dominated by per-segment/manifest syscalls; full stale-history/10M tuning remains follow-up. |
| P1.9 projection migration tightening | Implemented | Version downgrade rejected; version-only PREPARED/ACTIVATE recovery fixed; external mode and projector required; affected namespace validation; interrupted prepare/activate tests. |
| P1.10 capability contract | Implemented | `index.FamilyCapabilitiesFor` static table; coalescing dispatch uses it; bitmap/BSI/time/geo/hash safe, SimHash/vector conservative/false; tests. |

## Explicit deferrals

- Full photographic workload matrix including embedding-change, Pebble WAL/flush/compaction metrics, ingest p50/p95/p99, and statistical comparison. The new matrix is representative and deterministic, but not the complete requested matrix.
- 10M sparse tier remains behind `IMAGEIDX_BENCH_LARGE`; 1M is measured in the scale benchmark.
- Sparse search uses a scanned base+overlay; no persistent sparse ID lookup index. This keeps the design specialized and avoids a universal LSM.
