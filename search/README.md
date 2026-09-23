# search

Compile conjunctive predicates once, then select an explicit execution operation.
Plans are immutable and safe to share. Workspaces and destination buffers are
exclusively owned by one execution.

```go
plan, err := search.Compile(
    search.AllValues(tags, bitmapidx.StringValue("favorite")),
    search.During(times, start, end),
    search.InCells(locations, cellIDs...),
)
if err != nil { return err }

var ws search.Workspace
ids, err := plan.AppendIDs(ctx, dst[:0], &ws)
count, err := plan.Count(ctx, &ws)
found, err := plan.Exists(ctx, &ws)
kept, err := plan.FilterIDs(ctx, dst[:0], candidates, &ws)
page, err := plan.AppendPage(ctx, dst[:0], &ws,
    search.Page{AfterID: lastID, Limit: 100})
```

## Contracts and costs

| Operation | Contract |
|---|---|
| `Search` / `SearchWithMetrics` | Same bulk result as `AppendIDs`; metrics are opt-in. |
| `AppendIDs` | Append sorted, unique IDs, preserving the destination prefix. |
| `AppendPage` | Positive limit, exclusive `AfterID`; stop after verified matches. |
| `FilterIDs` | Preserve input order and duplicates; `dst=candidates[:0]` is supported. |
| `Count` | Bitmap cardinality kernels where possible; otherwise verified batches. |
| `Exists` | Stop on the first verified match. |
| `Visit` | Sorted IDs with error reporting and early callback stop. |
| `RankTime` | Top-k by exact `uint64` timestamp gap. |
| `RankGeo` | Top-k by `float64` haversine metres, including fractional metres. |
| `FilterRanked` | Probe supplied entries, preserving order, duplicates and scores. |

## Per-run metrics

`Search` keeps the uninstrumented bulk path. Use `SearchWithMetrics` when a
request needs counts and stage timings; reuse the report sequentially to avoid
allocating stage records on every run:

```go
metrics := search.NewSearchMetrics(plan)
ids, err := plan.SearchWithMetrics(ctx, dst[:0], &ws, metrics)
if err != nil { return err }
for _, stage := range metrics.Stages {
    log.Printf("stage=%d kind=%s input=%d passed=%d duration=%s",
        stage.PlanIndex, stage.Stage.Kind, stage.Input,
        stage.Passed, stage.Duration)
}
log.Printf("matched=%d total=%s", metrics.Matched, metrics.TotalDuration)
```

`Executor.SearchWithMetrics` uses pooled scratch; the report remains caller-owned.
`FilterIDsWithMetrics`, `RankTimeWithMetrics`, and `RankGeoWithMetrics` expose
the same stage records. Ranking also reports candidate, scored, and skipped
counts plus scoring/heap and final-sort durations in `metrics.Rank`. `Stages`
is in plan order, while `ExecutionOrder` shows actual execution order; `-1`
means a stage was not reached. Source `Collected` is its full population and
`Passed` is the number visited. Filter `Input`/`Passed` count candidate entries,
including duplicates. A buffered timestamp filter's `Collected` is the size
of its prepared lookup set; its `Calls` includes preparation and batch probes.
`Matched` is before top-k ranking, while `Returned` is the appended result
count. On cancellation or error, `Complete` is false and observed counts may
be partial even though the returned destination has its original length.
Metrics reflect live per-stage views, not a store-wide snapshot.

Bitmap sources seek by ID and do not expand the entire result into an ID slice
for pagination, existence or ranking. Their bitmaps may still be materialized or
copied: persistent results must detach from mmap. Timestamp sources are buffered
and sorted because timeidx has no ID-ordered cursor. Lossy timestamp filters
without an exact lookup also buffer a range. Numeric ranges may choose a union
when that is cheaper than probing many keys.

Cancellation is checked between candidate batches and stages, and during
scoring/visiting. A single index materialization or bitmap kernel is not
interruptible. Destination-returning operations return the original
slice length on error, but appended backing storage may have changed. A visitor
may already have delivered matches.

Stages and batches read independently published views. A query is **not a
database-wide snapshot**; writes between pages can alter subsequent pages.
`AfterID` is an ID-order cursor, not a ranked-score cursor. Keep indexes open
during execution. Callbacks must not reuse the active workspace.

## Predicates and planning

`AllValues` requires every supplied value; `FieldText` requires every analyzed
token. `FieldRange` uses stored-grid integer bounds. `During` uses `[start,end)`;
add `ExactTimestamp` to verify lossy candidates against canonical timestamps.
`InCells` takes S2 cell IDs, expanding coarse cells and snapping finer cells.
These constructors defer validation to `Compile`. Empty value lists are errors;
empty ranges match nothing. Compile validates all clauses even when another
clause is empty. There are no OR/NOT combinators.

`WithACL(userID, groupIDs...)` is the one built-in OR clause: it unions the
direct `UserACL` bitmap with the supplied `GroupACL` bitmaps, then intersects
that union with the rest of the query. Bind the two dedicated fields through
the bitmap store when compiling:

```go
plan, err := search.CompileWithStore(store,
    search.WithACL(userID, authenticatedGroupIDs...),
    search.AllValues(tags, bitmapidx.StringValue("favorite")),
)
```

The caller must authenticate group membership; `WithACL` does not check it.
`Compile` without a store rejects this clause. User ID zero, group ID zero,
and missing required ACL fields are errors. A user-only ACL needs only the
`UserACL` field. This clause does not automatically include public images.

`IsEmpty` means structural emptiness; use `Exists` for live data. Estimates are
cached planning hints sampled at Compile or Refresh, never correctness
conditions. After a workload changes, explicitly refresh without mutating an
executing plan:

```go
updated, err := plan.Refresh(ctx) // reuses canonicalized predicates
stages := updated.Stages()       // field, predicate, estimate and capabilities
```

Do not refresh on every request. Publish a replacement plan safely if other
goroutines share the containing variable.

## Shared execution and retained memory

```go
executor, err := search.NewExecutor(search.ExecutorConfig{
    WorkspaceCapacity: 4096,
    MaxRetainedBytes: 8 << 20,
})
ids, err := executor.AppendIDs(ctx, dst[:0], plan)
count, err := executor.Count(ctx, plan)
```

Executor is independent of plans and index lifetimes. It has no writer-batch
pool. The retention budget applies to **each idle workspace**, not active-query
or process memory. Oversized scratch is dropped after use; `sync.Pool` can also
discard buffers at GC. Explicit owners can use `Workspace.Trim` and
`RetainedBytes`. `Reset` alone retains capacity. Allocation behavior depends on
the operation, source representation, pending deltas and buffer sizing.

## Ranking

Scores no longer saturate at 65,535. `Result` is `RankedResult[uint64]` for time
and integer-ranked input; `GeoResult` is `RankedResult[float64]`. Geo ranking
methods accept/return `[]GeoResult`. Equal native scores break ties by ID.
Ranked `k < 0` means all matches, `k == 0` means no output. Invalid origins and
nil ranking callbacks return errors.

See [PERFORMANCE.md](PERFORMANCE.md) for benchmark methodology and results.
