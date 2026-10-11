# PR #16 final optimization review

## Authoritative final candidate measurements

This section supersedes intermediate candidate numbers below. Final changes also
skip all-zero destination routing arrays for delete-only batches and reuse
already-computed mutation ID bounds. No persistent structure was added.

Ten independent interleaved processes per tree against `4c7847c`:

| Operation | Baseline → final | Benchstat |
|---|---:|---|
| Memory historical64 | 97.42 → 90.06 µs | −7.56%, p=.002 |
| Durable historical64 | 9.390 → 9.274 ms | no significant difference, p=.579 |
| Durable append64 | 8.095 → 8.039 ms | no significant difference, p=.105 |
| 10M sequential mutation1 | 13.83 → 11.53 ms | −16.61%, p<.001 |
| Reopen | 486.5 → 491.9 µs | no significant difference, p=.105 |
| Exact search | 16.30 → 16.28 ns | no significant difference, p=.781 |
| Day search | 41.87 → 42.03 ns | +0.36%, p=.041, below 5% threshold |

Supplemental final delete-only interleaving: 1M sequential prepare1 improves
15.70 → 11.50 µs (−26.72%, p<.001); random prepare1 has no significant regression.
All ordinary search cases retain zero allocations. Historical memory allocation
counts are unchanged; durable generation caching removes one allocation.

Final ten-sample random preparation medians (batches 1/8/32/64):
1M 2.792 / 3.004 / 1.807 / 1.958 ms; 10M 11.678 / 23.394 / 19.230 / 19.660 ms.
Final three-sample random durable medians: 1M 11.56 / 12.97 / 12.13 / 14.16 ms;
10M 23.44 / 35.43 / 31.40 / 32.12 ms. Rewrite/inspection/allocation counts
remain as documented below. Random detection remains O(records).

Final random historical64 shard-bound medians for 4/16/64/256/1024 KiB:
1M 11.85 / 11.91 / 12.95 / 14.60 / 14.92 ms;
10M 32.77 / 30.85 / 35.52 / 33.93 / 36.87 ms. Storage/rewrite tradeoffs remain
unchanged and support retaining the configurable 4 KiB default. Full repeated-time,
sequential-ID, wide-search, reopen and chronological-insert matrix is archived.

Raw benchmark outputs and benchstat comparisons are archived in
`evidence/pr16-final.tar.gz`; the archive includes intermediate runs showing the
reproduced regression, final accepted-candidate runs and CPU profiles. Final
validation of this exact source passed all commands listed below.

## Review scope and commits

Reviewed PR HEAD: `a41eada5788494cec11709072cde3bbcbd07326f`.
Validated implementation HEAD: `28cd5363d31f23118fb04ab25d21a6948bb2d610`.
The subsequent evidence/report commit changes documentation and artifacts only.
Performance baseline: `4c7847cbe4e96e9cfb91ac55c5c474c78bd3b63d`.
The final candidate consists of the source/test changes below on reviewed HEAD;
the implementation commit and validation outcome are recorded before handoff.
No unrelated `.github/copilot-instructions.md` or generated context changes
belong to this work.

## Implemented changes

- Cache the published generation's immutable address-sorted extent list. Reuse
  it as the previous-generation input instead of rebuilding/allocating it at
  every commit. Recovery initializes this cache from the selected root. Abort
  and uncertain publication never update it; successful publication updates it
  with the data/source watermark. No reader-visible scratch is reused.
- `storage.UnionExtents` merges two address-sorted inputs linearly while retaining
  the owning-result, duplicate/conflicting-size/empty-extent checks. Unsorted or
  other arities keep the general sort path. Differential/ownership/error tests
  cover both. TimeIdx sorts each new extent list once, reuses the previous sorted
  list, and still reconstructs/validates the allocator before root publication.
- Remove the directory-wide clearing pass: the shared fixed-width descriptor
  encoder overwrites all 56 bytes including padding. CRC, ID bounds and shard
  metadata for unchanged shards were already cached; no per-shard allocation
  or reflection is used in encoding.
- Add per-call rebuild failure injection, not global hooks. Exercise staged
  creation/sync/verification, both ownership locks, rename, destination/source
  directory sync, foreign namespace rejection and subprocess exits on both
  sides of rename. Preserve original files before rename; preserve uncertainty
  and original I/O causes after rename.
- Add a sustained growth/reuse stress test with four concurrent readers,
  sequential/random IDs, exact reference checks, dual-root validation, extent
  accounting, reopen after every growth event and mid-run/final reopen. The
  workload ramps to 2048 records then churns at bounded size for 128 cycles.
- Add isolated commit-component benchmarks and a ten-process alternating-order
  A/B script. Correct an untimed preparation-harness cardinality assertion
  placed inside, rather than after, its accumulation loop. Failed benchmark
  output is excluded; correctness is also covered by the independent scale test.

## Regression investigation

Apple M4 Pro, darwin/arm64, Go 1.27.0, GOMAXPROCS=12, same local filesystem and
fixtures, no competing test/benchmark runs. Every comparison below has ten
independent process measurements per tree, alternating A/B then B/A order.
Benchstat: `golang.org/x/perf` revision `8e83ce0f7b1c` (2026-04-09).

First compare `4c7847c` against unchanged reviewed HEAD `a41eada`:

| Previously reported regression | Baseline median | Reviewed HEAD median | p-value |
|---|---:|---:|---:|
| Memory historical64 | 96.38 µs | 94.83 µs | .912 |
| Durable historical64 | 9.276 ms | 9.531 ms | .075 |
| Durable append64 | 7.970 ms | 8.039 ms | .684 |
| 10M sequential mutation1 | 13.53 ms | 13.52 ms | .796 |

None reproduces as a statistically significant regression. The previous
short/sequentially ordered runs do not justify calling them confirmed CPU
regressions; no durability barrier or validation was removed to mask them.

Final one-pass-merge candidate versus `4c7847c`, same ten-process protocol,
with all original persistent benchmark probes preserved:

| Operation | Baseline | Final | Benchstat |
|---|---:|---:|---|
| Memory historical64 | 93.27 µs | 86.62 µs | −7.13%, p=.001 |
| Durable historical64 | 9.962 ms | 10.089 ms | no significant difference, p=.315 |
| Durable append64 | 8.184 ms | 8.123 ms | no significant difference, p=.631 |
| 10M sequential mutation1 | 19.49 ms | 18.38 ms | no significant difference, p=.052 |
| Reopen | 472.9 µs | 474.1 µs | no significant difference, p=.353 |
| Exact search | 16.17 ns | 16.20 ns | no significant difference, p=.753 |
| Month search | 854.2 ns | 851.6 ns | no significant difference, p=.469 |

All search cases remain 0 B/op / 0 allocs/op. Search algorithms did not change;
the exact-search difference is a measurement, not an attributed optimization.
10M mutation1 allocation drops 1.891 → 1.587 MiB and 12 → 11 allocs/op.
Durable append64 drops 21.41 → 18.28 KiB and 12 → 11 allocs/op.
Historical64 drops 103.23 → 96.22 KiB; memory allocations are unchanged.
Reopen adds only ~32 heap bytes (65.70 → 65.73 KiB), with unchanged 436
allocations, from the extra cached-list field in Index. No significant hot-path
latency regression above 5% remains in this A/B comparison. Day search shows
a small significant +1.34% drift (41.56 → 42.12 ns, p<.001), below the acceptance
threshold and still zero allocation; it is not hidden.

## Reproduction

Use a detached `4c7847c` worktree. Copy the candidate's
`scale_benchmark_test.go` and `commit_components_benchmark_test.go` into its TimeIdx package
so both use identical harnesses, then:

```sh
bash index/timeidx/benchmark_ab.sh BASELINE_TREE CANDIDATE_TREE OUTPUT_DIRECTORY
TIMEIDX_SCALE=1 GOMAXPROCS=12 go test ./index/timeidx -run '^$' \
  -bench '^BenchmarkCommitComponents$' -benchmem -benchtime=100ms -count=10
TIMEIDX_SCALE=1 GOMAXPROCS=12 go test ./index/timeidx -run '^$' \
  -bench '^BenchmarkScalePreparation$' -benchmem -benchtime=200ms -count=10
TIMEIDX_SCALE=1 GOMAXPROCS=12 go test ./index/timeidx -run '^$' \
  -bench '^(BenchmarkScaleShards|BenchmarkScaleDurableBatches|BenchmarkScaleChronologicalInsert)$' \
  -benchmem -benchtime=3x -count=3
TIMEIDX_SCALE=1 go test -race ./index/timeidx -run '^TestScaleMutationCorrectness$'
go test -race ./index/timeidx -run 'Rebuild|SustainedGrowth' -count=1 -v
```

Run each tree serially. Component/preparation runs have ten samples per case;
the expanded shard/storage matrix has three samples per case, not a percentile
latency guarantee. Source search/durable regression decisions use the independent
interleaved ten-sample comparisons, not that short matrix. No SLA or p95/p99
claim is made. Explicit batches near 64 remain preferred; queues/group commits
and deferred reclamation are explicitly deferred, not pre-merge requirements.

## Supplemental regression check and directory results

The non-interleaved expanded runs showed slower single-ID preparation, encoding
and several sequential durable cases. A further ten-process interleaved A/B
retested 1M/10M sequential/random prepare1, 1M/10M sequential durable1/8 and
10M encoding. None showed a significant regression over 5%. Encoding was
386.3 → 386.4 µs (p=.853); 1M random prepare1 2.773 → 2.767 ms (p=.579);
10M sequential durable1 19.50 → 18.40 ms (p=.089). Absolute sync costs changed
between runs, so only matched interleaved results support regression decisions.
Those checks resolved the single-ID/encoding anomalies. A subsequent clean
harness run reproduced memory historical64 +6.18% (100.6 → 106.8 µs, p=.015).
CPU profiles found hot work in `prepare`'s old-row/addition merge, including
repeated membership and exhaustion checks. Allocation counts were unchanged;
runtime allocation/GC/scheduling also consumed substantial sampled time.
The final candidate filters each old row once, emits all preceding additions,
then appends remaining additions. This simplifies the hot loop without changing
ordering, scratch ownership or replacement boundaries. Its final ten-process
result above is −7.13% versus baseline. Profiled single-run timings are not
used as statistical performance evidence.

Isolated phase measurements found 10M sorted extent union 1950.71 → 185.88 µs,
with the same owning allocation and validation. Caching previous extents removes
~76.81 µs, 319,488 bytes and one allocation from each corresponding commit.
Directory allocation/free is ~18.55 µs, encoding ~386 µs, CRC32C ~190 µs,
allocator reconstruction ~616 µs and one dirty-directory sync ~8 ms. The latter
is one barrier only; phases are not simply additive. No checksum or ID-bound
recalculation existed for unchanged shards, and encoding already writes directly
into the unpublished mmap extent. Remaining flat-directory bytes and sync are
real costs, not hidden by measuring only record-column rewrites.

## Random-ID scaling and shard decision

Final preparation, durable, storage and shard metrics are recorded in
`SCALE_BENCHMARKS.md`: ten preparation samples and three expanded-matrix samples
at 1M/10M, both ID distributions, both chronological/repeated-time datasets,
all five bounds and batches 1/8/32/64, including genuinely new-ID inserts.

At 10M, random prepare8/32/64 medians are 23.897 / 19.419 / 19.755 ms versus
136.541 / 235.388 / 289.631 ms at `4c7847c`. Random durable64 is ~34.44 ms,
~1.8K mutations/s in explicit 64-record batches on this machine. That is useful
batch throughput, not a QPS/p95/p99 or application SLA guarantee. Detection
still inspects approximately 10M IDs, with allocations tied to shard/batch count,
not per-row allocation. Only touched/receiving shards are rewritten.

**Retain the configurable 4 KiB default.** At 1M, random historical64 rewrites
478,232 bytes at 4 KiB versus 1,103,392 at 16 KiB with similar measured latency.
At 10M, 16 KiB has the better balance for the measured mix (1,596,024 versus
2,446,960 bytes and ~29.82 versus ~32.09 ms). Larger bounds improve wide reads
but amplify scattered-update payload writes. Both repeated-time and new-ID
chronological workloads were measured; no universal winner justifies changing
the small/selective-update default. Recommend evaluating 16 KiB explicitly for
large namespaces, not adaptive sizing. Full representative tables include
allocation, reopen, directory size and initial remap counts.

## Sustained mmap growth and extent accounting

`TestSustainedGrowthAndExtentReuse` passed normally and under race detection for
sequential and permuted full-width IDs. Start at 4096 bytes / 512-byte shards;
ramp for 32 cycles to 2048 rows, then perform 128 bounded cycles of insert64,
move64, delete64, reinsert64 and extra-record removal. Four concurrent search/
estimate/metadata readers run during commits. There are 768 source-watermarked
commits per distribution, four successful growth events, reopen after every
growth, mid-run reopen and final reopen.

Both distributions produced the same allocator geometry:

| Cycle | File | Live payload | Current extents incl. directory/padding | Retained-only | Free | Growth events | Reused addresses |
|---:|---:|---:|---:|---:|---:|---:|---:|
| 16 | 32,768 | 16,384 | 19,456 | 2,560 | 9,728 | 3 | 163 |
| 32 | 65,536 | 32,768 | 38,400 | 4,608 | 21,504 | 4 | 348 |
| 64 | 65,536 | 32,768 | 36,352 | 5,120 | 23,040 | 4 | 794 |
| 160 | 65,536 | 32,768 | 36,352 | 5,120 | 23,040 | 4 | 2,138 |

At steady state, 64 active shards / 3584 directory bytes; largest free run
16,896 bytes. Capacity accounting is exact: 36,352 current + 5120 retained-only
+ 23,040 free + 1024 superblocks = 65,536. Cumulative logical rewrite volume
3,701,736 bytes; cumulative allocated extent volume 3,512,832 bytes. These
are traffic counters, not permanently occupied bytes. A reuse event requires
an address to disappear from **both** roots then reappear; unchanged shared
payloads are not counted as reuse. The file plateaus throughout the bounded
phase; no obsolete-generation accumulation occurs.

After every growth and each reporting interval, compare exact timestamp/ID
arrays and searches to a sorted map-free reference; decode both valid roots,
check generation/source consistency, CRCs, uniqueness, shard order and bounds;
reconstruct the cross-generation union to detect overlap and compare exact
free-capacity/live/cache state. Verify no retired mappings remain. This tests
a deliberately small arena, not a claimed 100M-row endurance run.

## Rebuild crash/failure results

All deterministic tests passed, including under `-race`:

- Staged file creation, data synchronization and final-name creation failures.
- Staged verification and sync; final and staging ownership-lock failures.
- Rename failure, destination directory-sync failure and source directory-sync
  failure, plus a seam immediately after rename.
- Wrong-namespace staging, corrupt staging and active final/staging owner rejection.
- Subprocess exit code 83 immediately before rename: original reopens unchanged
  with ID 1 / source generation 7, and the staging replacement remains valid.
- Exit immediately after rename: the visible replacement reopens with namespace
  1, ID 2 / source generation 8. Process exit is not a power-cut simulation;
  pre-directory-sync publication remains durability-uncertain by contract.

Pre-rename I/O failures preserve the original and release both locks (a subsequent
successful publication proves release). Post-rename failures return
`ErrRecoveryRequired` joined with the original I/O cause, never success or
corruption. Verification and reopen validate whichever file is visible; TimeIdx
does not delete the original before a ready replacement or replace active owners.
Membership remains startup-only; no database scanning was added.

## Shared storage and final validation

Passed on the final source:

```text
go build ./...
go test ./...
go test -race ./...
go vet ./...
go test ./index/geoidx/... ./index/timeidx/... ./internal/storage/... ./internal/pagealloc/... -count=1
go test -race ./index/geoidx/... ./index/timeidx/... ./internal/storage/... -count=1
TIMEIDX_SCALE=1 go test -race ./index/timeidx -run '^TestScaleMutationCorrectness$' -count=1
go test ./index/timeidx -run 'SustainedGrowth|Rebuild' -count=1 -v
sigmap validate
git diff --check (changed task files)
```

GeoIdx's full fresh suite and race suite passed after the shared union fast path.
Existing CRC32C/descriptor-layout, allocation/union/reconstruction, crash/recovery,
growth and reader-lifetime tests remain passing. The added merge tests compare
against the general sort path, verify no input/result aliasing and preserve
invalid-size/conflict rejection. No new shared abstraction or GeoIdx API change.
Map-syntax audit: no Go maps in TimeIdx, storage or pagealloc. Ordinary searches
remain 0 B/op / 0 allocs/op with reusable destination capacity. Namespace lookup,
source-generation atomicity, corruption/staleness classification and lifecycle
protocol are unchanged; no new data format was introduced beyond format 5.

## Known limitations and merge recommendation

- Arbitrary-ID mutation detection remains O(records); directory publication is
  O(shards) and rewrites the whole flat directory. No promise of constant-time
  random-ID updates or zero directory write amplification.
- Reopen retains O(records) duplicate-validation scratch and CPU work; large
  random-ID fixtures take ~1.36 seconds at 10M. There is no reverse-ID index.
- Sparse file capacity/free slack is not physical filesystem I/O. The sustained
  test is finite; other shard/workload mixes may need different capacity, but
  ownership is bounded to two recoverable roots plus actively pinned readers.
- Source completeness is application-supplied; the application coordinates DB
  snapshot/replay. CRC does not establish synchronization by itself.
- Subprocess termination verifies software recovery, not storage-device power
  loss. Post-rename directory-sync uncertainty is explicitly preserved.
- No application SLA was specified. The report provides measured batch throughput
  and scaling, not blanket suitability for low-latency 100M-row arbitrary-ID writes.
- Reverse directories, queues, workers, adaptive sharding, SIMD and deferred
  reclamation are explicitly deferred; no mandatory measurement requires them.

**Recommendation: merge-ready within the documented workload limits.**
Exact-source build, full tests, full race tests, vet, fresh GeoIdx/shared-storage
suites, 1M/10M race correctness, rebuild/growth tests and SigMap validation all
passed. The opening authoritative tables supersede intermediate measurements.
The reproduced historical regression was profiled and fixed, not dismissed.
PR #16 remains unmerged for final human approval.

The performance gate is verified. Other verified gates include directory
optimization; sustained growth and generation-safe reuse; bounded-size plateau;
rebuild failures/crashes; random-ID and shard-size evidence; fresh GeoIdx/TimeIdx
and race suites; allocation/map/registry/source-generation invariants; reproducible
report and no outstanding P0 correctness issue.

The following historical notes are retained for provenance; their old optional
outstanding-work list is not the current acceptance checklist.

---

## Historical alignment review (before source-generation work)

The subsequent source-watermark/lifecycle work uses reviewed HEAD `4c7847c` as
its benchmark baseline. See `SOURCE_AND_LIFECYCLE.md` for format 5 and its
exported-method audit, and `SCALE_BENCHMARKS.md` for the large-scale evidence.
The earlier outstanding-work list below records the previous review, not a
claim that optional queues, relaxed durability or runtime replacement should
be added. None has been introduced.

Baseline: `bd0752f5c170a5b5aaf97efa791c54f5bfbdb59e`. PR #16 must remain
unmerged until the expanded performance/recovery review is complete.

## Current changes

- `InitializeStore` validates the complete namespace configuration before
  initializing files and publishing an immutable sorted registry. Failures are
  returned per namespace; a failed member stays configured but unavailable.
- The compatibility startup builder requires `SealStartup` before `Namespace`
  lookup. Registration/removal after publication returns `ErrNamespaceFrozen`.
  `Namespace` never acquires a mutex or allocates. `OpenNamespace` remains a
  lifecycle convenience, not the hot lookup API.
- Shutdown atomically disables operations through every owned index before
  waiting for per-index readers. The published registry is never mutated.
- Corrupt files/identity mismatches return `ErrRebuildRequired` wrapping the
  integrity cause. I/O/configuration/unsupported-format errors are not rebuild
  signals. Normal older-generation recovery is unchanged.
- `PublishRebuild` is an offline startup-only contract: validate/sync a separate
  completed file, exclude active final/staging owners using existing locks,
  rename on the same filesystem, then synchronize both directories. Failures
  before rename preserve the original. Sync failures after rename are uncertain
  and require inspection/reopen. The application owns DB snapshot/replay.
- Both indexes now share the identical 56-byte physical directory codec;
  domain validation and checksums remain local. No disk-format change.
- Large mutation batches reuse writer-private scratch. Small batches still use
  the fixed 64-operation stack buffer. Fully covered search shards skip bounds
  searches. These changes still need the final performance gate.
- No maps remain anywhere in TimeIdx, including differential reference tests.

## Shared-code decisions

| Component | GeoIdx location | TimeIdx location | Decision |
|---|---|---|---|
| mmap lifecycle | `persist.go`, shard regions | `storage.go` / `index.go` | Keep local orchestration: GeoIdx retains per-shard region ownership; TimeIdx rebinds unchanged slices on growth. A common retire owner would hide different lifetime rules. Both already share `MappedFile`. |
| COW commit | `persist.go` | `storage.go` | Already share `CommitSuperblock` and all three outcomes; payload/publication stays local because deferred vs synchronous semantics differ. |
| Extent union | shared storage | shared storage | Already share sorted owned extent unions; ordered-input merge/scratch optimization still to evaluate. |
| Allocator recovery | shared storage | shared storage | Already share allocator reconstruction/pagealloc; repeated recovery reconstruction still to profile. |
| Shard directory encoding | `persist.go` | `storage.go` | Extracted exact 56-byte codec into `internal/storage/shard_descriptor.go`; bounds/domain checks stay local. |
| SoA allocation | typed S2 columns | `shard.go` | Keep specialized contiguous TimeIdx helper; sharing distinct S2 key backing storage would need unsafe conversion or duplicate allocations. Both share typed mmap `View`. |
| Reader tracking | Index tracker | Index readers | Already share `ReaderTracker`; reclamation orchestration remains local. |
| Splitting/merging | `shard.go` | `shard.go` | Keep specialized; GeoIdx min/target/max and layered compaction differ from TimeIdx's single bound. |
| Search | `search.go` | `search.go` | Keep separate geographic vs chronological semantics. |
| Namespace registry | N/A | `store.go` | Sorted immutable published slice, startup builder only. |
| Corruption classification | `persist.go` | `storage.go` | Keep domain errors local; common parsing primitives remain shared. Explicit TimeIdx rebuild signaling must not classify I/O as corruption. |

## Decisions retained

Synchronous durability remains the only mode. Format 5 now persists the
application-supplied source completeness watermark; checksums alone cannot
detect a valid but stale index. No relaxed durability mode has been introduced.
Reclamation
still waits for pinned readers; no unbounded retirement list was introduced.
The shard default remains 4 KiB pending the expanded workload matrix.

## Outstanding acceptance work

- Full random-ID/clustered/sparse/repeated-time shard matrix with reopen,
  fragmentation, directory/growth and rewrite-volume counters.
- Bounded group-commit prototype vs synchronous and explicit larger batches,
  including throughput and p95/p99 latency; no queue has been added yet.
- Deferred-reclamation stress/evidence and bounded-memory evaluation.
- Monotonic mutation routing evaluation and final reopened-index profiling.
- Rebuild crash/directory-sync failure injection beyond the current successful,
  corrupt-staging and active-owner exclusion tests.
- Final performance comparison/review and updated PR description.

The initial baseline matrix is saved as `timeidx-final-before-bd0752f.txt` in
the session's approved temporary directory. Initial after measurements are
`timeidx-final-after.txt`; neither constitutes the complete acceptance matrix.
