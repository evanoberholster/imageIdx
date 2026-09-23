# Search execution benchmarks

Measured on Apple M4 Pro (`darwin/arm64`) with Go's benchmark harness. The
in-memory fixture has 100,000 IDs present in two compacted enum fields. Each
operation uses a reused workspace and destination slice. The persistent fixture
has the same memberships, written with OS durability and checkpointed before
measurement. Results are the range of three isolated runs at 250 ms per run.

```sh
go test ./search -run '^$' -bench 'Benchmark(Execution|PersistentExecution|PlanRefresh|CombinedSearch|ExecutorQuery)$' -benchtime=250ms -count=3
```

| Operation | Before | After | After allocations |
|---|---:|---:|---:|
| In-memory combined search | 14.3–17.1 µs | 13.8 µs | 0 |
| In-memory count, 100k matches | 254–262 µs | 776–781 ns | 0 |
| In-memory first match (before `SearchLimit(1)`, after `AppendPage`) | 221–223 µs | 73.4–73.5 ns | 0 |
| In-memory filter of 100 ranked entries | 221–223 µs | 760–761 ns | 0 |

The baseline used the prior search implementation and the same fixture. Its
limited search and ranked filtering built the full candidate set before taking
the requested subset. These numbers compare the specific fixture and operations,
not all query shapes or end-to-end application throughput.

| Persistent operation | Time | Allocation |
|---|---:|---:|
| Count, 100k matches | 3.09–3.38 µs | ~36.9 KB, 2 allocs |
| First page (`Limit: 1`) | 1.21–1.22 µs | ~18.4 KB, 1 alloc |
| Filter 100 candidate IDs | 432–434 ns | 0 |
| Rank by time, best 10 of 100k | 1.49–1.50 ms | ~18.4 KB, 1 alloc |

Persistent bitmap sources detach from mmap ownership, so count, paging and
ranking still copy the selected source bitmap. Candidate filtering probes
postings under a short read guard and avoids that copy. Timestamp sources and
lossy timestamp filters can buffer IDs because `timeidx` has no ID-ordered
cursor. Ranking still scores every verified match even when it retains only
the best `k` results.

`BenchmarkPlanRefresh` was 90.7–94.4 ns and 448 B per call. The fixed-plan
`BenchmarkExecutorQuery` was 36.7–36.9 µs and 0 allocs per operation. Refresh
only after enough writes to change stage selectivity; it is not needed on every
query. Concurrent mutation, many range keys, broad geo cells, and cold mmap
pages are not represented by these timings.

## Opt-in search metrics

`BenchmarkSearchMetrics` runs the same two-enum-field plan through `Search`
and `SearchWithMetrics`, reusing the workspace, result buffer and metrics
report. Apple M4 Pro, five isolated 250 ms runs, `darwin/arm64`:

| Matches | `Search` | `SearchWithMetrics` | Extra time | Allocations |
|---:|---:|---:|---:|---:|
| 100 | 806–815 ns | 939–971 ns | ~130–160 ns | 0 in both |
| 100,000 | 221–224 µs | 222–227 µs | ~1–3% | 0 in both |

```sh
go test ./search -run '^$' -bench '^BenchmarkSearchMetrics$' -benchtime=250ms -count=5
```

For small results, clock reads and stage bookkeeping are a visible fraction
of total work; the plain `Search` path performs no metrics clock reads.
