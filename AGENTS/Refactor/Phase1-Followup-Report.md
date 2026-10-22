# Phase 1 Follow-up Report

**Base:** PR #24 head `4470096` on `refactor-index-design`
**Follow-up:** working tree (to be committed on the same branch)
**Environment:** Go 1.27.0, darwin/arm64, Apple M4 Pro

## Correctness work

- Added real journal replay/reopen coverage for insert→update coalescing:
  image `42` is inserted with camera `Canon` and rating `3`, image `99` is
  interleaved, then image `42` is updated to camera `Canon` and rating `4`.
  The test runs through both `Decoder` and `DecodeInto`, checks both indexes and
  liveness, closes/reopens twice, and verifies the durable generation.
- Added sequence coverage for add→delete, update→delete, update→revert,
  delete→reinsert fallback, empty projections, and no-op updates.
- Extended the randomized sequential-vs-coalesced bitmap oracle to 64 journal
  records, including liveness and final generation.
- Added crash-injection coverage for coalesced intervals at index intent,
  durable index, acknowledgment and cleanup boundaries.
- Added workspace payload boundary tests at 0, 1, 65535, 65536 and 65537 bytes,
  mismatched-payload rejection, ordinary/oversized reuse, and oscillating
  allocation coverage.
- Added namespace bitmap tests for 0/63/64/65535/65536-region membership,
  reserved IDs, unknown namespaces, legacy open, reopen, and non-KV rejection.
- Added projection input-mutation, duplicate set-semantics, size-matrix and
  unchanged/one-changed/all-changed tests.

## Performance change

A small-diff fast path now handles at most two total associations before the
sort/merge path. Inputs remain unsorted; duplicates are emitted with set
semantics.

Projection diff, one changed association:

| Associations | Before follow-up | After follow-up | Allocations |
|---:|---:|---:|---:|
| 1 | 121 ns | 45.7 ns | 0 |
| 4 | 216 ns | 146.6 ns | 0 |
| 16 | 633 ns | 550.3 ns | 0 |
| 64 | 6.22 µs | 6.02 µs | 0 |
| 256 | 31.8 µs | 30.8 µs | 0 |

Cold vs warmed commit+index (batch 1/16/64):

| Batch | Cold B/op | Warm B/op | Cold allocs | Warm allocs |
|---:|---:|---:|---:|---:|
| 1 | 16.6 KiB | 9.7 KiB | 98 | 99 |
| 16 | 38.3 KiB | 30.3 KiB | 229 | 229 |
| 64 | 109.4 KiB | 99.4 KiB | 570 | 577 |

The remaining cold-start difference is the one-time coordinator workspace and
first projection block. `Stats()` is documented as application-level counters
and logical byte estimates, not device writes or `fsync` counts.

## Verification

- `go build ./...`
- `go test -count=1 ./...`
- `go test -race -count=1 ./...`
- `go vet ./...`
- `sigmap validate` (passes; reports a 56% context-coverage warning)
- Escape analysis: `go test -run '^$' -gcflags='-m=2' ./database ./store ./index ./schema`
  shows heap escapes only for error/panic formatting and scratch-slice growth;
  warmed projection and workspace paths do not allocate per entry.

## Remaining limitations

SimHash, vector and delete→reinsert coalescing remain on ordered sequential
fallback. No async publication, journal v2, schema projection migration or
sparse SimHash work was started.
